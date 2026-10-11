package cwmp

import (
	"testing"
	"time"
)

// 会话不能被两台设备共用：指纹撞车时（A 设备的会话被 B 复用）必须丢弃，
// 而不是改绑 —— 改绑会让 A 的在途回执按 sess.DeviceID 写进 B 设备名下。
//
// 这是 2026-10-12 发版前审查修掉的洞。触发条件是同一 NAT/CGNAT 出口、同型号
// 同固件的两台设备（BasicAuth 开启时 user 那一维是常量，指纹退化成
// (源IP, User-Agent)）。这类场景实验室里复现不了，所以用单元测试钉住语义。
func TestBindDeviceRefusesToRebind(t *testing.T) {
	m := newSessionManager(time.Minute)

	// 真实撞车形态：**同一个会话**先被 A 绑上，随后 B 的请求复用到它
	// （byKey 指纹相同 → acquire 返回同一个 *Session）。
	s := &Session{ID: "sess-shared", Key: "same-fingerprint"}
	m.registerLocked(s)
	if !m.bindDevice(s, 100) {
		t.Fatal("A 首次绑定应成功")
	}

	// B 的请求带着自己的 deviceID 到来，却拿到了同一个会话
	if m.bindDevice(s, 200) {
		t.Fatal("会话已属于 A（DeviceID=100），不应允许改绑到 B")
	}

	// 关键：会话必须已被丢弃 —— 否则它的 DeviceID 仍指向 A，B 的回执会写进 A 名下
	if s.DeviceID != 0 {
		t.Errorf("被丢弃的会话不应再绑定任何设备，DeviceID = %d", s.DeviceID)
	}
	if _, ok := m.byID["sess-shared"]; ok {
		t.Error("被丢弃的会话应从 byID 移除")
	}
	if _, ok := m.byKey["same-fingerprint"]; ok {
		t.Error("被丢弃的会话应从 byKey 移除")
	}
	if _, ok := m.byDevice[100]; ok {
		t.Error("被丢弃的会话不应残留在 byDevice（否则 hasSession 一直为真）")
	}
}

// 同一设备的第二个会话要顶掉第一个（CPE 一次只跑一个会话，第二个说明旧的已死）。
func TestBindDeviceSupersedesSameDevice(t *testing.T) {
	m := newSessionManager(time.Minute)

	s1 := &Session{ID: "s1", Key: "k1"}
	m.registerLocked(s1)
	m.bindDevice(s1, 100)

	s2 := &Session{ID: "s2", Key: "k2"}
	m.registerLocked(s2)
	if !m.bindDevice(s2, 100) {
		t.Fatal("同一设备的第二个会话应成功绑定")
	}
	if _, ok := m.byID["s1"]; ok {
		t.Error("旧会话应被顶掉")
	}
	if m.byDevice[100] != s2 {
		t.Error("应指向新会话")
	}
}

// 同一设备反复用同一个会话重绑（一次 Inform 触多次请求）应幂等成功，
// 不该把自己丢掉 —— 这是 bindDevice 拒绝改绑后必须保住的正常路径。
func TestBindDeviceIdempotentForSameDevice(t *testing.T) {
	m := newSessionManager(time.Minute)
	s := &Session{ID: "s", Key: "k"}
	m.registerLocked(s)

	for i := 0; i < 3; i++ {
		if !m.bindDevice(s, 100) {
			t.Fatalf("第 %d 次重绑同一设备不该失败", i+1)
		}
	}
	if m.byDevice[100] != s {
		t.Error("重绑后仍应指向该会话")
	}
	if _, ok := m.byID["s"]; !ok {
		t.Error("重绑不该把自己从 byID 摘掉")
	}
}
