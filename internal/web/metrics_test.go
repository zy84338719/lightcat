package web

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zy84338719/lightcat/internal/store"
)

func newMetricsEnv(t *testing.T, withAuth bool, token string) (*store.Store, *http.ServeMux) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "lightcat.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	opt := Options{APIToken: token}
	if withAuth {
		hash, err := HashPassword("panel-pass")
		if err != nil {
			t.Fatal(err)
		}
		opt.Auth = NewCreds("admin", hash, []byte("0123456789abcdef0123456789abcdef"))
	}
	mux := http.NewServeMux()
	if _, err := Register(mux, st, &stubCtrl{}, opt); err != nil {
		t.Fatal(err)
	}
	return st, mux
}

// 无鉴权部署：/metrics 直接可读，Prometheus 文本格式、指标齐全。
func TestMetricsOpenAccess(t *testing.T) {
	st, mux := newMetricsEnv(t, false, "")
	if _, _, err := st.UpsertDevice(&store.Device{OUI: "001122", ProductClass: "R", SerialNumber: "M1"}); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("状态 = %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") || !strings.Contains(ct, "version=0.0.4") {
		t.Errorf("Content-Type 不对: %q", ct)
	}
	body := w.Body.String()
	for _, want := range []string{
		"# TYPE lightcat_devices gauge",
		"lightcat_devices 1",
		"lightcat_devices_online",
		"lightcat_tasks_pending",
		"lightcat_alert_records_stored",
		"lightcat_optical_history_rows",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("指标输出缺 %q", want)
		}
	}
}

// 开了面板鉴权：/metrics 与 /api 要么带登录态、要么带对的 Bearer token。
func TestAPITokenAuth(t *testing.T) {
	st, mux := newMetricsEnv(t, true, "s3cret-token")
	if _, _, err := st.UpsertDevice(&store.Device{OUI: "001122", ProductClass: "R", SerialNumber: "M2"}); err != nil {
		t.Fatal(err)
	}

	get := func(path, auth string) int {
		req := httptest.NewRequest("GET", path, nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w.Code
	}

	// 什么都没有：401（/api 与 /metrics 都挡）
	if code := get("/metrics", ""); code != http.StatusUnauthorized {
		t.Errorf("/metrics 无凭据应 401，得到 %d", code)
	}
	if code := get("/api/devices", ""); code != http.StatusUnauthorized {
		t.Errorf("/api/devices 无凭据应 401，得到 %d", code)
	}
	// 对的 token：放行
	if code := get("/metrics", "Bearer s3cret-token"); code != http.StatusOK {
		t.Errorf("/metrics 带 token 应 200，得到 %d", code)
	}
	if code := get("/api/devices", "Bearer s3cret-token"); code != http.StatusOK {
		t.Errorf("/api/devices 带 token 应 200，得到 %d", code)
	}
	// 错的 token / 错的格式：仍然 401
	if code := get("/api/devices", "Bearer 不对"); code != http.StatusUnauthorized {
		t.Errorf("错 token 应 401，得到 %d", code)
	}
	if code := get("/api/devices", "Basic dXNlcjpwYXNz"); code != http.StatusUnauthorized {
		t.Errorf("非 Bearer 应 401，得到 %d", code)
	}
	// 页面路由不吃 token（token 只开 /api 与 /metrics 的门）
	if code := get("/", "Bearer s3cret-token"); code != http.StatusSeeOther {
		t.Errorf("页面应仍走登录跳转（303），得到 %d", code)
	}
}

// 配了鉴权但没配 token：行为与从前一致（只认登录态）。
func TestNoTokenConfiguredKeepsSessionOnly(t *testing.T) {
	_, mux := newMetricsEnv(t, true, "")
	req := httptest.NewRequest("GET", "/api/devices", nil)
	req.Header.Set("Authorization", "Bearer 随便什么")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("没配 token 时 Bearer 不该放行，得到 %d", w.Code)
	}
}

