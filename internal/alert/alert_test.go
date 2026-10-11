package alert

import (
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/zy84338719/lightcat/internal/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func testDevice(t *testing.T, st *store.Store, serial string) int64 {
	t.Helper()
	id, _, err := st.UpsertDevice(&store.Device{OUI: "001122", ProductClass: "R", SerialNumber: serial, ModelName: "T1"})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func nullLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func rx(v float64) store.OpticalSample {
	return store.OpticalSample{RxDbm: sql.NullFloat64{Float64: v, Valid: true}}
}

// webhook 地址必须是 http/https 且带主机名：配置错误在启动时就炸。
func TestNewRejectsBadWebhook(t *testing.T) {
	st := newTestStore(t)
	for _, bad := range []string{"ftp://example.com/hook", "不是地址", "http://", "://x"} {
		if _, err := New(st, nullLogger(), bad, Thresholds{}, time.Hour); err == nil {
			t.Errorf("%q 应被拒绝", bad)
		}
	}
	if _, err := New(st, nullLogger(), "", Thresholds{}, time.Hour); err != nil {
		t.Errorf("空 webhook（只记录不推送）应该合法: %v", err)
	}
	if _, err := New(st, nullLogger(), "https://example.com/hook", Thresholds{}, time.Hour); err != nil {
		t.Errorf("正常 https 地址应通过: %v", err)
	}
}

// 收光低于阈值：落库 + webhook 收到 JSON + 标记已推送。
func TestCheckOpticalRxLowFiresAndNotifies(t *testing.T) {
	st := newTestStore(t)
	devID := testDevice(t, st, "RXLOW")

	type recv struct {
		body webhookPayload
	}
	got := make(chan recv, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("应 POST，得到 %s", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q", ct)
		}
		var p webhookPayload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Errorf("解析推送体失败: %v", err)
		}
		got <- recv{p}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m, err := New(st, nullLogger(), srv.URL, Thresholds{RxLowDbm: -27}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	m.CheckOptical(devID, rx(-28.5))

	select {
	case r := <-got:
		if r.body.Kind != KindRxLow || r.body.Source != "lightcat" {
			t.Errorf("推送内容不对: %+v", r.body)
		}
		if r.body.Value == nil || *r.body.Value != -28.5 {
			t.Errorf("推送里没带读数: %+v", r.body.Value)
		}
		if r.body.Threshold == nil || *r.body.Threshold != -27 {
			t.Errorf("推送里没带阈值: %+v", r.body.Threshold)
		}
		if r.body.Serial != "RXLOW" {
			t.Errorf("推送里应带设备序列号: %+v", r.body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("webhook 没收到推送")
	}

	// 落库并标记已推送（notify 是异步的，等一下）
	deadline := time.Now().Add(3 * time.Second)
	var list []store.Alert
	for time.Now().Before(deadline) {
		list, _ = st.ListAlerts(10)
		if len(list) == 1 && list[0].Notified {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(list) != 1 {
		t.Fatalf("应有 1 条告警，得到 %d", len(list))
	}
	if list[0].Kind != KindRxLow || !list[0].Notified {
		t.Errorf("告警记录不对: %+v", list[0])
	}
}

// 正常读数不告警；规则关闭（阈值 0）时越限也不告警。
func TestCheckOpticalNormalAndDisabled(t *testing.T) {
	st := newTestStore(t)
	devID := testDevice(t, st, "OK1")
	m, _ := New(st, nullLogger(), "", Thresholds{RxLowDbm: -27}, time.Hour)
	m.CheckOptical(devID, rx(-21.5))
	if n, _ := st.CountAlerts(); n != 0 {
		t.Errorf("正常收光不该告警，得到 %d 条", n)
	}

	// 收光缺失（NULL）不告警
	m.CheckOptical(devID, store.OpticalSample{})

	m2, _ := New(st, nullLogger(), "", Thresholds{}, time.Hour) // 全关
	m2.CheckOptical(devID, rx(-40))
	m2.DeviceOffline(devID)
	if n, _ := st.CountAlerts(); n != 0 {
		t.Errorf("规则全关不该告警，得到 %d 条", n)
	}
	if m2.Enabled() {
		t.Error("全关时 Enabled 应为 false")
	}
}

// 冷却窗口：持续越限不轰炸；窗口过了才允许下一条。
func TestCooldownSuppressesRepeat(t *testing.T) {
	st := newTestStore(t)
	devID := testDevice(t, st, "COOL1")

	// 长窗口：窗口内的重复越限全被吞掉
	m, _ := New(st, nullLogger(), "", Thresholds{RxLowDbm: -27}, time.Hour)
	m.CheckOptical(devID, rx(-28))
	m.CheckOptical(devID, rx(-29))
	m.CheckOptical(devID, rx(-30))
	if n, _ := st.CountAlerts(); n != 1 {
		t.Fatalf("冷却窗口内应只有 1 条，得到 %d", n)
	}

	// 短窗口：等它自然过期后，再次越限要能记新的一条
	m2, _ := New(st, nullLogger(), "", Thresholds{RxLowDbm: -27}, 50*time.Millisecond)
	time.Sleep(80 * time.Millisecond)
	m2.CheckOptical(devID, rx(-31))
	if n, _ := st.CountAlerts(); n != 2 {
		t.Fatalf("窗口过后应记新告警，得到 %d 条", n)
	}
}

// 过温与光口状态规则。
func TestTempHighAndOpticDown(t *testing.T) {
	st := newTestStore(t)
	devID := testDevice(t, st, "TEMP1")
	m, _ := New(st, nullLogger(), "", Thresholds{TempHighC: 70}, time.Hour)

	s := store.OpticalSample{
		TempC:  sql.NullFloat64{Float64: 81.5, Valid: true},
		Status: "Down",
	}
	m.CheckOptical(devID, s)
	list, _ := st.ListAlerts(10)
	kinds := map[string]bool{}
	for _, a := range list {
		kinds[a.Kind] = true
	}
	if !kinds[KindTempHigh] {
		t.Error("应有过温告警")
	}
	if !kinds[KindOpticDown] {
		t.Error("光口 Down 应有告警")
	}

	// Status=Up 不算异常
	st2 := newTestStore(t)
	devID2 := testDevice(t, st2, "UP1")
	m2, _ := New(st2, nullLogger(), "", Thresholds{}, time.Hour)
	m2.CheckOptical(devID2, store.OpticalSample{Status: "Up"})
	if n, _ := st2.CountAlerts(); n != 0 {
		t.Errorf("Up 不该告警，得到 %d 条", n)
	}
}

// 离线告警：开关有效、冷却有效。
func TestDeviceOffline(t *testing.T) {
	st := newTestStore(t)
	devID := testDevice(t, st, "OFF1")
	m, _ := New(st, nullLogger(), "", Thresholds{Offline: true}, time.Hour)
	m.DeviceOffline(devID)
	m.DeviceOffline(devID) // 冷却内
	if n, _ := st.CountAlerts(); n != 1 {
		t.Fatalf("离线告警应 1 条（冷却吞掉重复），得到 %d", n)
	}
	list, _ := st.ListAlerts(1)
	if list[0].Kind != KindOffline {
		t.Errorf("类型不对: %+v", list[0])
	}
}

// webhook 推送失败：告警仍在库里（面板可查），notified 保持 0，不重试不崩。
func TestWebhookFailureKeepsRecord(t *testing.T) {
	st := newTestStore(t)
	devID := testDevice(t, st, "FAIL1")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	m, _ := New(st, nullLogger(), srv.URL, Thresholds{Offline: true}, time.Hour)
	m.DeviceOffline(devID)
	srv.Close() // 后续推送直接连不上也不该崩

	// 等异步推送落地（失败路径也会走完 goroutine）
	time.Sleep(200 * time.Millisecond)
	list, _ := st.ListAlerts(10)
	if len(list) != 1 {
		t.Fatalf("推送失败也该留下记录，得到 %d 条", len(list))
	}
	if list[0].Notified {
		t.Error("推送失败不该标记已推送")
	}
}
