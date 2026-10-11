package i18n

import (
	"strings"
	"testing"
)

// 后端拼出来的字符串（任务结果、提示语、分组名）靠 TSmart 翻译：
// 先精确查表，再用占位符匹配，最后逐段替换。这里把几种典型形态钉住。
func TestTSmart(t *testing.T) {
	cases := map[string]string{
		"已采集 218 个参数": "Collected 218 parameter(s)",
		"设置成功":        "Set successfully",
		"设置成功；2 个参数设备未回读，无法核对：X：读回里没有这个参数": "Set successfully; 2 parameter(s) were not read back: X: not present in the read-back",
		"诊断进行中…":                        "Diagnostics in progress…",
		"子机 1（K251-20）":                 "Sub-device 1 (K251-20)",
		"主机 · SimWiFi · 2.4G":           "Host · SimWiFi · 2.4G",
		"在线 1 小时 0 分":                   "online 1h 0m",
		"信号 100%（4/4 格） · RSSI -41 dBm": "signal 100% (4/4 bars) · RSSI -41 dBm",
		"设备上报：repeater · 上行 DHCP":       "Device reports: repeater · uplink DHCP",
		"已删除「客厅光猫」（只删本地记录：参数 / 任务 / 上报历史）。设备若还配着本 ACS 地址，下次上报会重新纳管。": "Deleted “客厅光猫” (local records only: parameters / tasks / inform history). If it still points at this ACS it is onboarded again on the next report.",
		// 中文界面原样返回；认不出的也原样返回（宁可中文，也别显示错的东西）
		"已采集 3 个参数": "Collected 3 parameter(s)",
		"这句还没翻译":    "这句还没翻译",
	}
	for in, want := range cases {
		if got := TSmart(LangEN, in); got != want {
			t.Errorf("TSmart(%q) = %q，期望 %q", in, got, want)
		}
	}
	if got := TSmart(LangZH, "已采集 3 个参数"); got != "已采集 3 个参数" {
		t.Errorf("中文界面应原样返回，得到 %q", got)
	}
}

// 嵌套括号：无线组网分组名里的信号描述带了自己的括号。
//
// 根因在词条编译：`无线组网（%s）` 编译成 `无线组网（(.+?)）$`，非贪婪捕获遇到
// 内层的「（3/4 格）」会**提前闭合** —— 实测捕获到的是「75%（3/4 格）」（连「信号」
// 都没了），译文里于是残留中文。正则表达不出括号配平，只能在 tsmart 里手工切段。
//
// 这条一直挂在 docs/notes/i18n.md 的「已知边角」里（真机上表现为无线弹窗的组网方式
// 一栏夹着半句中文），修的时候试过几个位置都出问题：
//   - 切分放在整串匹配**之前** → 抢走「无线组网（信号 75%）」这类本来能整串命中的，
//     只翻一半；「信号 100%（4/4 格） · RSSI」也被拆坏；
//   - 放在逐段替换**之后** → 逐段替换会顺手把全角括号换成半角，之后配平就找不到括号了。
//
// 最终形态：整串命中后**先看译文里还有没有汉字**，有就退回按配平切段重来。
func TestTSmartNestedParens(t *testing.T) {
	cases := []struct{ in, want string }{
		{"无线组网（信号 75%（3/4 格））", "Wi-Fi backhaul（signal 75% (3/4 bars)）"},
		{"无线组网（信号 75%）", "Wi-Fi backhaul (signal 75%)"},
		{"无线组网（信号 75%（4/4 格）· RSSI -41 dBm）", "Wi-Fi backhaul（signal 75% (4/4 bars)· RSSI -41 dBm）"},
	}
	for _, c := range cases {
		got := TSmart(LangEN, c.in)
		if got != c.want {
			t.Errorf("TSmart(%q)\n  得到 %q\n  期望 %q", c.in, got, c.want)
		}
		if hasHan(got) {
			t.Errorf("TSmart(%q) 结果里还留着汉字: %q", c.in, got)
		}
	}
}

// 加了括号切分之后，**原来能翻的必须还翻得对** —— 那条路（整串 / 逐段替换）是主力，
// 切分只是兜底，抢了主力就会把好好的句子弄坏。这几条就是防抢跑的。
func TestTSmartNoRegression(t *testing.T) {
	cases := []struct{ in, want string }{
		{"信号 100%（4/4 格） · RSSI -41 dBm", "signal 100% (4/4 bars) · RSSI -41 dBm"},
		{"在线 1 小时 14 分", "online 1h 14m"},
		{"主机 · SimWiFi · 2.4G", "Host · SimWiFi · 2.4G"},
		{"信号 75%（3/4 格）", "signal 75% (3/4 bars)"},
		{"已保存。", "Saved."},
	}
	for _, c := range cases {
		if got := TSmart(LangEN, c.in); got != c.want {
			t.Errorf("TSmart(%q) = %q, 期望 %q", c.in, got, c.want)
		}
	}
}

// 括号没配平时不能乱切（切出半截反而更糟）。
func TestTSmartUnbalancedParens(t *testing.T) {
	for _, s := range []string{"无线组网（信号 75%", "信号 75%（3/4 格）", "（", "）"} {
		got := TSmart(LangEN, s)
		// 不要求翻出来（可能本来就没词条），但不能 panic、不能吞字
		if len([]rune(got)) == 0 && len([]rune(s)) > 0 {
			t.Errorf("TSmart(%q) 返回空串", s)
		}
	}
	// 未配平的那句：切不动，但**能翻的部分照样要翻**（「无线组网」在词条里）。
	// 断言的是「不吞字、不 panic」，不是「必须原样返回」—— 那是强加的期望。
	if got := TSmart(LangEN, "无线组网（信号 75%"); !strings.Contains(got, "75%") {
		t.Errorf("未配平括号时丢了内容: %q", got)
	}
}
