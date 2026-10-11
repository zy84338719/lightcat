package web

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zy84338719/lightcat/internal/store"
)

// 设备列表页：有设备报过收/发光时才显示那两列（设备不报就不摆空列）。
func TestIndexShowsOpticalColumnsOnlyWhenReported(t *testing.T) {
	newMux := func(t *testing.T) (http.Handler, *store.Store) {
		t.Helper()
		st, err := store.Open(filepath.Join(t.TempDir(), "lightcat.db"))
		if err != nil {
			t.Fatalf("打开库失败: %v", err)
		}
		t.Cleanup(func() { st.Close() })
		mux := http.NewServeMux()
		if _, err := Register(mux, st, &stubCtrl{}, Options{}); err != nil {
			t.Fatalf("挂路由失败: %v", err)
		}
		return mux, st
	}

	// 场景 1：设备只报无线，不报光功率 → 不出现这两列
	mux, st := newMux(t)
	id1, _, err := st.UpsertDevice(&store.Device{OUI: "001122", ProductClass: "R", SerialNumber: "NO-OPT",
		Manufacturer: "Example", ModelName: "Sim", DataModelRoot: "InternetGatewayDevice."})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertParams(id1, []store.Param{
		{Name: "InternetGatewayDevice.LANDevice.1.WLANConfiguration.1.SSID", Value: "Net"},
		{Name: "InternetGatewayDevice.LANDevice.1.WLANConfiguration.1.Channel", Value: "6"},
	}, "getvalues"); err != nil {
		t.Fatal(err)
	}
	w := doGet(t, mux, "/")
	body := w.Body.String()
	if !strings.Contains(body, "NO-OPT") && !strings.Contains(body, "Sim") {
		t.Fatalf("列表页没渲染出设备，状态 %d：%s", w.Code, body[:200])
	}
	if strings.Contains(body, "<th>收光</th>") || strings.Contains(body, "<th>发光</th>") {
		t.Errorf("没有设备上报光功率时不该出现这两列")
	}

	// 场景 2：有一台报了收/发光 → 两列出现，报了的显示数值、没报的显示 -
	mux2, st2 := newMux(t)
	idA, _, err := st2.UpsertDevice(&store.Device{OUI: "001122", ProductClass: "R", SerialNumber: "OPT-A",
		Manufacturer: "Example", ModelName: "PON", DataModelRoot: "InternetGatewayDevice."})
	if err != nil {
		t.Fatal(err)
	}
	idB, _, err := st2.UpsertDevice(&store.Device{OUI: "001122", ProductClass: "R", SerialNumber: "OPT-B",
		Manufacturer: "Example", ModelName: "NoPON", DataModelRoot: "InternetGatewayDevice."})
	if err != nil {
		t.Fatal(err)
	}
	if err := st2.UpsertParams(idA, []store.Param{
		// 真机（联通版 V271-20）的私有命名 + 一对没换算的原始值当诱饵
		{Name: "InternetGatewayDevice.WANDevice.1.X_GponInterafceConfig.RXPower", Value: "-15"},
		{Name: "InternetGatewayDevice.WANDevice.1.X_GponInterafceConfig.TXPower", Value: "2"},
		{Name: "InternetGatewayDevice.WANDevice.1.X_CU_WANEdgeONTPONInterfaceConfig.OpticalTransceiver.RXPower", Value: "254"},
	}, "getvalues"); err != nil {
		t.Fatal(err)
	}
	if err := st2.UpsertParams(idB, []store.Param{
		{Name: "InternetGatewayDevice.DeviceInfo.UpTime", Value: "1"},
	}, "getvalues"); err != nil {
		t.Fatal(err)
	}

	w2 := doGet(t, mux2, "/")
	body2 := w2.Body.String()
	if !strings.Contains(body2, "<th>收光</th>") || !strings.Contains(body2, "<th>发光</th>") {
		t.Fatalf("有设备上报光功率时应该出现这两列")
	}
	// 有寄存器原始值（254）时用它换算出来的精确读数；没有时才用整数近似值
	if !strings.Contains(body2, "-15.95 dBm") || !strings.Contains(body2, "2.00 dBm") {
		t.Errorf("报了的设备该显示读数：%s", body2)
	}
	if strings.Contains(body2, "254 dBm") {
		t.Errorf("没换算的原始值不该当成功率显示")
	}
	// 没报的那台显示 -
	row := rowFor(t, body2, "OPT-B")
	if !strings.Contains(row, "<td class=\"mono\"><span class=\"hint\">-</span></td>") {
		t.Errorf("没上报的设备该显示 -：%s", row)
	}
}

// rowFor 抠出某一行的 HTML（按行里的关键字定位）。
func rowFor(t *testing.T, html, needle string) string {
	t.Helper()
	i := strings.Index(html, needle)
	if i < 0 {
		t.Fatalf("页面里没有 %q", needle)
	}
	start := strings.LastIndex(html[:i], "<tr>")
	end := strings.Index(html[i:], "</tr>")
	if start < 0 || end < 0 {
		t.Fatalf("找不到 %q 所在的行", needle)
	}
	return html[start : i+end]
}

// 历史兜底：ontweb 通道的设备拿不到 PON 口参数域，概览列应从光功率历史取最近读数
// （rx 缺失就空着，宁缺毋编）。
func TestIndexOpticalFallbackFromHistory(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "lightcat.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	mux := http.NewServeMux()
	if _, err := Register(mux, st, &stubCtrl{}, Options{}); err != nil {
		t.Fatal(err)
	}
	id, _, err := st.UpsertDevice(&store.Device{OUI: "001122", ProductClass: "R", SerialNumber: "HIST-DEV",
		Manufacturer: "Huawei", ModelName: "V271-20", UserAgent: "lightcat-ontweb/1.0"})
	if err != nil {
		t.Fatal(err)
	}
	// 参数库里没有光功率参数；历史里只有发光（普通账号边界）
	f := func(v float64) *float64 { return &v }
	if err := st.InsertOpticalSample(id, time.Now(), nil, f(-1.25), f(54), nil, nil, "Up"); err != nil {
		t.Fatal(err)
	}

	w := doGet(t, mux, "/")
	body := w.Body.String()
	if !strings.Contains(body, "HIST-DEV") {
		t.Fatalf("设备未渲染")
	}
	if !strings.Contains(body, "-1.25") {
		t.Errorf("概览发光列应从历史兜底显示 -1.25")
	}
	if !strings.Contains(body, "收光") && !strings.Contains(body, "发光") {
		t.Errorf("有历史读数时应出现收光/发光列")
	}
}
