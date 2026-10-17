package alert

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oekit/concurrency/retry"

	"github.com/zy84338719/lightcat/internal/store"
)

// fastRetry 把退避缩到毫秒级：生产参数是 2s 基数 + 2 次重试，一个用例就要等 6 秒，
// 而这里要验的是**分类与次数**，不是退避有多长。
func fastRetry(attempts int) retry.Config {
	return retry.Config{
		Attempts:    attempts,
		Delay:       time.Millisecond,
		MaxDelay:    5 * time.Millisecond,
		Exponential: true,
		Retryable:   retryablePushErr,
	}
}

func newNotifyManager(t *testing.T, st *store.Store, url string, cfg retry.Config) *Manager {
	t.Helper()
	m, err := New(st, nullLogger(), url, Thresholds{Offline: true}, time.Hour)
	if err != nil {
		t.Fatalf("构造 Manager 失败: %v", err)
	}
	m.pushCfg = cfg
	m.budget = 2 * time.Second
	return m
}

func fireAlert(t *testing.T, st *store.Store, devID int64) store.Alert {
	t.Helper()
	m := &Manager{st: st, log: nullLogger(), th: Thresholds{Offline: true}, cooldown: time.Hour}
	m.DeviceOffline(devID)
	list, err := st.ListAlerts(10)
	if err != nil || len(list) == 0 {
		t.Fatalf("取告警列表失败: %v", err)
	}
	return list[0]
}

// TestNotifyRetriesOn5xx 5xx 属于「接收端自己坏了」，值得再来一次：
// 断言真的按配置重试到次数上限，而不是一试就放弃。
func TestNotifyRetriesOn5xx(t *testing.T) {
	st := newTestStore(t)
	devID := testDevice(t, st, "RETRY5XX")

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	m := newNotifyManager(t, st, srv.URL, fastRetry(3))
	a := fireAlert(t, st, devID)
	m.notify(context.Background(), a)

	if got := hits.Load(); got != 3 {
		t.Errorf("5xx 时请求次数 = %d，期望 3（首次 + 2 次重试）", got)
	}
	list, _ := st.ListAlerts(10)
	if list[0].Notified {
		t.Error("最终仍失败，不该标记已推送")
	}
}

// TestNotifyNoRetryOnClientError 400 是接收端**明确拒收**：重试它毫无意义，
// 反而白占退避窗口、并让对方再拒一次。
func TestNotifyNoRetryOnClientError(t *testing.T) {
	st := newTestStore(t)
	devID := testDevice(t, st, "NO400")

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "nope", http.StatusBadRequest)
	}))
	defer srv.Close()

	m := newNotifyManager(t, st, srv.URL, fastRetry(3))
	a := fireAlert(t, st, devID)

	start := time.Now()
	m.notify(context.Background(), a)
	elapsed := time.Since(start)

	if got := hits.Load(); got != 1 {
		t.Errorf("400 时请求次数 = %d，期望 1（不该重试）", got)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("400 走了 %v，像是在空等退避", elapsed)
	}
}

// TestNotifyRetriesOn429 限流明确说了「稍后再来」，所以 429 要重试。
func TestNotifyRetriesOn429(t *testing.T) {
	st := newTestStore(t)
	devID := testDevice(t, st, "RETRY429")

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m := newNotifyManager(t, st, srv.URL, fastRetry(3))
	a := fireAlert(t, st, devID)
	m.notify(context.Background(), a)

	if got := hits.Load(); got != 2 {
		t.Errorf("429 后成功，请求次数 = %d，期望 2", got)
	}
	list, _ := st.ListAlerts(10)
	if !list[0].Notified {
		t.Error("重试成功后应标记已推送")
	}
}

// TestNotifyBackoffCancellable 关停契约：接收端挂住、进程要退出时，
// 正在退避等待的推送必须立刻收手 —— 否则把关停窗口（5s）睡满，
// 真正的库写入排空就被挤掉了（shutdownBackground 是全局唯一的窗口）。
func TestNotifyBackoffCancellable(t *testing.T) {
	st := newTestStore(t)
	devID := testDevice(t, st, "HANGUP")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	// 生产参数（2s 基数）：只用第一次尝试 + 一段长退避来复现「正卡在等待里」。
	m := newNotifyManager(t, st, srv.URL, retry.Config{
		Attempts:    3,
		Delay:       10 * time.Second, // 远大于任何合理的关停窗口
		MaxDelay:    10 * time.Second,
		Exponential: true,
		Retryable:   retryablePushErr,
	})
	a := fireAlert(t, st, devID)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.notify(ctx, a)
	}()

	time.Sleep(50 * time.Millisecond) // 让它发完第一次、正卡在退避里
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("取消后 2s 仍未返回 —— 退避等待没被 ctx 打断，会把关停窗口睡满")
	}
}

// TestPushErrClassification 直接钉住分类表：分类错了，上面几条的结论就都不成立。
func TestPushErrClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"500 可重试", &pushErr{err: io.ErrUnexpectedEOF, retryable: true}, true},
		{"400 不重试", &pushErr{err: io.ErrUnexpectedEOF, retryable: false}, false},
		{"非 pushErr 不重试", context.Canceled, false},
		{"nil 不重试", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := retryablePushErr(c.err); got != c.want {
				t.Errorf("retryablePushErr(%v) = %v，期望 %v", c.err, got, c.want)
			}
		})
	}
}

// TestNotifyConcurrentPushes 多个告警同时重试时，抖动让它们的退避不该齐刷刷对齐 ——
// 齐步走等于自己造出重试风暴。这里只断言「确实有多次尝试且都成功」，
// 抖动的统计性质不好在单测里断言（会 flaky）。
func TestNotifyConcurrentPushes(t *testing.T) {
	st := newTestStore(t)
	var (
		mu    sync.Mutex
		total int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		total++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m := newNotifyManager(t, st, srv.URL, fastRetry(2))
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		devID := testDevice(t, st, "CONC"+string(rune('A'+i)))
		a := fireAlert(t, st, devID)
		wg.Add(1)
		go func(a store.Alert) {
			defer wg.Done()
			m.notify(context.Background(), a)
		}(a)
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if total != 5 {
		t.Errorf("5 条告警各推一次 = %d 次，期望 5", total)
	}
}

// TestNotifyHungReceiverCancellable 比上一个更狠：接收端**根本不回**，
// 推送卡在 hc.Do 里面而不是退避里。hc.Timeout 是 10s，比关停窗口（5s）还长，
// 所以光靠超时兜不住 —— 必须靠请求 ctx 被取消。
func TestNotifyHungReceiverCancellable(t *testing.T) {
	st := newTestStore(t)
	devID := testDevice(t, st, "HUNGRCV")

	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done(): // 客户端主动断开
		}
	}))
	defer func() {
		close(block)
		srv.Close()
	}()

	m := newNotifyManager(t, st, srv.URL, retry.Config{
		Attempts:  1, // 只试一次，让它整个卡在 HTTP 上
		Retryable: retryablePushErr,
	})
	a := fireAlert(t, st, devID)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.notify(ctx, a)
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("取消后 2s 仍未返回 —— 请求没绑 ctx，关停会被 hc.Timeout(10s) 拖住")
	}
}
