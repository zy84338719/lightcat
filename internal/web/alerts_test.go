package web

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/zy84338719/lightcat/internal/store"
)

func newAlertTestEnv(t *testing.T) (*store.Store, *http.ServeMux, int64) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "lightcat.db"))
	if err != nil {
		t.Fatalf("打开库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	devID, _, err := st.UpsertDevice(&store.Device{OUI: "001122", ProductClass: "R", SerialNumber: "ALERT-PAGE", ModelName: "T1"})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	if _, err := Register(mux, st, &stubCtrl{}, Options{}); err != nil {
		t.Fatalf("挂路由失败: %v", err)
	}
	return st, mux, devID
}

func getBody(t *testing.T, mux *http.ServeMux, path string) (int, string) {
	t.Helper()
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	return w.Code, w.Body.String()
}

// 告警页：有记录渲染行（类型标签 / 内容 / 设备链接 / 推送状态），没有就显示空态。
func TestAlertsPage(t *testing.T) {
	st, mux, devID := newAlertTestEnv(t)

	// 空态
	code, body := getBody(t, mux, "/alerts")
	if code != http.StatusOK {
		t.Fatalf("状态 = %d", code)
	}
	if !strings.Contains(body, "还没有告警。") {
		t.Error("空库应显示空态文案")
	}

	if err := st.InsertAlert(&store.Alert{
		DeviceID: devID, Kind: "rx_low",
		Message:   "收光功率 -28.50 dBm，低于阈值 -27.00 dBm",
		Value:     sql.NullFloat64{Float64: -28.5, Valid: true},
		Threshold: sql.NullFloat64{Float64: -27, Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertAlert(&store.Alert{DeviceID: devID, Kind: "offline", Message: "设备判定离线（多次探测无响应）", Notified: true}); err != nil {
		t.Fatal(err)
	}

	code, body = getBody(t, mux, "/alerts")
	if code != http.StatusOK {
		t.Fatalf("状态 = %d", code)
	}
	for _, want := range []string{
		"收光过低", "设备离线", // 类型标签
		"-28.50 dBm", // 内容
		"/devices/" + strconv.FormatInt(devID, 10), // 设备链接
		"ALERT-PAGE", // 序列号
		"已推送",        // notified 徽标
		"共 2 条",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("告警页缺少 %q", want)
		}
	}
}

// JSON 接口：脚本/自动化用。
func TestAlertsAPI(t *testing.T) {
	st, mux, devID := newAlertTestEnv(t)
	if err := st.InsertAlert(&store.Alert{DeviceID: devID, Kind: "temp_high", Message: "光模块温度 81.5 ℃，高于阈值 70.0 ℃"}); err != nil {
		t.Fatal(err)
	}
	code, body := getBody(t, mux, "/api/alerts")
	if code != http.StatusOK {
		t.Fatalf("状态 = %d", code)
	}
	var resp struct {
		Data []struct {
			Kind    string `json:"Kind"`
			Message string `json:"Message"`
		} `json:"data"`
		Count int `json:"count"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("解析 JSON 失败: %v（body=%.200s）", err, body)
	}
	if resp.Count != 1 || len(resp.Data) != 1 || resp.Data[0].Kind != "temp_high" {
		t.Errorf("JSON 内容不对: %+v", resp)
	}
}

// 设备详情页：有告警显示「最近告警」块，没有就整块不出现（不摆空壳）。
func TestDevicePageAlertBlock(t *testing.T) {
	st, mux, devID := newAlertTestEnv(t)
	other, _, err := st.UpsertDevice(&store.Device{OUI: "001122", ProductClass: "R", SerialNumber: "NOALERT"})
	if err != nil {
		t.Fatal(err)
	}

	_, body := getBody(t, mux, "/devices/"+strconv.FormatInt(other, 10))
	if strings.Contains(body, "最近告警") {
		t.Error("没有告警的设备不该出现告警块")
	}

	if err := st.InsertAlert(&store.Alert{DeviceID: devID, Kind: "optic_down", Message: "光口状态异常：Down"}); err != nil {
		t.Fatal(err)
	}
	_, body = getBody(t, mux, "/devices/"+strconv.FormatInt(devID, 10))
	if !strings.Contains(body, "最近告警") {
		t.Error("有告警的设备应显示「最近告警」块")
	}
	if !strings.Contains(body, "光口异常") || !strings.Contains(body, "查看全部告警") {
		t.Error("告警块内容不完整")
	}
}

// 首页顶栏：「告警」入口带条数徽标。
func TestIndexAlertBadge(t *testing.T) {
	st, mux, devID := newAlertTestEnv(t)

	_, body := getBody(t, mux, "/")
	if !strings.Contains(body, `href="/alerts"`) {
		t.Error("首页应有告警入口")
	}
	if strings.Contains(body, "告警（") {
		t.Error("没有告警时不该带计数")
	}

	if err := st.InsertAlert(&store.Alert{DeviceID: devID, Kind: "offline", Message: "m"}); err != nil {
		t.Fatal(err)
	}
	_, body = getBody(t, mux, "/")
	if !strings.Contains(body, "告警（1）") {
		t.Error("有告警时入口应带计数")
	}
}
