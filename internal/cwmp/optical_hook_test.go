package cwmp

import (
	"database/sql"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zy84338719/lightcat/internal/store"

	_ "modernc.org/sqlite"
)

// gpvResponseEnv 造一条 GetParameterValuesResponse 信封（形态照真机样本）。
func gpvResponseEnv(t *testing.T, params [][2]string) *Envelope {
	t.Helper()
	var sb strings.Builder
	sb.WriteString(`<SOAP-ENV:Envelope xmlns:SOAP-ENV="http://schemas.xmlsoap.org/soap/envelope/"` +
		` xmlns:SOAP-ENC="http://schemas.xmlsoap.org/soap/encoding/"` +
		` xmlns:xsd="http://www.w3.org/2001/XMLSchema" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"` +
		` xmlns:cwmp="urn:dslforum-org:cwmp-1-0">` +
		`<SOAP-ENV:Header><cwmp:ID SOAP-ENV:mustUnderstand="1">test1</cwmp:ID></SOAP-ENV:Header>` +
		`<SOAP-ENV:Body><cwmp:GetParameterValuesResponse>` +
		`<ParameterList SOAP-ENC:arrayType="cwmp:ParameterValueStruct[2]">`)
	for _, p := range params {
		sb.WriteString(`<ParameterValueStruct><Name>` + p[0] + `</Name>` +
			`<Value xsi:type="xsd:string">` + p[1] + `</Value></ParameterValueStruct>`)
	}
	sb.WriteString(`</ParameterList></cwmp:GetParameterValuesResponse></SOAP-ENV:Body></SOAP-ENV:Envelope>`)
	root, err := ParseXML(strings.NewReader(sb.String()))
	if err != nil {
		t.Fatalf("解析测试信封失败: %v", err)
	}
	env, err := ParseEnvelope(root)
	if err != nil {
		t.Fatalf("解析 Envelope 失败: %v", err)
	}
	if env.Method == nil || env.Method.Local != "GetParameterValuesResponse" {
		t.Fatalf("信封里没有 GPV 回执方法: %+v", env.Method)
	}
	return env
}

func newHookSession(devID int64) *Session {
	return &Session{ID: "hook-test", DeviceID: devID, lock: make(chan struct{}, 1)}
}

// GPV 回执入库后必须触发 OnParamsStored（趋势历史靠它接线）。
func TestOnParamsStoredFiresAfterGPV(t *testing.T) {
	srv, _, devID := newTestServer(t, Config{})

	var gotDevice int64
	var gotParams []store.Param
	srv.OnParamsStored = func(deviceID int64, params []store.Param) {
		gotDevice = deviceID
		gotParams = params
	}

	env := gpvResponseEnv(t, [][2]string{
		{"InternetGatewayDevice.WANDevice.1.X_GponInterafceConfig.RXPower", "-21.5"},
		{"InternetGatewayDevice.WANDevice.1.X_GponInterafceConfig.TXPower", "2.8"},
	})
	w := httptest.NewRecorder()
	srv.onGetParameterValuesResponse(w, newHookSession(devID), env, env.Method)

	if gotDevice != devID {
		t.Fatalf("钩子拿到的设备 = %d，期望 %d", gotDevice, devID)
	}
	if len(gotParams) != 2 {
		t.Fatalf("钩子拿到 %d 个参数，期望 2", len(gotParams))
	}
	if !strings.HasSuffix(gotParams[0].Name, "RXPower") || gotParams[0].Value != "-21.5" {
		t.Errorf("参数没原样传进钩子: %+v", gotParams[0])
	}
	// 参数确实入库了（钩子不替代入库）
	if p, ok, _ := srv.store.GetParam(devID, "InternetGatewayDevice.WANDevice.1.X_GponInterafceConfig.RXPower"); !ok || p.Value != "-21.5" {
		t.Errorf("参数没入库: %+v ok=%v", p, ok)
	}
}

// 钩子 panic 必须被兜住：旁路逻辑再错也不能把设备会话打挂（NFR-9）。
func TestOnParamsStoredPanicContained(t *testing.T) {
	srv, _, devID := newTestServer(t, Config{})
	srv.OnParamsStored = func(deviceID int64, params []store.Param) {
		panic("钩子里炸了")
	}

	env := gpvResponseEnv(t, [][2]string{{"InternetGatewayDevice.DeviceInfo.ModelName", "X"}})
	w := httptest.NewRecorder()
	srv.onGetParameterValuesResponse(w, newHookSession(devID), env, env.Method)

	// 会话没被打挂：参数照样入库、响应照样写出（无任务时 204 结束会话）
	if p, ok, _ := srv.store.GetParam(devID, "InternetGatewayDevice.DeviceInfo.ModelName"); !ok || p.Value != "X" {
		t.Errorf("panic 后参数仍应入库: %+v ok=%v", p, ok)
	}
	if w.Code != 204 && w.Code != 200 {
		t.Errorf("panic 后会话应正常收尾，得到状态 %d", w.Code)
	}
}

// 离线钩子：SweepOffline 判定离线的那一刻要触发（告警靠它接线）。
func TestOnDeviceOfflineFires(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	devID, _, err := st.UpsertDevice(&store.Device{OUI: "001122", ProductClass: "R", SerialNumber: "S1"})
	if err != nil {
		t.Fatalf("建测试设备失败: %v", err)
	}
	// 把最后上报时间改老（store 没有公开改法，测试直接开同一个库文件改）
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("开裸库失败: %v", err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`UPDATE devices SET last_inform_at = '2020-01-01T00:00:00Z', periodic_interval = 0 WHERE id = ?`, devID); err != nil {
		t.Fatalf("造旧上报时间失败: %v", err)
	}

	srv := NewServer(st, Config{
		OfflineProbe: false,            // 纯超时判定
		OfflineAfter: time.Millisecond, // 一秒都不等
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	var got int64
	srv.OnDeviceOffline = func(deviceID int64) { got = deviceID }
	srv.SweepOffline(t.Context())
	if got != devID {
		t.Fatalf("离线钩子拿到的设备 = %d，期望 %d", got, devID)
	}
}
