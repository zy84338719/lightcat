package web

import "testing"

// 监听地址校验：这是唯一挡住「面板被绑到意外地址」的一道关。
// 归一化不对的后果是静默的 —— 设成 0.0.0.0 或某个外部 IP，面板就暴露到局域网外。
// 这条函数此前**没有任何测试**，而它决定监听地址。
func TestNormalizeListen(t *testing.T) {
	ok := []struct{ in, want string }{
		{"9090", ":9090"},
		{":9090", ":9090"},
		{"0.0.0.0:9090", "0.0.0.0:9090"},
		{"127.0.0.1:9090", "127.0.0.1:9090"},
		{"localhost:9090", "localhost:9090"},
		{"[::1]:9090", "[::1]:9090"},
		{"", ""}, // 空 = 与 ACS 同端口（调用方判断）
	}
	for _, c := range ok {
		got, err := normalizeListen(c.in)
		if err != nil {
			t.Errorf("normalizeListen(%q) 报错: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("normalizeListen(%q) = %q, 想要 %q", c.in, got, c.want)
		}
	}

	bad := []struct{ in, why string }{
		{"abc", "端口不是数字"},
		{"99999", "端口越界"},
		{"0", "端口为 0"},
		{"example.com:80", "主机必须是 IP（域名会跟着 DNS 漂）"},
		{"256.1.1.1:80", "非法 IP"},
		{"127.0.0.1", "缺端口"},
	}
	for _, c := range bad {
		if got, err := normalizeListen(c.in); err == nil {
			t.Errorf("normalizeListen(%q) 应报错，却返回 %q（%s）", c.in, got, c.why)
		}
	}
}
