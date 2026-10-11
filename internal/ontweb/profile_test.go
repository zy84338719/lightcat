package ontweb

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 配方选择：按设备自报的型号/固件匹配；对不上就退回默认配方（不猜）。
func TestPickProfile(t *testing.T) {
	if p := pickProfile(DeviceInfo{Model: "V271-20", Software: "V5R023C10S200"}); p != defaultProfile {
		t.Errorf("华为 V271 应命中华为配方，得到 %s", p.Name)
	}
	if p := pickProfile(DeviceInfo{Model: "HN8145X6N"}); p != defaultProfile {
		t.Errorf("HN8145 应命中华为配方，得到 %s", p.Name)
	}
	// 完全对不上的机型：退回默认配方（解析失败时错误信息会带型号/固件）
	if p := pickProfile(DeviceInfo{Model: "ZXHN F670", Software: "V9.0.10P1"}); p != defaultProfile {
		t.Errorf("未知机型应退回默认配方，得到 %s", p.Name)
	}
}

// fakeONT 是一台「页面结构对不上配方」的假光猫：登录能成，设备信息正常，
// 其余页面全是登录页 HTML（模拟固件改版后的样子）。
func fakeONT(t *testing.T, devicePage string) *httptest.Server {
	t.Helper()
	loginPage := `<html><body>请先登录</body></html>`
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/asp/GetRandCount.asp":
			w.Write([]byte("\ufeffabc123def"))
		case "/login.cgi":
			w.Write([]byte("<script>top.location.replace('/index.asp')</script>"))
		case "/html/ssmp/deviceinfo/deviceinfo.asp":
			w.Write([]byte(devicePage))
		default:
			w.Write([]byte(loginPage))
		}
	}))
}

const goodDevicePage = `var deviceInfos = new Array(new stDeviceInfo("InternetGatewayDevice.DeviceInfo","485754439EE1FCB1","3A07.C","V5R023C10S326","V271-20","HWTC","2024-09-22","BC:1E:85:FF:71:BF","OptiXstar V271-20"));
var cpuUsed = '17%'; var memUsed = '64%';`

// 部分页面对不上：采集不该整体报废 —— 采到多少算多少，缺的进 Warnings 说清楚。
func TestSnapshotSoftDegradation(t *testing.T) {
	srv := fakeONT(t, goodDevicePage)
	defer srv.Close()

	cl, err := New(srv.URL, "user", "pass")
	if err != nil {
		t.Fatal(err)
	}
	snap, err := cl.Snapshot()
	if err != nil {
		t.Fatalf("设备信息正常时采集不该整体失败: %v", err)
	}
	if snap.Device.SN != "485754439EE1FCB1" {
		t.Errorf("设备信息应采到: %+v", snap.Device)
	}
	if snap.Profile == "" {
		t.Error("应记录本次使用的配方名")
	}
	joined := strings.Join(snap.Warnings, "\n")
	for _, want := range []string{"光口信息页", "无线配置页", "onttoken"} {
		if !strings.Contains(joined, want) {
			t.Errorf("Warnings 里应说明 %q 解析失败，实际：%s", want, joined)
		}
	}
	// 每条软诊断都该指到适配入口，而不是干巴巴一句失败
	if !strings.Contains(joined, "配方") {
		t.Errorf("Warnings 应带上当前配方名便于适配：%s", joined)
	}
}

// 设备信息页都对不上：硬失败，错误里带检测到的型号/固件与适配入口。
func TestSnapshotHardFailInforms(t *testing.T) {
	srv := fakeONT(t, `<html><body>firmware v2 totally different layout</body></html>`)
	defer srv.Close()

	cl, err := New(srv.URL, "user", "pass")
	if err != nil {
		t.Fatal(err)
	}
	_, err = cl.Snapshot()
	if err == nil {
		t.Fatal("解析不出序列号应报错")
	}
	msg := err.Error()
	for _, want := range []string{"序列号", "profile.go", "型号"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误信息应包含 %q，实际：%s", want, msg)
		}
	}
}

// 登录握手对不上（token 拿不到）：错误要说清是配方问题还是设备不在线。
func TestLoginTokenFailureInforms(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("\ufeff")) // token 为空
	}))
	defer srv.Close()

	cl, err := New(srv.URL, "user", "pass")
	if err != nil {
		t.Fatal(err)
	}
	err = cl.Login()
	if err == nil {
		t.Fatal("空 token 应报错")
	}
	if !strings.Contains(err.Error(), "profile.go") || !strings.Contains(err.Error(), "配方") {
		t.Errorf("错误应指向配方适配入口，实际：%v", err)
	}
}
