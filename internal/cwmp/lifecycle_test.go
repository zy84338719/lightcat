package cwmp

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oekit/concurrency/gopool"

	"github.com/zy84338719/lightcat/internal/store"
)

// backdateInform 把库里所有设备的 last_inform_at 往前拨 d，造出「超期未上报」。
//
// 为什么要另开连接直改：Store 的公开 API 只会把 last_inform_at 写成**当前**时刻
// （TouchInform / UpsertDevice 都是 now），而离线判定必须有真实的时间差才能触发，
// 所以只能从旁边写一行 SQL。仅测试使用。
func backdateInform(t *testing.T, dbPath string, d time.Duration) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("另开连接失败: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE devices SET last_inform_at = ?`,
		time.Now().Add(-d).UTC().Format(time.RFC3339)); err != nil {
		t.Fatalf("回拨 last_inform_at 失败: %v", err)
	}
}

// probePolicyFor 是让「超期 → 主动探测」这条路径成立的配置。
// Deadline = Factor × PeriodicInterval = 2×120 = 240 秒，所以回拨 600 秒必超期。
func probePolicyFor(concurrency int) Config {
	return Config{
		OfflineProbe:         true,
		OfflineProbeFactor:   2,
		OfflineProbeAttempts: 3,
		OfflineProbeInterval: time.Second,
		OfflineProbeGrace:    30 * time.Second,
		OfflineAfter:         10 * time.Minute,
		ConnReqEnabled:       true,
		ConnReqTimeout:       2 * time.Second,
		ProbeConcurrency:     concurrency,
	}
}

// newProbeServer 建 N 台待探设备 + 一个会阻塞 hold 的「设备」HTTP 服务，
// 并记录在飞请求的**峰值并发**。
func newProbeServer(t *testing.T, n, concurrency int, hold time.Duration) (*Server, *store.Store, func() int32, *gopool.Pool) {
	t.Helper()

	var cur, peak atomic.Int32
	dev := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := cur.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(hold)
		cur.Add(-1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(dev.Close)

	dbPath := filepath.Join(t.TempDir(), "probe.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	for i := range n {
		if _, _, err := st.UpsertDevice(&store.Device{
			OUI:              "001122",
			ProductClass:     "PROBE",
			SerialNumber:     "P" + strconv.Itoa(i),
			ConnRequestURL:   dev.URL,
			PeriodicInterval: 120,
		}); err != nil {
			t.Fatalf("建待探设备失败: %v", err)
		}
	}
	backdateInform(t, dbPath, 600*time.Second)

	srv := NewServer(st, probePolicyFor(concurrency),
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	bg := gopool.NewPool("test-bg", gopool.PoolOption{MaxIdleWorkers: 32, TaskChanBuffer: 64})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = bg.Close(ctx)
	})
	WithBackground(srv, bg)

	return srv, st, peak.Load, bg
}

// TestSweepOffline_ProbeConcurrencyCapped 是 OEK-3 的验收断言：
// 一批设备同时掉线时，单轮巡检中同时在飞的 ConnectionRequest 不超过配置值。
//
// 反向验证：把 ProbeConcurrency 调到设备数以上（对照组）会让断言不成立 ——
// 也就是说这个闸是真的在限并发，不是恰好没触发。
func TestSweepOffline_ProbeConcurrencyCapped(t *testing.T) {
	const (
		devices     = 12
		concurrency = 3
	)
	srv, _, peak, bg := newProbeServer(t, devices, concurrency, 120*time.Millisecond)

	probed, _ := srv.SweepOffline(context.Background())
	if probed != devices {
		t.Fatalf("探测台数 = %d，期望 %d（闸只该限制并发，不该少派发）", probed, devices)
	}

	// 排空后台池，等所有探测真正落地
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := bg.Close(ctx); err != nil {
		t.Fatalf("后台池未排空: %v", err)
	}

	if got := peak(); got > concurrency {
		t.Errorf("峰值并发 = %d，超过闸值 %d", got, concurrency)
	}
	if got := peak(); got < 2 {
		t.Errorf("峰值并发 = %d，说明探测被串行化了（闸不该把并发压成 1）", got)
	}
}

// TestSweepOffline_CtxCancelStopsDispatch 验证关停时巡检会尽快放手：
// ctx 一取消，取闸立刻返回错误，巡检带着已派发数正常返回而不是硬等满批次。
func TestSweepOffline_CtxCancelStopsDispatch(t *testing.T) {
	const devices = 12
	// hold 设得很长：不取消的话闸根本放不出第二个批次。
	srv, _, _, _ := newProbeServer(t, devices, 2, 3*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var probed int
	go func() {
		defer close(done)
		probed, _ = srv.SweepOffline(ctx)
	}()

	// 等到第一批（2 台）确实在飞，再取消
	deadline := time.After(3 * time.Second)
	for srv.CurrentGateUsage() == 0 {
		select {
		case <-deadline:
			t.Fatal("等待探测起飞超时")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ctx 取消后 SweepOffline 没有及时返回（关停会被拖满一个批次）")
	}
	if probed >= devices {
		t.Errorf("取消后仍派发了 %d/%d 台，闸没有响应 ctx", probed, devices)
	}
}

// TestAcquireProbeSlot_ReleaseIsIdempotent 盯住闸的释放语义：
// 重复调用 release 不能把槽位多还回去（否则闸会越用越宽，并发上限形同虚设）。
func TestAcquireProbeSlot_ReleaseIsIdempotent(t *testing.T) {
	srv, _, _, _ := newProbeServer(t, 1, 1, 0)

	rel, err := srv.acquireProbeSlot(context.Background())
	if err != nil {
		t.Fatalf("取闸失败: %v", err)
	}
	if got := srv.CurrentGateUsage(); got != 1 {
		t.Fatalf("占用闸 = %d，期望 1", got)
	}
	rel()
	rel()
	rel()
	if got := srv.CurrentGateUsage(); got != 0 {
		t.Fatalf("重复释放后占用闸 = %d，期望 0（闸被多还了槽位）", got)
	}
}

// TestSweepOffline_ShutdownCancelledProbeNotCounted 关停期被取消的探测
// 不该算作「设备无响应」—— 否则一次正常 SIGTERM 会把全网设备推进离线判定。
func TestSweepOffline_ShutdownCancelledProbeNotCounted(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "cancel.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // 一直挂到客户端（= ctx）取消
	}))
	t.Cleanup(hang.Close)

	if _, _, err := st.UpsertDevice(&store.Device{
		OUI: "001122", ProductClass: "CANCEL", SerialNumber: "S1",
		ConnRequestURL: hang.URL, PeriodicInterval: 120,
	}); err != nil {
		t.Fatalf("建设备失败: %v", err)
	}
	backdateInform(t, dbPath, 600*time.Second)

	srv := NewServer(st, probePolicyFor(4),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	bg := gopool.NewPool("cancel-bg", gopool.PoolOption{MaxIdleWorkers: 8, TaskChanBuffer: 16})
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = bg.Close(ctx)
	}()
	WithBackground(srv, bg)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	srv.SweepOffline(ctx)
	if err := bg.Close(ctx); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("排空失败: %v", err)
	}

	d, err := st.GetDevice(1)
	if err != nil {
		t.Fatalf("读设备失败: %v", err)
	}
	if d.ProbeCount != 0 {
		t.Errorf("ProbeCount = %d，关停期取消的探测被记成了失败响应（会把设备推向离线判定）", d.ProbeCount)
	}
}
