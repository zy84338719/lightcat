package web

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	hertzserver "github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/adaptor"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	"github.com/zy84338719/lightcat/internal/store"
)

// startHealthServer 起一个与生产同构的引擎：先挂 NewHertzEngine 那样的
// "/*any" 兜底（面板 mux 经 adaptor 进来），再挂健康端点 ——
// 这样才验得出「静态路由压得过通配」，只挂健康路由的测法证明不了这点。
func startHealthServer(t *testing.T) (string, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "health.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}

	mux := http.NewServeMux() // 面板 mux：健康路径在它这里应当是 404
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(consts.StatusOK)
		_, _ = io.WriteString(w, "panel-home")
	})

	e := hertzserver.New(hertzserver.WithListener(ln), hertzserver.WithDisablePrintRoute(true))
	fallback := adaptor.HertzHandler(mux)
	e.Any("/", fallback)
	e.Any("/*any", fallback)
	RegisterHealthRoutes(e, st)

	go func() { _ = e.Run() }()
	t.Cleanup(func() {
		// 短超时即可：这里只是收掉测试用的监听器，不是在测优雅退出
		// （那件事由 cmd/lightcat 的 shutdown 测试负责）。
		// 用默认 5s 会让每个用例白白多花 5 秒。
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		_ = e.Shutdown(ctx)
	})
	return "http://" + ln.Addr().String(), st
}

func fetch(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s 失败: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读响应体失败: %v", err)
	}
	return resp.StatusCode, string(body)
}

// TestHealthEndpoints_BeatCatchAll 钉住路由优先级：健康端点必须是 Hertz 原生
// handler，不能掉进面板 mux 的通配兜底（掉进去就是面板首页 / 404）。
func TestHealthEndpoints_BeatCatchAll(t *testing.T) {
	base, _ := startHealthServer(t)

	cases := []struct {
		path string
		want string // 响应体里必须出现的标记
	}{
		{PingPath, "pong"},
		{VersionPath, "code"},
	}
	for _, c := range cases {
		code, body := fetch(t, base+c.path)
		if code != http.StatusOK {
			t.Errorf("%s 状态码 = %d，期望 200", c.path, code)
		}
		if !strings.Contains(body, c.want) {
			t.Errorf("%s 响应体 = %q，没有 %q（可能落到了面板兜底路由）", c.path, body, c.want)
		}
		if strings.Contains(body, "panel-home") {
			t.Errorf("%s 落到了面板首页（通配兜底抢在了静态路由前面）", c.path)
		}
	}
}

// TestHealthPath_ReportsSQLite 是就绪探针的主语义：库正常就 200。
func TestHealthPath_ReportsSQLite(t *testing.T) {
	base, _ := startHealthServer(t)

	code, body := fetch(t, base+HealthPath)
	if code != http.StatusOK {
		t.Fatalf("%s 状态码 = %d，期望 200；body=%s", HealthPath, code, body)
	}
	if !strings.Contains(body, "sqlite") {
		t.Errorf("响应体没有列出 sqlite 检查项：%s", body)
	}
}

// TestPingStaysUpWhenStoreClosed liveness 与 readiness 必须分开：
// 库挂了（正在关停、或磁盘故障）时 liveness 仍要答 pong，
// 否则 k8s 会把「只是库暂时不可用」的容器反复重启，越重启越坏。
func TestPingStaysUpWhenStoreClosed(t *testing.T) {
	base, st := startHealthServer(t)
	if err := st.Close(); err != nil {
		t.Fatalf("关库失败: %v", err)
	}

	code, body := fetch(t, base+PingPath)
	if code != http.StatusOK {
		t.Errorf("库关闭后 %s 状态码 = %d，期望 200（liveness 不该依赖库）", PingPath, code)
	}

	// readiness 则应当转为不通过，让流量被摘走
	code, body = fetch(t, base+HealthPath)
	if code == http.StatusOK {
		t.Errorf("库关闭后 %s 仍返回 200：%s —— 没起到摘流量的作用", HealthPath, body)
	}
}

// TestBuildVersionReportsVersion 版本端点形状检查：必须有 data.version 字段。
//
// 这里**不**去写 oekit/core/version.Version（那是 main 的活儿，见
// cmd/lightcat 的 syncOekitVersion）：同一个测试进程里其它用例的 Hertz server
// 正并发读那个包级变量，在这里写它会被 -race 直接判为 data race。
// 同步本身的正确性由 cmd/lightcat 的 TestSyncOekitVersion 盯。
func TestBuildVersionReportsVersion(t *testing.T) {
	base, _ := startHealthServer(t)
	_, body := fetch(t, base+VersionPath)

	var resp struct {
		Data struct {
			Version string `json:"version"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("解析版本响应失败: %v；body=%s", err, body)
	}
	if resp.Data.Version == "" {
		t.Errorf("版本端点返回空版本号：%s", body)
	}
}
