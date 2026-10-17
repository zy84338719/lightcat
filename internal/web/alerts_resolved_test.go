package web

import (
	"testing"

	"github.com/zy84338719/lightcat/internal/alert"
	"github.com/zy84338719/lightcat/internal/store"
)

// 离线告警是**留痕**的：设备后来又报上了，那条记录不会自动消失。
// 原来它和「正在发生的离线」打同一个红徽章，用户扫一眼告警页会以为设备现在
// 还离线着 —— 看着像故障，其实不是。这条测试钉住两者必须可区分。
func TestAlertRowsMarksRecoveredOffline(t *testing.T) {
	alerts := []store.Alert{
		{ID: 1, DeviceID: 7, Kind: alert.KindOffline, Message: "设备判定离线"},
		{ID: 2, DeviceID: 8, Kind: alert.KindRxLow, Message: "收光过低"},
		{ID: 3, DeviceID: 9, Kind: alert.KindOpticDown, Message: "光口异常"},
	}
	// 7 号设备已恢复在线；8/9 不在表里（被删了）
	online := map[int64]bool{7: true}

	rows := alertRows(alerts, online)
	if len(rows) != 3 {
		t.Fatalf("行数 = %d，期望 3", len(rows))
	}
	if rows[0]["Resolved"] != true {
		t.Error("设备已恢复的离线告警应标 Resolved（模板据此显示「已恢复」而不是红徽章）")
	}
	// 非离线类告警不该被标成「已恢复」—— 收光/光口异常没有「恢复」这层语义
	if rows[1]["Resolved"] != false || rows[2]["Resolved"] != false {
		t.Error("只有离线类告警才有 Resolved 语义")
	}

	// 设备仍离线时不标恢复
	rows = alertRows(alerts, map[int64]bool{7: false})
	if rows[0]["Resolved"] != false {
		t.Error("设备仍离线时不该标 Resolved")
	}

	// 设备已从库里删掉：按「已恢复」处理 —— 人都不在了，谈不上还离线
	rows = alertRows(alerts, map[int64]bool{})
	if rows[0]["Resolved"] != true {
		t.Error("设备已删除的离线告警应标 Resolved")
	}
}
