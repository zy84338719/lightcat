package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/zy84338719/lightcat/internal/store"
)

// 动作文本解析：SET/GET、类型、合并、注释与报错口径。
func TestParsePresetActions(t *testing.T) {
	acts, err := parsePresetActions(`
# 开机把周期上报调成 300 秒，再读一次版本
SET InternetGatewayDevice.ManagementServer.PeriodicInformInterval=300|unsignedInt
SET InternetGatewayDevice.ManagementServer.PeriodicInformEnable=1|boolean
GET InternetGatewayDevice.DeviceInfo.SoftwareVersion
`)
	if err != nil {
		t.Fatalf("合法输入不该报错: %v", err)
	}
	// 相邻 SET 合并成一条，GET 单独一条 —— 顺序保持
	if len(acts) != 2 {
		t.Fatalf("应合并成 2 个动作，得到 %d: %+v", len(acts), acts)
	}
	if acts[0].Kind != store.PresetActionSet || len(acts[0].Values) != 2 {
		t.Errorf("第一个动作应是含 2 项的 SET: %+v", acts[0])
	}
	if acts[0].Values[0].Type != "unsignedInt" || acts[0].Values[0].Value != "300" {
		t.Errorf("类型/值解析不对: %+v", acts[0].Values[0])
	}
	if acts[1].Kind != store.PresetActionGet || acts[1].Names[0] != "InternetGatewayDevice.DeviceInfo.SoftwareVersion" {
		t.Errorf("GET 动作不对: %+v", acts[1])
	}

	// SET/GET 交替：顺序不能被合并打乱
	acts, err = parsePresetActions("SET a.b=1\nGET c.d\nSET e.f=2")
	if err != nil {
		t.Fatal(err)
	}
	if len(acts) != 3 || acts[0].Kind != store.PresetActionSet ||
		acts[1].Kind != store.PresetActionGet || acts[2].Kind != store.PresetActionSet {
		t.Errorf("交替顺序不对: %+v", acts)
	}

	// 默认类型 string；值里可以带 =（按第一个 = 切）
	acts, _ = parsePresetActions("SET Device.WiFi.SSID.1.SSID=My=Net")
	if acts[0].Values[0].Value != "My=Net" || acts[0].Values[0].Type != "string" {
		t.Errorf("值/类型不对: %+v", acts[0].Values[0])
	}

	// 报错口径：行号 + 原因
	for _, bad := range []string{"", "   ", "# 只有注释", "DEL x.y", "SET novalue", "GET"} {
		if _, err := parsePresetActions(bad); err == nil {
			t.Errorf("%q 应报错", bad)
		}
	}
	if _, err := parsePresetActions("DEL x"); !strings.Contains(err.Error(), "第 1 行") {
		t.Errorf("报错应带行号: %v", err)
	}
}

func newPresetTestEnv(t *testing.T) (*store.Store, *http.ServeMux) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "lightcat.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	mux := http.NewServeMux()
	if err := Register(mux, st, &stubCtrl{}, Options{}); err != nil {
		t.Fatal(err)
	}
	return st, mux
}

// 预设页：空态 → 建一条 → 列表可见 → 停用/启用 → 删除。
func TestPresetsPageFlow(t *testing.T) {
	st, mux := newPresetTestEnv(t)

	// 空态
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/presets", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "还没有预设") {
		t.Fatalf("空态页不对: %d", w.Code)
	}

	// 新建（表单 POST）
	form := url.Values{
		"name":       {"纳管即配好周期上报"},
		"events":     {"0 BOOTSTRAP"},
		"new_device": {"on"},
		"once":       {"on"},
		"cond_op":    {"eq"},
		"actions":    {"SET InternetGatewayDevice.ManagementServer.PeriodicInformInterval=300|unsignedInt"},
	}
	w = httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/presets", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("新建应 303，得到 %d：%s", w.Code, w.Body.String())
	}
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "msg=") || strings.Contains(loc, "err=1") {
		t.Fatalf("新建应带成功提示回列表: %q", loc)
	}
	list, _ := st.ListPresets()
	if len(list) != 1 || !list[0].OncePerDevice || len(list[0].Actions) != 1 {
		t.Fatalf("预设没存对: %+v", list)
	}
	pid := list[0].ID

	// 列表可见（名称、触发、动作摘要）
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/presets", nil))
	body := w.Body.String()
	for _, want := range []string{"纳管即配好周期上报", "0 BOOTSTRAP", "首次纳管", "写 1 项"} {
		if !strings.Contains(body, want) {
			t.Errorf("列表页缺少 %q", want)
		}
	}

	// 停用 → 启用
	toggle := func(enable string) {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/presets/"+strconv.FormatInt(pid, 10)+"/toggle",
			strings.NewReader("enable="+enable))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusSeeOther {
			t.Fatalf("toggle 应 303，得到 %d", w.Code)
		}
	}
	toggle("0")
	if p, _ := st.ListPresets(); p[0].Enabled {
		t.Error("停用没生效")
	}
	toggle("1")
	if p, _ := st.ListPresets(); !p[0].Enabled {
		t.Error("启用没生效")
	}

	// 删除
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("POST", "/presets/"+strconv.FormatInt(pid, 10)+"/delete", nil))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("删除应 303，得到 %d", w.Code)
	}
	if p, _ := st.ListPresets(); len(p) != 0 {
		t.Error("删除没生效")
	}
}

// 表单校验：没触发条件 / 坏动作 / 坏事件码都要带错误提示回表单，不落库。
func TestPresetCreateValidation(t *testing.T) {
	st, mux := newPresetTestEnv(t)
	post := func(form url.Values) string {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/presets", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusSeeOther {
			t.Fatalf("应 303，得到 %d", w.Code)
		}
		return w.Header().Get("Location")
	}

	// 没有任何触发条件
	loc := post(url.Values{"name": {"x"}, "actions": {"GET a.b"}})
	if !strings.Contains(loc, "err=1") {
		t.Errorf("没触发条件应报错: %q", loc)
	}
	// 坏动作
	loc = post(url.Values{"name": {"x"}, "events": {"1 BOOT"}, "actions": {"rm -rf /"}})
	if !strings.Contains(loc, "err=1") {
		t.Errorf("坏动作应报错: %q", loc)
	}
	// 不认识的事件码（表单伪造）
	loc = post(url.Values{"name": {"x"}, "events": {"9 HACK"}, "actions": {"GET a.b"}})
	if !strings.Contains(loc, "err=1") {
		t.Errorf("坏事件码应报错: %q", loc)
	}
	if list, _ := st.ListPresets(); len(list) != 0 {
		t.Errorf("校验失败的都不该落库，得到 %d 条", len(list))
	}
}

// JSON 接口（自动化用）。
func TestPresetsAPI(t *testing.T) {
	st, mux := newPresetTestEnv(t)
	if _, err := st.CreatePreset(&store.Preset{
		Name: "api-test", Enabled: true, Events: []string{"1 BOOT"},
		Actions: []store.PresetAction{{Kind: store.PresetActionGet, Names: []string{"a.b"}}},
	}); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/presets", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "api-test") {
		t.Fatalf("API 不对: %d %s", w.Code, w.Body.String())
	}
}
