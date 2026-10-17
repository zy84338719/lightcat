package web

import (
	"encoding/json"
	stderrors "errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oekit/pkg/errors"

	"github.com/zy84338719/lightcat/internal/store"
)

func newAPITestServer(t *testing.T) (*http.ServeMux, *store.Store, int64) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "api_code.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	id, _, err := st.UpsertDevice(&store.Device{
		OUI: "001122", ProductClass: "CodeRouter", SerialNumber: "CODE-1",
	})
	if err != nil {
		t.Fatalf("建设备失败: %v", err)
	}

	mux := http.NewServeMux()
	if _, err := Register(mux, st, &stubCtrl{}, Options{}); err != nil {
		t.Fatalf("挂路由失败: %v", err)
	}
	return mux, st, id
}

func getJSON(t *testing.T, mux *http.ServeMux, path string) (int, map[string]any) {
	t.Helper()
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	return w.Code, body
}

// TestAPIError_BodyCarriesBizCode 钉住 OEK-5 的契约：
// 错误体必须同时带 code（机器读）与 error（人读）。
func TestAPIError_BodyCarriesBizCode(t *testing.T) {
	mux, _, _ := newAPITestServer(t)

	cases := []struct {
		name     string
		path     string
		wantHTTP int
		wantCode int
	}{
		{"设备不存在", "/api/devices/999999", http.StatusNotFound, CodeNotFound},
		{"设备 ID 非法", "/api/devices/not-a-number", http.StatusBadRequest, CodeBadRequest},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, body := getJSON(t, mux, c.path)
			if status != c.wantHTTP {
				t.Errorf("HTTP 状态 = %d，期望 %d", status, c.wantHTTP)
			}
			code, ok := body["code"].(float64)
			if !ok {
				t.Fatalf("错误体缺 code 字段：%v", body)
			}
			if int(code) != c.wantCode {
				t.Errorf("业务码 = %d，期望 %d", int(code), c.wantCode)
			}
			if msg, _ := body["error"].(string); msg == "" {
				t.Error("错误体缺 error 文案")
			}
		})
	}
}

// TestAPIError_UnauthCarriesCode 未认证响应也要带业务码，
// 否则抓取器只能靠 HTTP 状态猜「是没登录还是路径不存在」。
func TestAPIError_UnauthCarriesCode(t *testing.T) {
	hash, _ := HashPassword("pw-123456")
	creds := NewCreds("admin", hash, []byte("test-secret-key"))
	mux := newAuthTestMux(t, creds)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/devices", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("未带凭据访问 /api 的状态 = %d，期望 401", w.Code)
	}

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析 401 响应失败: %v；body=%s", err, w.Body.String())
	}
	if code, ok := body["code"].(float64); !ok || int(code) != CodeUnauthorized {
		t.Errorf("401 响应缺业务码 %d：%v", CodeUnauthorized, body)
	}
	if msg, _ := body["error"].(string); msg == "" {
		t.Errorf("401 响应缺 error 文案：%v", body)
	}
}

// TestAPIError_BizCodeFromStatus 确认状态码 → 业务码的默认映射没写错方向。
func TestAPIError_BizCodeFromStatus(t *testing.T) {
	cases := []struct {
		status int
		want   int
	}{
		{http.StatusBadRequest, CodeBadRequest},
		{http.StatusUnauthorized, CodeUnauthorized},
		{http.StatusForbidden, CodeUnauthorized},
		{http.StatusNotFound, CodeNotFound},
		{http.StatusInternalServerError, CodeInternal},
		{http.StatusNotImplemented, CodeInternal}, // 没定义的态统一归内部，不编新码
	}
	for _, c := range cases {
		if got := codeForStatus(c.status); got != c.want {
			t.Errorf("codeForStatus(%d) = %d，期望 %d", c.status, got, c.want)
		}
	}
}

// TestAPIError_MessageHasNoBizCodePrefix 防回归：BizError.Error() 自带
// "biz_code:8004 " 前缀，直接透出会污染用户可见文案。
func TestAPIError_MessageHasNoBizCodePrefix(t *testing.T) {
	ae := apiErr(http.StatusNotFound, "设备不存在")
	if strings.Contains(ae.Error(), "biz_code") {
		t.Errorf("错误文案里混进了 biz_code 前缀：%q", ae.Error())
	}
	if ae.Error() != "设备不存在" {
		t.Errorf("文案 = %q，期望 %q", ae.Error(), "设备不存在")
	}
}

// TestAPIError_UnderlyingIsBizError 确认底层真的是 oekit BizError，
// 这样外部能用 errors.AsBizError 统一取码，不必认识 web.APIError。
func TestAPIError_UnderlyingIsBizError(t *testing.T) {
	ae := apiErrCode(CodeUnavailable, http.StatusInternalServerError, "未接入控制接口")

	var be *errors.BizError
	if !stderrors.As(ae.Err, &be) {
		t.Fatal("APIError.Err 不是 BizError，外部取不到业务码")
	}
	if be.BizCode != CodeUnavailable {
		t.Errorf("BizError.BizCode = %d，期望 %d", be.BizCode, CodeUnavailable)
	}
	if be.HTTPCode != http.StatusInternalServerError {
		t.Errorf("BizError.HTTPCode = %d，期望 %d", be.HTTPCode, http.StatusInternalServerError)
	}
	if be.Message != "未接入控制接口" {
		t.Errorf("BizError.Message = %q", be.Message)
	}
}

// TestBizCodeOf 确认统一读码入口对业务错误与非业务错误都成立。
func TestBizCodeOf(t *testing.T) {
	if got := bizCodeOf(apiErr(http.StatusNotFound, "设备不存在")); got != CodeNotFound {
		t.Errorf("bizCodeOf(APIError) = %d，期望 %d", got, CodeNotFound)
	}
	be := errors.NewBizError(8009, "冲突")
	if got := bizCodeOf(stderrors.New("外层: " + be.Error())); got == be.BizCode {
		t.Errorf("非包装文本里不应解析出业务码，得到 %d", got)
	}
	if got := bizCodeOf(stderrors.New("普通错误")); got != 0 {
		t.Errorf("普通错误的业务码应为 0，得到 %d", got)
	}
}
