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

// RxUnavailable 的判据必须把三件事分开，否则界面又会退回「静默盲区」：
//  1. 认得出 TX、认不到任何 Rx → 设备/通道能力缺失（要显式说「不支持」）
//  2. Rx 有值 → 正常
//  3. 两个都没有 → 这一轮没采到（还是「-」）
func TestHostOpticalRxUnavailable(t *testing.T) {
	cases := []struct {
		name string
		o    HostOptical
		want bool
	}{
		{"认到TX认不到Rx → 不支持", HostOptical{Tx: "-1.25 dBm", TxName: "x.TransceiverPower"}, true},
		{"收光有值 → 不算不支持", HostOptical{Rx: "-21.5 dBm", RxName: "x.RxPower", Tx: "-1.25 dBm", TxName: "x.TransceiverPower"}, false},
		{"只有Rx没Tx → 不算不支持", HostOptical{Rx: "-21.5 dBm", RxName: "x.RxPower"}, false},
		{"两个都没 → 这轮没采到", HostOptical{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.o.RxUnavailable(); got != c.want {
				t.Errorf("RxUnavailable() = %v，期望 %v（%+v）", got, c.want, c.o)
			}
		})
	}
}

// histHasRx：历史里出现过收光就说明这条通道给过，「读不到」只是这一轮的采样缺失。
func TestHistHasRx(t *testing.T) {
	withRx := []store.OpticalSample{{RxDbm: sql.NullFloat64{Float64: -21.5, Valid: true}}}
	without := []store.OpticalSample{{TxDbm: sql.NullFloat64{Float64: -1.25, Valid: true}}}
	if !histHasRx(withRx) {
		t.Error("历史里有收光读数，应返回 true")
	}
	if histHasRx(without) {
		t.Error("历史里只有发光，应返回 false")
	}
	if histHasRx(nil) {
		t.Error("空历史应返回 false")
	}
}

// TestDevicePageSaysRxUnsupported 端到端：华为 V271-20 这类设备（ontweb 通道、
// 固件不给收光）打开详情页，必须看到「本采集通道不提供收光功率读数」而不是
// 一个让人以为一切正常的空白区块。
func TestDevicePageSaysRxUnsupported(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "rx.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	id, _, err := st.UpsertDevice(&store.Device{
		OUI: "485754", ProductClass: "V271-20",
		SerialNumber: "RXCHK", ModelName: "V271-20",
		UserAgent: "lightcat-ontweb",
	})
	if err != nil {
		t.Fatal(err)
	}
	// 照真机：认得出 TX/电压/温度/偏流，压根没有 Rx 参数
	if err := st.UpsertParams(id, []store.Param{
		{Name: "InternetGatewayDevice.LANDevice.1.X_HW_LANOpticInterfaceConfig.1.TransceiverPower", Value: "7499"},
		{Name: "InternetGatewayDevice.LANDevice.1.X_HW_LANOpticInterfaceConfig.1.SupplyVoltage", Value: "31450"},
		{Name: "InternetGatewayDevice.LANDevice.1.X_HW_LANOpticInterfaceConfig.1.TransceiverTemperature", Value: "13056"},
		{Name: "InternetGatewayDevice.LANDevice.1.X_HW_LANOpticInterfaceConfig.1.BiasCurrent", Value: "9500"},
	}, "web"); err != nil {
		t.Fatal(err)
	}
	// 历史也给两条（只有 TX）—— 触发趋势区块渲染
	rx, tx, temp, volt, bias := -21.5, -1.25, 51.0, 3.145, 19.0
	for i := 0; i < 2; i++ {
		if err := st.InsertOpticalSample(id, time.Now().Add(time.Duration(-i)*time.Hour),
			nil, &tx, &temp, &volt, &bias, "Up"); err != nil {
			t.Fatal(err)
		}
	}
	_ = rx

	mux := http.NewServeMux()
	if _, err := Register(mux, st, &stubCtrl{}, Options{}); err != nil {
		t.Fatalf("挂路由失败: %v", err)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/devices/"+strconv.FormatInt(id, 10), nil))
	body := rec.Body.String()

	if !strings.Contains(body, "本采集通道不提供收光功率读数") {
		t.Errorf("详情页没说明「本通道不提供收光」，用户会误以为收光正常")
	}
	if strings.Contains(body, "收光功率趋势") {
		t.Error("不该出现收光趋势图（本来就没数据）")
	}
	// 发光那一侧必须照常显示，不能被这次改动误伤
	if !strings.Contains(body, "发光") {
		t.Error("发光趋势被误伤了")
	}
}
