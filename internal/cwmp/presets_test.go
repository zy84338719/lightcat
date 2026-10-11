package cwmp

import (
	"encoding/json"
	"testing"

	"github.com/zy84338719/lightcat/internal/store"
)

func mkPreset(name string, events []string, newDevice bool, actions []store.PresetAction) *store.Preset {
	return &store.Preset{
		Name: name, Enabled: true,
		Events: events, NewDevice: newDevice,
		Actions: actions, OncePerDevice: true,
	}
}

func spvAction(name, value, typ string) store.PresetAction {
	return store.PresetAction{Kind: store.PresetActionSet,
		Values: []store.PresetValue{{Name: name, Value: value, Type: typ}}}
}

func gpvAction(names ...string) store.PresetAction {
	return store.PresetAction{Kind: store.PresetActionGet, Names: names}
}

// pendingKinds 按**执行顺序**（ID 升序）返回待办任务的类型。
// 注意 ListTasks 是倒序（最近在前），这里要的是队列语义。
func pendingKinds(t *testing.T, st *store.Store, devID int64) []string {
	t.Helper()
	tasks, err := st.ListTasks(devID, 50)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for i := len(tasks) - 1; i >= 0; i-- {
		if tasks[i].Status == store.TaskPending {
			kinds = append(kinds, tasks[i].Kind)
		}
	}
	return kinds
}

// drainPending 清空一台设备的待办队列（条件用例之间互不干扰：
// GetParameterValues 任务有「同设备同类型未结就跳过」的去重，不清会挡住后面的用例）。
func drainPending(t *testing.T, st *store.Store, devID int64) {
	t.Helper()
	for {
		tk, err := st.ClaimNextTask(devID, nil)
		if err != nil || tk == nil {
			return
		}
		_ = st.CompleteTask(tk.ID, "测试清场")
	}
}

// 事件命中：动作按序入队（同一会话里就会依次下发）。
func TestEvalPresetsEventTrigger(t *testing.T) {
	srv, st, devID := newTestServer(t, Config{})
	if _, err := st.CreatePreset(mkPreset("开机采集", []string{"1 BOOT"}, false,
		[]store.PresetAction{
			spvAction("InternetGatewayDevice.ManagementServer.PeriodicInformInterval", "300", "unsignedInt"),
			gpvAction("InternetGatewayDevice.DeviceInfo.SoftwareVersion"),
		})); err != nil {
		t.Fatal(err)
	}

	// 事件不命中：什么都不发
	srv.evalPresets(devID, []string{"2 PERIODIC"}, false)
	if k := pendingKinds(t, st, devID); len(k) != 0 {
		t.Fatalf("事件不命中不该入队，得到 %v", k)
	}

	// 命中：SET 与 GET 按写入顺序入队
	srv.evalPresets(devID, []string{"1 BOOT"}, false)
	k := pendingKinds(t, st, devID)
	if len(k) != 2 || k[0] != TaskSetParameterValues || k[1] != TaskGetParameterValues {
		t.Fatalf("入队顺序/类型不对: %v", k)
	}
	// SET 的载荷就是预设里写的参数
	tasks, _ := st.ListTasks(devID, 50)
	var payload spvPayload
	for _, tk := range tasks {
		if tk.Kind == TaskSetParameterValues {
			_ = json.Unmarshal([]byte(tk.Payload), &payload)
		}
	}
	if len(payload.Values) != 1 || payload.Values[0].Value != "300" || payload.Values[0].Type != "unsignedInt" {
		t.Errorf("SET 载荷不对: %+v", payload.Values)
	}

	// once_per_device：再来一次不重复入队
	srv.evalPresets(devID, []string{"1 BOOT"}, false)
	if k := pendingKinds(t, st, devID); len(k) != 2 {
		t.Fatalf("once 预设不该重复触发，得到 %d 条待办", len(k))
	}
}

