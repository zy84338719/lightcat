package store

import (
	"testing"
)

func TestPresetCRUD(t *testing.T) {
	st := newTestStore(t)

	p := &Preset{
		Name:      "纳管即配好周期上报",
		Enabled:   true,
		Events:    []string{"0 BOOTSTRAP"},
		NewDevice: true,
		CondParam: "ModelName",
		CondOp:    "eq",
		CondValue: "V271-20",
		Actions: []PresetAction{{
			Kind:   PresetActionSet,
			Values: []PresetValue{{Name: "InternetGatewayDevice.ManagementServer.PeriodicInformInterval", Value: "300", Type: "unsignedInt"}},
		}},
		OncePerDevice: true,
	}
	id, err := st.CreatePreset(p)
	if err != nil {
		t.Fatalf("建预设失败: %v", err)
	}
	if id == 0 {
		t.Fatal("应返回自增 ID")
	}

	list, err := st.ListPresets()
	if err != nil || len(list) != 1 {
		t.Fatalf("列出预设失败: %d 条, err=%v", len(list), err)
	}
	got := list[0]
	if got.Name != p.Name || !got.Enabled || !got.NewDevice || !got.OncePerDevice {
		t.Errorf("字段没原样往返: %+v", got)
	}
	if len(got.Events) != 1 || got.Events[0] != "0 BOOTSTRAP" {
		t.Errorf("事件没原样往返: %+v", got.Events)
	}
	if len(got.Actions) != 1 || got.Actions[0].Kind != PresetActionSet ||
		len(got.Actions[0].Values) != 1 || got.Actions[0].Values[0].Type != "unsignedInt" {
		t.Errorf("动作 JSON 没原样往返: %+v", got.Actions)
	}
	if got.CondParam != "ModelName" || got.CondOp != "eq" || got.CondValue != "V271-20" {
		t.Errorf("条件没原样往返: %+v", got)
	}

	// 空名字要拒绝
	if _, err := st.CreatePreset(&Preset{Name: "  "}); err == nil {
		t.Error("空预设名应报错")
	}

	// 启停
	if err := st.SetPresetEnabled(id, false); err != nil {
		t.Fatal(err)
	}
	enabled, _ := st.ListEnabledPresets()
	if len(enabled) != 0 {
		t.Errorf("停用后 ListEnabledPresets 不该有它，得到 %d", len(enabled))
	}
	if err := st.SetPresetEnabled(id, true); err != nil {
		t.Fatal(err)
	}
	enabled, _ = st.ListEnabledPresets()
	if len(enabled) != 1 {
		t.Errorf("启用后应能列出，得到 %d", len(enabled))
	}

	// 触发记录 + 编辑清记录
	if fired, _ := st.PresetFired(id, 7); fired {
		t.Error("还没触发过")
	}
	if err := st.MarkPresetFired(id, 7); err != nil {
		t.Fatal(err)
	}
	if fired, _ := st.PresetFired(id, 7); !fired {
		t.Error("应已记录触发")
	}
	_ = st.MarkPresetFired(id, 7) // 幂等
	if err := st.UpdatePreset(&got); err != nil {
		t.Fatal(err)
	}
	if fired, _ := st.PresetFired(id, 7); fired {
		t.Error("编辑预设应清掉触发记录（改过的预设要重新生效）")
	}

	// 删除连触发记录一起清
	_ = st.MarkPresetFired(id, 7)
	if err := st.DeletePreset(id); err != nil {
		t.Fatal(err)
	}
	list, _ = st.ListPresets()
	if len(list) != 0 {
		t.Errorf("删除后应列不出，得到 %d", len(list))
	}
}
