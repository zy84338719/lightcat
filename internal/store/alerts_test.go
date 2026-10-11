package store

import (
	"database/sql"
	"testing"
	"time"
)

func TestAlertInsertAndList(t *testing.T) {
	st := newTestStore(t)
	devID, _, err := st.UpsertDevice(&Device{OUI: "001122", ProductClass: "R", SerialNumber: "ALERT1", ModelName: "HN8145"})
	if err != nil {
		t.Fatal(err)
	}

	a := &Alert{
		DeviceID: devID, Kind: "rx_low",
		Message:   "收光功率 -28.50 dBm，低于阈值 -27.00 dBm",
		Value:     sql.NullFloat64{Float64: -28.5, Valid: true},
		Threshold: sql.NullFloat64{Float64: -27, Valid: true},
	}
	if err := st.InsertAlert(a); err != nil {
		t.Fatalf("写告警失败: %v", err)
	}
	if a.ID == 0 {
		t.Error("应回填自增 ID")
	}
	if a.CreatedAt.IsZero() {
		t.Error("应默认填当前时间")
	}

	list, err := st.ListAlerts(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("应有 1 条，得到 %d", len(list))
	}
	// 联查带出设备信息（列表页不用再逐条查设备）
	if list[0].SerialNumber != "ALERT1" || list[0].ModelName != "HN8145" {
		t.Errorf("联查设备信息不对: %+v", list[0])
	}
	if !list[0].Value.Valid || list[0].Value.Float64 != -28.5 {
		t.Errorf("value 没原样往返: %+v", list[0].Value)
	}
	if list[0].Notified {
		t.Error("新告警不该是已推送")
	}

	// 按设备列出
	byDev, err := st.ListDeviceAlerts(devID, 10)
	if err != nil || len(byDev) != 1 {
		t.Fatalf("按设备列出失败: %d 条, err=%v", len(byDev), err)
	}
	n, err := st.CountAlerts()
	if err != nil || n != 1 {
		t.Fatalf("计数不对: %d, err=%v", n, err)
	}
}

func TestAlertLastAtAndNotify(t *testing.T) {
	st := newTestStore(t)
	devID, _, _ := st.UpsertDevice(&Device{OUI: "001122", ProductClass: "R", SerialNumber: "ALERT2"})

	// 没有告警时：ok=false 而不是报错
	if _, ok, err := st.LastAlertAt(devID, "rx_low"); err != nil || ok {
		t.Fatalf("空库应 ok=false: ok=%v err=%v", ok, err)
	}

	a := &Alert{DeviceID: devID, Kind: "rx_low", Message: "x"}
	old := time.Now().Add(-2 * time.Hour)
	a.CreatedAt = old
	if err := st.InsertAlert(a); err != nil {
		t.Fatal(err)
	}
	at, ok, err := st.LastAlertAt(devID, "rx_low")
	if err != nil || !ok {
		t.Fatalf("取最近告警失败: ok=%v err=%v", ok, err)
	}
	if at.IsZero() || time.Since(at) < time.Hour {
		t.Errorf("时间不对: %v", at)
	}
	// 别的类型查不到（冷却窗口按 设备+类型 算）
	if _, ok, _ := st.LastAlertAt(devID, "temp_high"); ok {
		t.Error("temp_high 不该有记录")
	}

	if err := st.MarkAlertNotified(a.ID); err != nil {
		t.Fatal(err)
	}
	list, _ := st.ListAlerts(10)
	if !list[0].Notified {
		t.Error("标记已推送没生效")
	}
}

// 告警表有全局保留条数：超了裁最老的（防止跑几年后无限增长）。
func TestAlertPrunesBeyondLimit(t *testing.T) {
	st := newTestStore(t)
	devID, _, _ := st.UpsertDevice(&Device{OUI: "001122", ProductClass: "R", SerialNumber: "ALERT3"})
	for i := 0; i < alertHistoryLimit+5; i++ {
		if err := st.InsertAlert(&Alert{DeviceID: devID, Kind: "offline", Message: "m"}); err != nil {
			t.Fatal(err)
		}
	}
	n, _ := st.CountAlerts()
	if n > alertHistoryLimit {
		t.Errorf("应裁到 %d 条以内，得到 %d", alertHistoryLimit, n)
	}
	// 裁的是最老的：最新一条还在
	list, _ := st.ListAlerts(1)
	if len(list) != 1 {
		t.Fatal("最新告警不该被裁掉")
	}
}
