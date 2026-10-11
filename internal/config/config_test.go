package config

import "testing"

// TLS 要么都配、要么都不配：只配一半是笔误，启动时就要拦住。
func TestTLSPairing(t *testing.T) {
	if _, err := Load([]string{"-tls-cert", "/x/cert.pem"}); err == nil {
		t.Error("只配证书应报错")
	}
	if _, err := Load([]string{"-tls-key", "/x/key.pem"}); err == nil {
		t.Error("只配私钥应报错")
	}
	c, err := Load([]string{"-tls-cert", "/x/cert.pem", "-tls-key", "/x/key.pem"})
	if err != nil {
		t.Fatalf("成对配置不该报错: %v", err)
	}
	if !c.TLSEnabled() {
		t.Error("成对配置应判定为启用 TLS")
	}
	c2, err := Load(nil)
	if err != nil {
		t.Fatal(err)
	}
	if c2.TLSEnabled() {
		t.Error("默认不该启用 TLS")
	}
	// 环境变量同样生效
	t.Setenv("ACS_TLS_CERT", "/e/cert.pem")
	t.Setenv("ACS_TLS_KEY", "/e/key.pem")
	c3, err := Load(nil)
	if err != nil || !c3.TLSEnabled() {
		t.Errorf("环境变量应启用 TLS: %v err=%v", c3, err)
	}
}

// 新增运维参数的默认值（回归：别让默认悄悄变掉）。
func TestDefaults(t *testing.T) {
	c, err := Load(nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.OpticalHistoryDays != 30 {
		t.Errorf("光功率历史默认保留 30 天，得到 %d", c.OpticalHistoryDays)
	}
	if c.FilesDir != "files" {
		t.Errorf("文件库目录默认 files，得到 %q", c.FilesDir)
	}
	if c.AlertRxLow != -27 || c.AlertTempHigh != 70 || !c.AlertOffline {
		t.Errorf("告警阈值默认不对: rx=%v temp=%v offline=%v", c.AlertRxLow, c.AlertTempHigh, c.AlertOffline)
	}
	if c.AlertCooldown.Hours() != 6 {
		t.Errorf("告警冷却默认 6h，得到 %v", c.AlertCooldown)
	}
	if c.AlertWebhook != "" {
		t.Errorf("webhook 默认应为空（只记录不推送），得到 %q", c.AlertWebhook)
	}
}
