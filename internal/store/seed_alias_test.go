package store

import (
	"path/filepath"
	"strings"
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
		key := want.field + "|X_HW_LANOpticInterfaceConfig|" + want.suffix
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
		if a.Field == "rx_power" && strings.EqualFold(a.MatchContains, "X_HW_LANOpticInterfaceConfig") {
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

// TestOntwebAliasNoCaseDuplicate 踩过的坑：迁移 #19 最初把 contains 写成小写
// 'x_hw_lanopticinterface'，与表上已有的规范写法 'X_HW_LANOpticInterfaceConfig'
// 大小写不同 —— 而 UNIQUE(field, match_contains, match_suffix) 对大小写敏感，
// 于是 INSERT OR IGNORE 没去重成，老库上每个字段多出一行（现网就是这么中招的）。
//
// Matches 是大小写不敏感的，所以行为无害，但同一份规则存两份说不清来源，
// 而且 #19 注释里「老库不会重复插入」那句话当时是错的。
//
// 这条测试按**现网的真实状态**复现：迁移 #19 跑完后人为插入那 4 条重复行、
// 把 user_version 停在 19（模拟当时的状态），再重开一次 —— 迁移 #20 必须清掉它们。
func TestOntwebAliasNoCaseDuplicate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dup.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// 复现 bug 态：#19 插出的 4 条小写行已经躺在库里
	for _, r := range []struct{ field, suffix, decode string }{
		{"tx_power", ".transceiverpower", "dbm_01uw"},
		{"temperature", ".transceivertemperature", "div256"},
		{"voltage", ".supplyvoltage", "mv01"},
		{"bias_current", ".biascurrent", "ua2"},
	} {
		if _, err := st.db.Exec(`INSERT INTO param_aliases
			(field, vendor, match_contains, match_suffix, decode, priority, note, updated_at)
			VALUES (?, 'Huawei', 'x_hw_lanopticinterface', ?, ?, 10, '重复行', '')`,
			r.field, r.suffix, r.decode); err != nil {
			t.Fatal(err)
		}
	}
	if n := countAliasFamily(t, st, ""); n != 8 {
		t.Fatalf("构造 bug 态失败：该族应有 8 行（4 规范 + 4 重复），实际 %d", n)
	}
	// 迁移是「按 user_version 只跑一次」的，把版本号退回 19 —— 模拟「跑过 #19、
	// 还没跑 #20」的真实状态（迁移只能追加，不能回头重跑，这是测它的唯一办法）。
	if _, err := st.db.Exec(`PRAGMA user_version = 19`); err != nil {
		t.Fatal(err)
	}
	st.Close()

	// 重开 → 迁移 #20 应该清掉重复行
	st2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()

	if n := countAliasFamily(t, st2, ""); n != 4 {
		t.Errorf("迁移 #20 后该族有 %d 行，期望 4（重复行没清干净）", n)
	}
	if n := countAliasFamily(t, st2, "X_HW_LANOpticInterfaceConfig"); n != 4 {
		t.Errorf("规范写法只剩 %d 条，期望 4（#20 误删了正常的）", n)
	}
}

// countAliasFamily 数该族别名行数；canonical 非空时只数规范写法。
func countAliasFamily(t *testing.T, st *Store, canonical string) int {
	t.Helper()
	q := `SELECT count(*) FROM param_aliases WHERE lower(match_contains) LIKE '%lanopticinterface%'`
	var args []any
	if canonical != "" {
		q += ` AND match_contains = ?`
		args = append(args, canonical)
	}
	var n int
	if err := st.db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
