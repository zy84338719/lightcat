package web

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zy84338719/lightcat/internal/store"
)

func notfoundTestMux(t *testing.T) (*http.ServeMux, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "lightcat.db"))
	if err != nil {
		t.Fatalf("打开库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	mux := http.NewServeMux()
	if err := Register(mux, st, &stubCtrl{}, Options{}); err != nil {
		t.Fatalf("挂路由失败: %v", err)
	}
	return mux, st
}

// 共用端口模式（复刻 main.go 的挂法：CWMP 路由在前、Register 叠加在后）不得 panic。
// 历史 bug："GET /" 兜底与 "/acs"（不限方法）在 Go mux 的冲突规则里构成
// 「方法更少但路径更泛」的不可比组合，注册期直接把服务炸掉 —— e2e 抓到，这里钉死。
func TestRegisterOnSharedMuxNoPanic(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "lightcat.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cwmp := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "cwmp-405", http.StatusMethodNotAllowed)
			return
		}
		_, _ = w.Write([]byte("cwmp-ok"))
	})
	mux := http.NewServeMux()
	mux.Handle("/acs", cwmp)
	mux.Handle("POST /{$}", cwmp)
	if err := Register(mux, st, &stubCtrl{}, Options{}); err != nil {
		t.Fatal(err)
	}

	// CWMP 路径与根 POST 仍归 CWMP
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("POST", "/acs", strings.NewReader("<x/>")))
	if w.Body.String() != "cwmp-ok" {
		t.Errorf("POST /acs 应归 CWMP：%q", w.Body.String())
	}
	w = doGet(t, mux, "/acs")
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /acs 应由 CWMP 回 405，得到 %d", w.Code)
	}
	// 面板首页仍归 index
	w = doGet(t, mux, "/")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "还没有设备接入") {
		t.Errorf("GET / 应是面板首页，得到 %d", w.Code)
	}
	// GET 陌生路径 → 样式化 404（带顶栏与回首页入口）
	w = doGet(t, mux, "/no-such-page")
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "没有找到这个页面") {
		t.Errorf("陌生 GET 应是样式化 404，得到 %d", w.Code)
	}
	// POST 陌生路径 → 裸 404（设备/脚本世界不给 HTML）
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("POST", "/nope-not-a-route", strings.NewReader("<x/>")))
	if w.Code != http.StatusNotFound {
		t.Errorf("陌生 POST 应 404，得到 %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "<html") || strings.Contains(w.Body.String(), "没有找到") {
		t.Errorf("陌生 POST 不该回面板 HTML：%q", w.Body.String()[:min(80, w.Body.Len())])
	}
}

// 共用端口且 -path=/（CWMP 已注册 "/"）：Register 不得再挂兜底（重复模式会 panic），
// 面板首页仍由 "GET /{$}" 精确接管，根 POST 归 CWMP —— 与历史行为一致。
func TestRegisterSharedRootPathNoPanic(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "lightcat.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cwmp := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("cwmp-root"))
	})
	mux := http.NewServeMux()
	mux.Handle("/", cwmp) // main.go：cfg.Path == "/" 时的挂法
	if err := Register(mux, st, &stubCtrl{}, Options{}); err != nil {
		t.Fatal(err)
	}

	w := doGet(t, mux, "/")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "还没有设备接入") {
		t.Errorf("GET / 应被面板首页精确接管，得到 %d", w.Code)
	}
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("POST", "/", strings.NewReader("<x/>")))
	if w.Body.String() != "cwmp-root" {
		t.Errorf("POST / 应归 CWMP：%q", w.Body.String())
	}
}

// POST-only 面板路由的 GET 要用 405 挡掉（「重启接口只认 POST」契约），
// 不能被样式化 404 兜底吞掉。
func TestPostOnlyRoutesRejectGet405(t *testing.T) {
	mux, _ := notfoundTestMux(t)
	for _, p := range []string{"/settings/restart", "/devices/1/reboot", "/presets/1/delete"} {
		w := doGet(t, mux, p)
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("GET %s 应 405，得到 %d", p, w.Code)
		}
		if allow := w.Header().Get("Allow"); allow != "POST" {
			t.Errorf("GET %s 应带 Allow: POST，得到 %q", p, allow)
		}
	}
}

// 过期设备链接 → 样式化 404；/api 的未知端点 → JSON 404（机器端点不给 HTML）。
func TestStyledAndJSONNotFound(t *testing.T) {
	mux, _ := notfoundTestMux(t)

	w := doGet(t, mux, "/devices/9999")
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "没有找到这个页面") {
		t.Errorf("过期设备链接应是样式化 404，得到 %d", w.Code)
	}
	w = doGet(t, mux, "/api/no-such-endpoint")
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), `"error"`) {
		t.Errorf("/api 未知端点应回 JSON 404，得到 %d %q", w.Code, w.Body.String()[:min(80, w.Body.Len())])
	}
}
