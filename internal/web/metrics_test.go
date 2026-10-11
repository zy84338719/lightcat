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
