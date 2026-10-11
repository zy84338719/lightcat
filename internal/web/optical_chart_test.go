package web

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zy84338719/lightcat/internal/store"
)

func sample(rx, tx, temp float64) store.OpticalSample {
	s := store.OpticalSample{}
	s.RxDbm = sql.NullFloat64{Float64: rx, Valid: rx != 0}
	s.TxDbm = sql.NullFloat64{Float64: tx, Valid: tx != 0}
	s.TempC = sql.NullFloat64{Float64: temp, Valid: temp != 0}
	return s
}

// 空值（如 Web 通道没有收光）不该画出来，也不该把曲线拉变形：
// 少于 2 个有效点返回空串，整块不渲染。
func TestSparkPointsRxSkipsInvalid(t *testing.T) {
	pick := opticalPickers()["rx"]

	// 全都没有 rx（ontweb 设备）：不画
	if got := sparkPoints([]store.OpticalSample{sample(0, 2.8, 43), sample(0, 2.9, 44)}, pick); got != "" {
		t.Errorf("没有收光数据时不该有线，得到 %q", got)
	}
	// 只有 1 个 rx 点：画不出趋势
	if got := sparkPoints([]store.OpticalSample{sample(-21.5, 0, 0), sample(0, 0, 0)}, pick); got != "" {
		t.Errorf("单点不该画，得到 %q", got)
	}
	// 2 个 rx 点：有线
	if got := sparkPoints([]store.OpticalSample{sample(-21.5, 0, 0), sample(-22.1, 0, 0)}, pick); got == "" {
		t.Error("两个收光点应画出线")
	}
}

// OpticalReadingsFrom：与面板同一套口径（映射表优先、启发式兜底），
// 认不出来就 Empty —— 趋势历史宁缺毋编。
func TestOpticalReadingsFrom(t *testing.T) {
	// 启发式兜底：没登记映射的新机型，按叶子名认方向
	rd := OpticalReadingsFrom(nil, []store.Param{
		{Name: "InternetGatewayDevice.WANDevice.1.X_GponInterafceConfig.RXPower", Value: "-21.50"},
		{Name: "InternetGatewayDevice.WANDevice.1.X_GponInterafceConfig.TXPower", Value: "2.80"},
	})
	if rd.Empty() {
		t.Fatal("启发式应认得出收/发光")
	}
	if rd.Rx == nil || *rd.Rx != -21.5 {
		t.Errorf("rx = %v，期望 -21.5", rd.Rx)
	}
	if rd.Tx == nil || *rd.Tx != 2.8 {
		t.Errorf("tx = %v，期望 2.8", rd.Tx)
	}

	// 映射表：寄存器原始值按规则换算（254 → -15.95 dBm）
	aliases := []store.ParamAlias{{
		Field: "rx_power", MatchContains: "opticaltransceiver.", MatchSuffix: ".rxpower",
		Decode: "dbm_01uw", Priority: 20, Enabled: true,
	}}
	rd = OpticalReadingsFrom(aliases, []store.Param{
		{Name: "InternetGatewayDevice.X_CU_OpticalTransceiver.RXPower", Value: "254"},
	})
	if rd.Rx == nil {
		t.Fatal("映射表应命中")
	}
	if diff := *rd.Rx - (-15.95); diff > 0.01 || diff < -0.01 {
		t.Errorf("换算结果 = %v，期望 ≈ -15.95", *rd.Rx)
	}

	// 无线功率之类的干扰项：不该被认成光功率
	rd = OpticalReadingsFrom(nil, []store.Param{
		{Name: "InternetGatewayDevice.LANDevice.1.WiFi.X_HW_Txpower", Value: "30"},
		{Name: "InternetGatewayDevice.LANDevice.1.WLANConfiguration.1.TransmitPower", Value: "80"},
	})
	if !rd.Empty() {
		t.Errorf("无线发射功率混进来了: %+v", rd)
	}
}

// 详情页：历史里有收光数据时渲染收光趋势图；没有（ontweb 设备）不摆空壳。
func TestDevicePageRendersRxChart(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "lightcat.db"))
	if err != nil {
		t.Fatalf("打开库失败: %v", err)
	}
	defer st.Close()

	devID, _, err := st.UpsertDevice(&store.Device{OUI: "001122", ProductClass: "R", SerialNumber: "RX1"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	f := func(v float64) *float64 { return &v }
	if err := st.InsertOpticalSample(devID, now.Add(-time.Hour), f(-21.5), f(2.8), nil, nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertOpticalSample(devID, now, f(-22.1), f(2.9), nil, nil, nil, ""); err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	if _, err := Register(mux, st, &stubCtrl{}, Options{}); err != nil {
		t.Fatalf("挂路由失败: %v", err)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/devices/"+strconv.FormatInt(devID, 10), nil))
	if w.Code != http.StatusOK {
		t.Fatalf("详情页状态 = %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "收光功率趋势") {
		t.Error("有收光历史时应渲染收光趋势图")
	}
	if !strings.Contains(body, "spark-rx") {
		t.Error("收光曲线应带 spark-rx 样式")
	}
	if !strings.Contains(body, "发光功率趋势") {
		t.Error("发光趋势图不该丢")
	}

	// 只有 tx 没有 rx 的设备（ontweb 形态）：收光图整块不出现
	devID2, _, err := st.UpsertDevice(&store.Device{OUI: "001122", ProductClass: "R", SerialNumber: "TXONLY"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.InsertOpticalSample(devID2, now.Add(-time.Hour), nil, f(2.8), nil, nil, nil, "Up"); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertOpticalSample(devID2, now, nil, f(2.9), nil, nil, nil, "Up"); err != nil {
		t.Fatal(err)
	}
	w2 := httptest.NewRecorder()
	mux.ServeHTTP(w2, httptest.NewRequest("GET", "/devices/"+strconv.FormatInt(devID2, 10), nil))
	if strings.Contains(w2.Body.String(), "收光功率趋势") {
		t.Error("没有收光数据时不该摆收光空壳图")
	}
}