// 首次纳管触发 + 停用不触发。
func TestEvalPresetsNewDeviceAndDisabled(t *testing.T) {
	srv, st, devID := newTestServer(t, Config{})
	id, err := st.CreatePreset(mkPreset("纳管欢迎", nil, true,
		[]store.PresetAction{gpvAction("InternetGatewayDevice.DeviceInfo.UpTime")}))
	if err != nil {
		t.Fatal(err)
	}
	// new_device=false 时事件也不带 → 不触发
	srv.evalPresets(devID, []string{"2 PERIODIC"}, false)
	if k := pendingKinds(t, st, devID); len(k) != 0 {
		t.Fatalf("不该触发，得到 %v", k)
	}
	// 首次纳管 → 触发
	srv.evalPresets(devID, []string{"0 BOOTSTRAP"}, true)
	if k := pendingKinds(t, st, devID); len(k) != 1 {
		t.Fatalf("首次纳管应触发，得到 %v", k)
	}
	// 停用后新设备也不触发
	_ = st.SetPresetEnabled(id, false)
	devID2 := devID + 1000 // 另一台没触发过的设备
	srv.evalPresets(devID2, nil, true)
	if k := pendingKinds(t, st, devID2); len(k) != 0 {
		t.Fatalf("停用后不该触发，得到 %v", k)
	}
}

// 参数条件：eq / ne / contains / exists 四种，与「参数不存在」的口径。
func TestEvalPresetsConditions(t *testing.T) {
	srv, st, devID := newTestServer(t, Config{})
	// 设备已有参数：ModelName=V271-20
	if err := st.UpsertParams(devID, []store.Param{
		{Name: "InternetGatewayDevice.DeviceInfo.ModelName", Value: "V271-20"},
		{Name: "InternetGatewayDevice.DeviceInfo.SoftwareVersion", Value: ""},
	}, "getvalues"); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name      string
		op, value string
		wantFire  bool
	}{
		{"eq 命中", "eq", "V271-20", true},
		{"eq 不命中", "eq", "HN8145", false},
		{"ne 命中", "ne", "HN8145", true},
		{"contains 命中", "contains", "271", true},
		{"contains 不命中", "contains", "999", false},
		{"exists 命中（非空）", "exists", "", true},
	}
	for i, c := range cases {
		p := mkPreset("cond", []string{"2 PERIODIC"}, false,
			[]store.PresetAction{gpvAction("InternetGatewayDevice.DeviceInfo.UpTime")})
		p.CondParam = "DeviceInfo.ModelName" // 按后缀匹配
		p.CondOp = c.op
		p.CondValue = c.value
		p.OncePerDevice = false // 条件用例之间互不干扰
		if _, err := st.CreatePreset(p); err != nil {
			t.Fatal(err)
		}
		before := len(pendingKinds(t, st, devID))
		srv.evalPresets(devID, []string{"2 PERIODIC"}, false)
		after := len(pendingKinds(t, st, devID))
		fired := after > before
		if fired != c.wantFire {
			t.Errorf("[%s] 触发=%v，期望 %v", c.name, fired, c.wantFire)
		}
		_ = st.DeletePreset(int64(i + 1))
		drainPending(t, st, devID) // 清场：GPV 去重不该挡住下一个用例
	}

	// exists 对空值参数：不算存在
	p := mkPreset("exists-empty", []string{"2 PERIODIC"}, false,
		[]store.PresetAction{gpvAction("InternetGatewayDevice.DeviceInfo.UpTime")})
	p.CondParam = "SoftwareVersion"
	p.CondOp = "exists"
	p.OncePerDevice = false
	if _, err := st.CreatePreset(p); err != nil {
		t.Fatal(err)
	}
	before := len(pendingKinds(t, st, devID))
	srv.evalPresets(devID, []string{"2 PERIODIC"}, false)
	if after := len(pendingKinds(t, st, devID)); after > before {
		t.Error("空值参数不该命中 exists")
	}
}

// 预设数据写坏（不认识的动作类型）：跳过并留痕，不炸会话、不占 once 记录。
func TestEvalPresetsUnknownActionSkipped(t *testing.T) {
	srv, st, devID := newTestServer(t, Config{})
	p := mkPreset("坏动作", []string{"1 BOOT"}, false,
		[]store.PresetAction{{Kind: "Download"}}) // 本期不支持的动作类型
	id, err := st.CreatePreset(p)
	if err != nil {
		t.Fatal(err)
	}
	srv.evalPresets(devID, []string{"1 BOOT"}, false)
	if k := pendingKinds(t, st, devID); len(k) != 0 {
		t.Fatalf("不认识的动作不该入队，得到 %v", k)
	}
	// 空动作不算触发：once 记录不该被占掉
	if fired, _ := st.PresetFired(id, devID); fired {
		t.Error("没下发任何动作不该记触发")
	}
}
