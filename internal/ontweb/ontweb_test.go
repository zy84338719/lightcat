package ontweb

import (
	"strings"
	"testing"
)

func TestHexDecode(t *testing.T) {
	// 固件把非 ASCII 与引号都转成 \xNN；多字节 UTF-8 按字节序列还原。
	got := hexDecode(`{\x22mac\x22:\x226c\x3a71\x3ad2\x3a1e\x3a36\x3a63\x22}`)
	if got != `{"mac":"6c:71:d2:1e:36:63"}` {
		t.Fatalf("hexDecode 结果不对: %q", got)
	}
	if s := hexDecode("plain"); s != "plain" {
		t.Fatalf("无转义文本不应被改动: %q", s)
	}
}

func TestParseOpticPage(t *testing.T) {
	page := `var opticPower = 'ON'; var opticStatus = 'Up';
	var opticInfos = new Array(new UserOpticInfo("InternetGatewayDevice.LANDevice.1.X_HW_LANOpticInterfaceConfig.1.","7499","31450","13312","9500"));`
	o := parseOpticPage(page)
	if o.Status != "Up" || o.PowerOn != "ON" {
		t.Fatalf("状态解析不对: %+v", o)
	}
	if o.Iface == "" || o.TXRaw != "7499" || o.VoltageRaw != "31450" ||
		o.TempRaw != "13312" || o.BiasRaw != "9500" {
		t.Fatalf("光口原始值解析不对: %+v", o)
	}
}

func TestParseDevicePage(t *testing.T) {
	page := `var deviceInfos = new Array(new stDeviceInfo("InternetGatewayDevice.DeviceInfo","485754439EE1FCB1","3A07\x2eC","V5R023C10S326","V271\x2d20","HWTC","2024\x2d09\x2d22","BC\x3a1E\x3a85\x3aFF\x3a71\x3aBF","OptiXstar V271-20"));`
	d := parseDevicePage(page)
	if d.SN != "485754439EE1FCB1" {
		t.Fatalf("SN 解析不对: %q", d.SN)
	}
	if d.Hardware != "3A07.C" || d.Software != "V5R023C10S326" || d.Model != "V271-20" {
		t.Fatalf("版本/型号解析不对: %+v", d)
	}
	if !strings.HasPrefix(d.MAC, "BC:1E") {
		t.Fatalf("MAC 解析不对: %q", d.MAC)
	}
}

func TestParseWlanPage(t *testing.T) {
	// 截取真机页面结构：SsidNum + 一条 stWlan（域里是 \x2e 转义的点），
	// 另混一条 stWpsPin —— 只有 28 参的 stWlan 该被认出来。
	page := "var SsidNum = '4,4';\n" +
		`WlanArr = new Array(new stWlan("InternetGatewayDevice\x2eLANDevice\x2e1\x2eWLANConfiguration\x2e5","ath4","1","Studio_5G","1","128","1","WPA2","None","None","1","104\x2dbit","TKIPEncryption","PSKAuthentication","AESEncryption","PSKAuthentication","TKIPandAESEncryption","PSKAuthentication","3600","","","1","SMS4","WAPIPSK","","3810"),` +
		`new stWpsPin("x","y","z","w","1"));`
	rows, err := parseWlanPage(page)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("应只认出 1 条 stWlan（stWpsPin 要被滤掉）: %d", len(rows))
	}
	w := rows[0]
	if w.Instance != 5 || w.SSID != "Studio_5G" || w.Enable != "1" {
		t.Fatalf("实例解析不对: %+v", w)
	}
	if w.Band != "5GHz" {
		t.Fatalf("频段划分不对（SsidNum=4,4 → 实例 5 是 5GHz）: %q", w.Band)
	}
	if w.BeaconType != "WPA2" || w.XHWWpaEnc != "TKIPandAESEncryption" {
		t.Fatalf("加密字段解析不对: %+v", w)
	}
}
