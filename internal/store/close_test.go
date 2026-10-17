package store

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
)

// TestClose_Idempotent 盯住 OEK-1 的幂等契约。
//
// 为什么要在意：关库有两条路径（正常 SIGTERM 与面板「立即重启服务」），
// 第二次 *sql.DB.Close 返回 sql.ErrConnDone 是个无信息量的假故障，
// 会把真问题淹没在日志里。
func TestClose_Idempotent(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "close.db"))
	if err != nil {
		t.Fatalf("打开库失败: %v", err)
	}
	if st.Closed() {
		t.Fatal("刚打开的库不应是 Closed 状态")
	}

	first := st.Close()
	second := st.Close()
	third := st.Close()

	if first != nil {
		t.Errorf("首次 Close 返回错误: %v", first)
	}
	if second != nil || third != nil {
		t.Errorf("重复 Close 应返回同一个 nil 成功结果，实际 second=%v third=%v", second, third)
	}
	if !st.Closed() {
		t.Error("Close 后 Closed() 应为 true")
	}
}

// TestClose_ConcurrentSafe 确认并发调用 Close 不会 panic 或竞态：
// sync.Once 是这里的全部意义所在，-race 下必须干净。
func TestClose_ConcurrentSafe(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "close_race.db"))
	if err != nil {
		t.Fatalf("打开库失败: %v", err)
	}

	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = st.Close()
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("并发 Close[%d] 返回错误: %v", i, err)
		}
	}
}

// TestErrClosed_IsDistinct 保证 ErrClosed 是个能被 errors.Is 认出来的哨兵 ——
// 关停期的写库失败要能和真故障区分开（见 ErrClosed 的注释）。
func TestErrClosed_IsDistinct(t *testing.T) {
	wrapped := wrapForTest(ErrClosed)
	if !errors.Is(wrapped, ErrClosed) {
		t.Error("包装后的错误无法被 errors.Is 识别为 ErrClosed")
	}
	if errors.Is(wrapped, errOtherForTest) {
		t.Error("不相关错误被误判为 ErrClosed")
	}
}

var errOtherForTest = errors.New("别的错")

func wrapForTest(err error) error {
	return &wrapped{err}
}

type wrapped struct{ inner error }

func (w *wrapped) Error() string { return "上下文: " + w.inner.Error() }
func (w *wrapped) Unwrap() error { return w.inner }
