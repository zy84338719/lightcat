package web

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	hertzserver "github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/adaptor"

	"github.com/zy84338719/lightcat/internal/trace"
)

// syncBuf 是并发安全的日志缓冲：Hertz 会并发调 handler，裸 bytes.Buffer 会 data race。
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// startTraceServer 起一台带 trace 中间件的引擎。handler 是普通 http.Handler
// （经 adaptor 挂载，与面板 mux / CWMP 的接法完全一致）—— 测的正是这条链。
func startTraceServer(t *testing.T) (string, *syncBuf) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	buf := &syncBuf{}
	log := slog.New(trace.SlogHandler(slog.NewTextHandler(buf, nil)))

	e := hertzserver.New(
		hertzserver.WithListener(ln),
		hertzserver.WithDisablePrintRoute(true),
	)
	RegisterTraceMiddleware(e)
	e.GET("/probe", adaptor.HertzHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := trace.From(r.Context())
		log.InfoContext(r.Context(), "处理请求", "path", r.URL.Path)
		w.Header().Set("X-Seen-Trace", id)
		_, _ = w.Write([]byte(id))
	})))
	go func() { _ = e.Run() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		_ = e.Shutdown(ctx)
	})
	return "http://" + ln.Addr().String(), buf
}

// TestTraceIDReachesHandlerAndLog 是整条链的验收点：
// Hertz 中间件注入 → adaptor 把 ctx 传进 http.Request → slog 自动补上 trace_id。
func TestTraceIDReachesHandlerAndLog(t *testing.T) {
	base, buf := startTraceServer(t)
	resp, err := http.Get(base + "/probe")
	if err != nil {
		t.Fatalf("GET 失败: %v", err)
	}
	defer resp.Body.Close()

	seen := resp.Header.Get("X-Seen-Trace")
	if seen == "" {
		t.Fatal("handler 没从 r.Context() 取到 trace_id")
	}
	// 响应头也回写一份，便于前端/排障直接看到
	if got := resp.Header.Get(trace.Header); got != seen {
		t.Errorf("响应头 %s = %q，与 handler 看到的 %q 不一致", trace.Header, got, seen)
	}
	out := buf.String()
	if !strings.Contains(out, "处理请求") {
		t.Fatalf("日志行本身没出来：\n%s", out)
	}
	if !strings.Contains(out, trace.Key+"="+seen) {
		t.Errorf("日志里没有 %s=%s，实际输出：\n%s", trace.Key, seen, out)
	}
}

// TestTraceIDReusesInboundHeader 上游（反代 / WAF / 日志采集）带了 trace_id
// 就沿用：一次请求穿过多层代理时不该每层都变，否则按它搜日志会断成几段。
func TestTraceIDReusesInboundHeader(t *testing.T) {
	base, _ := startTraceServer(t)
	req, _ := http.NewRequest("GET", base+"/probe", nil)
	req.Header.Set(trace.Header, "upstream-supplied-id")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET 失败: %v", err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("X-Seen-Trace"); got != "upstream-supplied-id" {
		t.Errorf("trace_id = %q，期望沿用上游的 upstream-supplied-id", got)
	}
}

// TestTraceHandlerNoTraceIDLeavesFormatUnchanged 没有 trace_id 时不该凭空多出
// trace_id="" —— 那会让所有非请求触发的后台任务日志都变形。
func TestTraceHandlerNoTraceIDLeavesFormatUnchanged(t *testing.T) {
	buf := &syncBuf{}
	log := slog.New(trace.SlogHandler(slog.NewTextHandler(buf, nil)))
	log.Info("后台任务", "k", "v")
	out := buf.String()
	if strings.Contains(out, trace.Key) {
		t.Errorf("无 ctx 时不该注入 trace_id：\n%s", out)
	}
	if !strings.Contains(out, "msg=后台任务") || !strings.Contains(out, "k=v") {
		t.Errorf("原有字段被破坏了：\n%s", out)
	}
}

// TestTraceHandlerNoDuplicate 显式带过的 trace_id 不该被补成两个。
func TestTraceHandlerNoDuplicate(t *testing.T) {
	buf := &syncBuf{}
	log := slog.New(trace.SlogHandler(slog.NewTextHandler(buf, nil)))
	ctx := trace.With(context.Background(), "explicit-id")
	log.InfoContext(ctx, "显式带过", trace.Key, "mine")
	out := buf.String()
	if n := strings.Count(out, trace.Key+"="); n != 1 {
		t.Errorf("trace_id 出现了 %d 次，期望 1：\n%s", n, out)
	}
	if !strings.Contains(out, trace.Key+"=mine") {
		t.Errorf("应保留显式值：\n%s", out)
	}
}

// TestTraceMiddlewareAppliesToCatchAll 中间件必须在**任何路由注册之前**挂：
// Hertz 的 RouterGroup.Use 是把中间件追加进 group.Handlers，而 handle() 在
// 注册时就快照了这条链 —— 晚挂对已注册的路由无效。
// 这条测试防的就是「有人后来把 RegisterTraceMiddleware 挪到 NewHertzEngine 之后」。
func TestTraceMiddlewareAppliesToCatchAll(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Seen-Trace", trace.From(r.Context()))
		_, _ = w.Write([]byte("catchall"))
	})
	e := NewHertzEngine(mux, ln, nil) // 生产同一条装配路径
	go func() { _ = e.Run() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		_ = e.Shutdown(ctx)
	})

	resp, err := http.Get("http://" + ln.Addr().String() + "/anything")
	if err != nil {
		t.Fatalf("GET 失败: %v", err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("X-Seen-Trace"); got == "" {
		t.Error("兜底路由没拿到 trace_id —— 中间件挂晚了（Hertz 在路由注册时快照中间件链）")
	}
}
