package web

import (
	"fmt"
	"testing"

	"github.com/zy84338719/lightcat/internal/store"
)

// benchAliases 模拟库里的映射表规模：种子映射 10 条 + Web 适配 5 条。
func benchAliases() []store.ParamAlias {
	mk := func(field, contains, suffix, decode string, prio int) store.ParamAlias {
		return store.ParamAlias{Field: field, MatchContains: contains, MatchSuffix: suffix,
			Decode: decode, Priority: prio, Enabled: true}
	}
	return []store.ParamAlias{
		mk("rx_power", "opticaltransceiver.", ".rxpower", "dbm_01uw", 20),
		mk("tx_power", "opticaltransceiver.", ".txpower", "dbm_01uw", 20),
		mk("temperature", "opticaltransceiver.", ".temperature", "div256", 20),
		mk("voltage", "opticaltransceiver.", ".vcc", "mv01", 20),
		mk("bias_current", "opticaltransceiver.", ".txbias", "ua2", 20),
		mk("rx_power", "x_gponinterafceconfig.", ".rxpower", "identity", 10),
		mk("tx_power", "x_gponinterafceconfig.", ".txpower", "identity", 10),
		mk("temperature", "x_gponinterafceconfig.", ".transceivertemperature", "identity", 10),
		mk("voltage", "x_gponinterafceconfig.", ".supplyvoltage", "mv", 10),
		mk("bias_current", "x_gponinterafceconfig.", ".biascurrent", "identity", 10),
		mk("tx_power", "x_hw_lanopticinterfaceconfig", ".transceiverpower", "dbm_01uw", 10),
		mk("voltage", "x_hw_lanopticinterfaceconfig", ".supplyvoltage", "mv01", 10),
		mk("temperature", "x_hw_lanopticinterfaceconfig", ".temperature", "div256", 10),
		mk("bias_current", "x_hw_lanopticinterfaceconfig", ".biascurrent", "ua2", 10),
		mk("rx_power", "x_hw_lanopticinterfaceconfig", ".receivepower", "dbm_01uw", 10),
	}
}

// benchParams 造一批 GPV 回执规模的参数（默认 200 = 单批上限）。
func benchParams(n int, withOptical bool) []store.Param {
	out := make([]store.Param, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, store.Param{
			Name:  fmt.Sprintf("InternetGatewayDevice.LANDevice.1.X_HW_APDevice.3.SomeLeaf.%d", i),
			Value: "1",
		})
	}
	if withOptical {
		out = append(out,
			store.Param{Name: "InternetGatewayDevice.WANDevice.1.X_GponInterafceConfig.RXPower", Value: "-21"},
			store.Param{Name: "InternetGatewayDevice.WANDevice.1.X_GponInterafceConfig.TXPower", Value: "0"},
		)
	}
	return out
}

// OnParamsStored 钩子在**每批** GPV 回执上跑一次读数辨认（TR-069 趋势历史的入口）。
// 单批上限 200 个参数、映射表 15 条、5 个面板字段 —— 这里量它的真实开销，
// 确保纳管一台 4700 参数设备（约 24 批）时钩子不成为瓶颈（主导成本是 SQLite 写入）。
func BenchmarkOpticalReadingsFrom(b *testing.B) {
	aliases := benchAliases()
	params := benchParams(200, false)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = OpticalReadingsFrom(aliases, params)
	}
}

// 含光口参数的批次（会真的命中映射并解码）。
func BenchmarkOpticalReadingsFromHit(b *testing.B) {
	aliases := benchAliases()
	params := benchParams(200, true)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = OpticalReadingsFrom(aliases, params)
	}
}
