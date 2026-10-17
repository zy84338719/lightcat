package ontweb

import (
	"strings"
	"testing"
)

// TestParseHostsJSON_ValidUnchanged 正常报文必须原样解析出全部终端。
// jsonrepair 对合法 JSON 走 stdlib 快路径，这里守着「引入修复器没有改变正常行为」。
func TestParseHostsJSON_ValidUnchanged(t *testing.T) {
	// 真实报文形态：整段被 \xNN 转义过（hexDecode 会还原）
	raw := `[{"MacAddr":"11:22:33:44:55:66","HostName":"living-room","IpAddr":"192.168.1.10"}]`

	got := parseHostsJSON(raw)
	if len(got) != 1 {
		t.Fatalf("解析出 %d 条，期望 1：%+v", len(got), got)
	}
	if got[0].MAC != "11:22:33:44:55:66" {
		t.Errorf("MAC = %q", got[0].MAC)
	}
	if got[0].HostName != "living-room" {
		t.Errorf("HostName = %q", got[0].HostName)
	}
	if got[0].IP != "192.168.1.10" {
		t.Errorf("IP = %q", got[0].IP)
	}
}

// TestParseHostsJSON_StillHandlesHexEscape 确认改用 jsonrepair 之后，
// 厂商那种整段 \xNN 转义（hexDecode 那条路）没被破坏。
func TestParseHostsJSON_StillHandlesHexEscape(t *testing.T) {
	raw := `[{"MacAddr":"11\x3a22\x3a33\x3a44\x3a55\x3a66","HostName":"hexed"}]`

	got := parseHostsJSON(raw)
	if len(got) != 1 || got[0].MAC != "11:22:33:44:55:66" {
		t.Errorf("\\xNN 转义报文没解析对：%+v", got)
	}
}

// TestParseHostsJSON_RecoversWrappedJSON 报文体被「包了一层」时能救回来。
// 这是引入 jsonrepair 的主要收益：以前这类整份作废，界面上一个终端都不显示。
func TestParseHostsJSON_RecoversWrappedJSON(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"markdown 代码围栏", "```json\n[{\"MacAddr\":\"aa:bb:cc:dd:ee:01\",\"HostName\":\"fenced\"}]\n```"},
		{"尾随逗号", `[{"MacAddr":"aa:bb:cc:dd:ee:01","HostName":"trailing"},]`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseHostsJSON(c.raw)
			if len(got) != 1 {
				t.Fatalf("应救回 1 条，实际 %d 条：%q", len(got), c.raw)
			}
			if got[0].HostName == "" {
				t.Errorf("救回来的记录字段为空：%+v", got[0])
			}
		})
	}
}

// TestParseHostsJSON_StillRejectsGarbage 修不了的东西必须照样返回 nil：
// 修复器不是万能钥匙，对非 JSON 的垃圾输入要保持「拿不到就是拿不到」，
// 不能造出一堆空的 Host 混进界面。
func TestParseHostsJSON_StillRejectsGarbage(t *testing.T) {
	for _, raw := range []string{
		"",
		"会话已过期，请重新登录",
		"<html><body>404</body></html>",
	} {
		if got := parseHostsJSON(raw); got != nil {
			t.Errorf("垃圾输入 %q 却解析出了 %d 条", raw, len(got))
		}
	}
}

// TestParseHostsJSON_TruncatedStaysNil 显式钉住 jsonrepair 的能力边界：
// 截断的数组救不回来，必须返回 nil。
//
// 为什么要有这条：宁可界面上「这次没采到终端」，也不能把解析了一半的数组
// 当成完整结果显示出来 —— 后者会让用户以为设备真的只剩那几台。
// （写这条是因为一开始误以为 jsonrepair 能修截断，实测不能。）
func TestParseHostsJSON_TruncatedStaysNil(t *testing.T) {
	raw := `[{"MacAddr":"aa:bb:cc:dd:ee:01","HostName":"ok1"},{"MacAddr":"aa:bb:cc:dd:ee:02","HostName":"trunc`

	if got := parseHostsJSON(raw); got != nil {
		t.Errorf("截断报文应返回 nil（避免把半份数据当完整结果显示），却得到 %d 条", len(got))
	}
}

// TestParseHostsJSON_SkipsSummaryRow 固件会在末尾附一条没有 MAC 的汇总行，
// 那条要跳过而不是变成一条空终端。
func TestParseHostsJSON_SkipsSummaryRow(t *testing.T) {
	raw := `[{"MacAddr":"11:22:33:44:55:66","HostName":"pc"},` +
		`{"HostName":"总设备数","DevStatus":"2"}]`

	got := parseHostsJSON(raw)
	if len(got) != 1 {
		t.Fatalf("应只保留 1 条有效终端，实际 %d 条：%+v", len(got), got)
	}
	if strings.Contains(got[0].HostName, "总设备数") {
		t.Errorf("汇总行没被跳过：%+v", got[0])
	}
}
