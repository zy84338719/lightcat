package store

import (
	"path/filepath"
	"testing"
)

// TestSeedCoversOntwebOpticParams 钉住迁移 #19。
//
// 之前种子（迁移 #6）只覆盖 **TR-069 通道**在真机上对出来的那两族
// （OpticalTransceiver.* / X_GponInterafceConfig.*），而 ontweb（光猫 Web 普通账号）
// 通道写出来的参数是 TR-098 形状的 X_HW_LANOpticInterfaceConfig.1.*——
// 那一族一条规则都没有。后果是新装的机器纳管华为 V271-20 时，收/发光/温度/
// 电压/偏流在参数驱动的视图里全认不出来；现网的库是手工补过的，只有新装机器受影响，
// 所以更不容易被发现。
func TestSeedCoversOntwebOpticParams(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "seed.db"))
	if err != nil {
		t.Fatalf("打开库失败: %v", err)
	}
	defer st.Close()

	als, err := st.EnabledAliases()
	if err != nil {
		t.Fatalf("读别名失败: %v", err)
	}
	have := map[string]bool{}
	for _, a := range als {
		have[a.Field+"|"+a.MatchContains+"|"+a.MatchSuffix] = true
	}
	for _, want := range []struct{ field, suffix, decode string }{
		{"tx_power", ".transceiverpower", "dbm_01uw"},
		{"temperature", ".transceivertemperature", "div256"},
		{"voltage", ".supplyvoltage", "mv01"},
		{"bias_current", ".biascurrent", "ua2"},
	} {
		key := want.field + "|x_hw_lanopticinterface|" + want.suffix
		if !have[key] {
			t.Errorf("种子缺 %s 的规则（contains=x_hw_lanopticinterface suffix=%s）", want.field, want.suffix)
		}
	}
}

// 刻意**不加** rx_power：华为 V271-20 的 UserOpticInfo 只给 TX/电压/温度/偏流，
// 固件根本不提供上行收光。加一条永远命不中的规则只会让人以为「配了就能读到」。
// 收光缺失要显式告知（web 侧 HostOptical.RxUnavailable），不是靠一条假规则糊过去。
func TestSeedDoesNotFakeRxPower(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "seed2.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	als, err := st.EnabledAliases()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range als {
		if a.Field == "rx_power" && a.MatchContains == "x_hw_lanopticinterface" {
			t.Error("不该给 X_HW_LANOpticInterface 族加 rx_power 规则：该固件不提供收光")
		}
	}
}

// 老库已经手工补过这 4 条时，迁移必须幂等（不能插出重复行）。
func TestOntwebAliasMigrationIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "idem.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := st.EnabledAliases()
	st.Close()

	// 重开一次：迁移已到 user_version 最高，不该重复应用
	st2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	second, _ := st2.EnabledAliases()
	if len(first) != len(second) {
		t.Errorf("重开后别名数从 %d 变成 %d，迁移不幂等", len(first), len(second))
	}
}