// 配了 API Token 但**没开**面板账号密码：机器端点仍必须校验 Token。
//
// 这个组合之前是有洞的 —— apiGuard 的 Bearer 分支被 opt.Auth.Enabled() 门控，
// 而 APIAuthOK 首行就 return true，于是「配了 token」看起来有防护、实际
// /api 全网可读（含序列号、IP、全部参数），而设置页还在提示 token 已生效。
// 现在配了 token 就强制校验，这条测试把这个语义钉住。
func TestAPITokenEnforcedWithoutPanelAuth(t *testing.T) {
	const token = "token-without-panel-auth"
	st, mux := newMetricsEnv(t, false, token)
	if _, _, err := st.UpsertDevice(&store.Device{OUI: "001122", ProductClass: "R", SerialNumber: "M3"}); err != nil {
		t.Fatal(err)
	}

	get := func(path, auth string) int {
		req := httptest.NewRequest("GET", path, nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w.Code
	}

	for _, path := range []string{"/metrics", "/api/devices"} {
		if code := get(path, ""); code != http.StatusUnauthorized {
			t.Errorf("%s 不带 token 应 401，实际 %d", path, code)
		}
		if code := get(path, "Bearer wrong-token"); code != http.StatusUnauthorized {
			t.Errorf("%s 带错 token 应 401，实际 %d", path, code)
		}
		if code := get(path, "Bearer "+token); code != http.StatusOK {
			t.Errorf("%s 带对 token 应 200，实际 %d", path, code)
		}
	}
}

// 反过来：既没配 token 也没开面板鉴权 = 部署时就没打算设防，保持原有公开语义。
// 这条是为了钉住「改了鉴权逻辑但没顺手把默认部署也锁上」。
func TestNoTokenNoPanelAuthStaysOpen(t *testing.T) {
	_, mux := newMetricsEnv(t, false, "")
	for _, path := range []string{"/metrics", "/api/devices"} {
		if code := func() int {
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
			return w.Code
		}(); code != http.StatusOK {
			t.Errorf("%s 在无 token 且无面板鉴权时应保持可访问，实际 %d", path, code)
		}
	}
}

// Hertz 侧 guard 用的 APIAuthOK 与 net/http 侧 apiGuard 必须同一口径。
//
// 2026-10-12 实测踩到过一次：/metrics 走 mux（200）而 /api/* 走 Hertz 原生路由
// （401），排查半天才发现是两边逻辑不同 —— Hertz 侧漏了「有登录态就放行」这条，
// 于是面板鉴权开着、没配 token 时 /api 对已登录用户也拒绝。生产流量走的正是
// Hertz 原生路由，所以必须单独钉住这个函数的四种组合。
func TestAPIAuthOKSemantics(t *testing.T) {
	newSrv := func(withPanel bool, token string) *Server {
		st, err := store.Open(filepath.Join(t.TempDir(), "a.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		opt := Options{APIToken: token}
		if withPanel {
			hash, err := HashPassword("panel-pass")
			if err != nil {
				t.Fatal(err)
			}
			opt.Auth = NewCreds("admin", hash, []byte("0123456789abcdef0123456789abcdef"))
		}
		return &Server{store: st, opt: opt}
	}
	// 一张真实有效的面板 cookie
	good := func(t *testing.T) string {
		t.Helper()
		hash, err := HashPassword("panel-pass")
		if err != nil {
			t.Fatal(err)
		}
		c := NewCreds("admin", hash, []byte("0123456789abcdef0123456789abcdef"))
		w := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/settings", nil)
		c.setCookie(w, req)
		for _, ck := range w.Result().Cookies() {
			if ck.Name == PanelCookieName {
				return ck.Value
			}
		}
		t.Fatal("拿不到登录 cookie")
		return ""
	}

	cases := []struct {
		name      string
		withPanel bool
		token     string
		cookie    string
		bearer    string
		want      bool
	}{
		{"开面板+无token+登录态 → 放行", true, "", "GOOD", "", true},
		{"开面板+无token+无登录态 → 拒", true, "", "", "", false},
		{"开面板+有token+登录态 → 放行", true, "tok", "GOOD", "", true},
		{"开面板+有token+对Bearer → 放行", true, "tok", "", "tok", true},
		{"开面板+有token+错Bearer → 拒", true, "tok", "", "nope", false},
		{"开面板+有token+什么都没有 → 拒", true, "tok", "", "", false},
		{"没开面板+有token+无Bearer → 拒（这就是修掉的洞）", false, "tok", "", "", false},
		{"没开面板+有token+对Bearer → 放行", false, "tok", "", "tok", true},
		{"没开面板+无token → 公开", false, "", "", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := newSrv(c.withPanel, c.token)
			cookie := ""
			if c.cookie == "GOOD" {
				cookie = good(t)
			}
			if got := srv.APIAuthOK(cookie, c.bearer); got != c.want {
				t.Errorf("APIAuthOK = %v, 想要 %v", got, c.want)
			}
		})
	}
}
