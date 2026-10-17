package cwmp

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/zy84338719/lightcat/internal/store"
	"github.com/zy84338719/lightcat/internal/trace"
)

// syncBuf 并发安全日志缓冲。
type syncBuf struct {
	mu  sync.Mutex
	buf strings.Builder
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

// traceIDs 抽出日志里出现的所有 trace_id。
func (b *syncBuf) traceIDs() []string {
	var out []string
	for _, line := range strings.Split(b.String(), "\n") {
		for _, f := range strings.Fields(line) {
			if strings.HasPrefix(f, trace.Key+"=") {
				out = append(out, strings.TrimPrefix(f, trace.Key+"="))
			}
		}
	}
	return out
}

// newTraceServer 造一台带真实日志输出的 CWMP Server。
func newTraceServer(t *testing.T) (*Server, *store.Store, *syncBuf) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "trace.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	buf := &syncBuf{}
	log := slog.New(trace.SlogHandler(slog.NewTextHandler(buf, nil)))
	// AutoFetchDeviceInfo 打开，才能看到「已入队：取设备基本信息」这类级联日志
	srv := NewServer(st, Config{Path: "/acs", AutoFetchDeviceInfo: true}, log)
	return srv, st, buf
}

// buildInformEnvelope 造一条最小可解析的 Inform 信封。
func buildInformEnvelope(t *testing.T, serial, event string) string {
	t.Helper()
	body := `<?xml version="1.0"?>
<soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/">
 <soap:Body>
  <Inform xmlns="urn:dslforum-org:cwmp-1-0">
   <DeviceId><OUI>001122</OUI><ProductClass>R</ProductClass><SerialNumber>` + serial + `</SerialNumber></DeviceId>
   <Event><EventStruct><EventCode>` + event + `</EventCode></EventStruct></Event>
   <ParameterList><ParameterValueStruct><Name>InternetGatewayDevice.X</Name><Value>1</Value></ParameterValueStruct></ParameterList>
  </Inform>
 </soap:Body>
</soap:Envelope>`
	if !strings.Contains(body, serial) {
		t.Fatal("信封没带上 serial")
	}
	return body
}

// TestInformCascadeSharesTraceID 是本 issue 的核心验收点：
// 一次 Inform 触发的**全部**日志行必须带同一个 trace_id，否则用户贴一段日志出来
// 我们没法回答「这十几条是不是同一次上报」。
func TestInformCascadeSharesTraceID(t *testing.T) {
	srv, st, buf := newTraceServer(t)
	ctx := trace.With(context.Background(), "trace-abc-123")

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/acs", strings.NewReader(buildInformEnvelope(t, "TRACE1", "1 BOOT"))).WithContext(ctx)
	srv.ServeHTTP(w, r)

	out := buf.String()
	if out == "" {
		t.Fatal("没有任何日志输出")
	}
	ids := buf.traceIDs()
	if len(ids) == 0 {
		t.Fatalf("日志里一个 trace_id 都没有：\n%s", out)
	}
	for i, id := range ids {
		if id != "trace-abc-123" {
			t.Errorf("第 %d 行的 trace_id = %q，期望 trace-abc-123（Inform 级联内必须同值）\n%s", i, id, out)
		}
	}
	// 级联至少要有「纳管」+「入队」两类日志，否则这条测试会退化成只验一行
	if !strings.Contains(out, "收到 Inform") {
		t.Errorf("没看到「收到 Inform」：\n%s", out)
	}
	if !strings.Contains(out, "已入队") {
		t.Errorf("没看到「已入队：…」——级联没串起来：\n%s", out)
	}
	if len(ids) < 2 {
		t.Errorf("只出现 %d 条带 trace_id 的日志，级联覆盖不足：\n%s", len(ids), out)
	}

	// 落库的 Inform 记录里也得有它，面板才能显示
	dev, err := st.FindDeviceBySerial("TRACE1")
	if err != nil {
		t.Fatalf("取设备失败: %v", err)
	}
	records, err := st.ListInforms(dev.ID, 10)
	if err != nil || len(records) == 0 {
		t.Fatalf("取上报记录失败: %v", err)
	}
	if records[0].TraceID != "trace-abc-123" {
		t.Errorf("入库的 trace_id = %q，期望 trace-abc-123", records[0].TraceID)
	}
}

// TestNoTraceIDNoInjection 没有 trace_id 的上下文（面板触发的路径传 Background）
// 不该凭空注入空值 —— 那会让每一行这类日志都多个 trace_id=""。
func TestNoTraceIDNoInjection(t *testing.T) {
	srv, _, buf := newTraceServer(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/acs", strings.NewReader(buildInformEnvelope(t, "TRACE2", "1 BOOT")))
	srv.ServeHTTP(w, r.WithContext(context.Background()))

	if buf.String() == "" {
		t.Skip("该配置下没有日志输出")
	}
	if strings.Contains(buf.String(), trace.Key) {
		t.Errorf("无 trace_id 时不该注入该字段：\n%s", buf.String())
	}
}

var _ = io.Discard
