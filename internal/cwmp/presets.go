package cwmp

import (
	"encoding/json"
	"strings"

	"github.com/zy84338719/lightcat/internal/store"
)

// evalPresets 在 Inform 处理时评估全部启用的预设，命中的把动作**按序入队** ——
// dispatchNextTask 会在**同一次会话**里依次下发，不用等设备下一轮周期上报
// （这与 ConnectionRequest 凭据自愈同一个思路：纳管即配好）。
//
// 触发口径（与需求 FR-10 对齐，取「够用且如实」的子集）：
//   - 事件码：Inform 的事件与预设 events 任一相等（大小写不敏感）；
//   - 首次纳管：UpsertDevice 报 created，且预设勾了 new_device；
//   - 参数条件：按参数名**后缀**找值（TR-098/TR-181 两种根都命中），
//     op 支持 eq / ne / contains / exists；
//   - once_per_device：命中并下发过就不再触发（preset_fires 记录；
//     面板上编辑预设会清记录，改过的预设重新生效一次）。
//
// 评估失败（读库、入队）只记日志不中断会话：预设是锦上添花，
// 不能因为它把纳管主流程打挂（NFR-9 同一口径）。
func (s *Server) evalPresets(deviceID int64, events []string, newDevice bool) {
	presets, err := s.store.ListEnabledPresets()
	if err != nil {
		s.log.Warn("读预设失败", "device_id", deviceID, "err", err)
		return
	}
	if len(presets) == 0 {
		return
	}
	var (
		params     []store.Param
		paramsOnce bool
	)
	loadParams := func() []store.Param {
		if !paramsOnce {
			params, _ = s.store.ListParams(deviceID)
			paramsOnce = true
		}
		return params
	}
	for _, p := range presets {
		if p.OncePerDevice {
			if fired, err := s.store.PresetFired(p.ID, deviceID); err == nil && fired {
				continue
			}
		}
		if !presetTriggered(p, events, newDevice) {
			continue
		}
		if p.CondParam != "" && !presetCondMet(p, loadParams()) {
			continue
		}
		n, err := s.enqueuePresetActions(deviceID, p)
		if err != nil {
			s.log.Warn("预设动作入队失败（跳过这条预设）",
				"device_id", deviceID, "preset", p.Name, "err", err)
			continue
		}
		if n == 0 {
			continue // 空动作：不算触发，不占 once 记录
		}
		if p.OncePerDevice {
			if err := s.store.MarkPresetFired(p.ID, deviceID); err != nil {
				s.log.Warn("记预设触发失败", "preset", p.Name, "err", err)
			}
		}
		s.log.Info("预设命中，动作已入队",
			"device_id", deviceID, "preset", p.Name, "actions", n)
	}
}

// presetTriggered 判事件码 / 首次纳管是否命中。
func presetTriggered(p store.Preset, events []string, newDevice bool) bool {
	if p.NewDevice && newDevice {
		return true
	}
	if len(p.Events) == 0 {
		return false
	}
	want := make(map[string]bool, len(p.Events))
	for _, e := range p.Events {
		want[strings.ToUpper(strings.TrimSpace(e))] = true
	}
	for _, e := range events {
		if want[strings.ToUpper(strings.TrimSpace(e))] {
			return true
		}
	}
	return false
}

// presetCondMet 按参数名后缀找值并比较。找不到参数：
// exists → false；ne → true（「没有这个参数」确实不等于任何值）；其余 → false。
func presetCondMet(p store.Preset, params []store.Param) bool {
	suffix := strings.ToLower(strings.TrimSpace(p.CondParam))
	if !strings.HasPrefix(suffix, ".") {
		suffix = "." + suffix
	}
	val := ""
	found := false
	for _, prm := range params {
		if strings.HasSuffix(strings.ToLower(prm.Name), suffix) && !strings.HasSuffix(prm.Name, ".") {
			val, found = prm.Value, true
			break
		}
	}
	switch strings.ToLower(p.CondOp) {
	case "exists":
		return found && strings.TrimSpace(val) != ""
	case "eq":
		return found && strings.TrimSpace(val) == strings.TrimSpace(p.CondValue)
	case "ne":
		return !found || strings.TrimSpace(val) != strings.TrimSpace(p.CondValue)
	case "contains":
		return found && strings.Contains(strings.ToLower(val), strings.ToLower(p.CondValue))
	}
	// 不认识的 op：不触发（宁可不发，不能错发）
	return false
}

// enqueuePresetActions 把一条预设的动作按序入队，返回入队条数。
func (s *Server) enqueuePresetActions(deviceID int64, p store.Preset) (int, error) {
	n := 0
	for _, a := range p.Actions {
		switch a.Kind {
		case TaskSetParameterValues:
			if len(a.Values) == 0 {
				continue
			}
			vals := make([]ParamValue, 0, len(a.Values))
			for _, v := range a.Values {
				typ := strings.TrimSpace(v.Type)
				if typ == "" {
					typ = "string"
				}
				vals = append(vals, ParamValue{Name: v.Name, Value: v.Value, Type: typ})
			}
			if _, err := s.EnqueueSetParameters(deviceID, vals); err != nil {
				return n, err
			}
			n++
		case TaskGetParameterValues:
			if len(a.Names) == 0 {
				continue
			}
			payload, _ := json.Marshal(gpvPayload{Names: a.Names})
			if _, _, err := s.store.EnqueueTaskIfAbsent(&store.Task{
				DeviceID: deviceID,
				Kind:     TaskGetParameterValues,
				Payload:  string(payload),
			}); err != nil {
				return n, err
			}
			n++
		default:
			// 不认识的 kind：跳过并留痕（预设是数据，写错了不该炸会话）
			s.log.Warn("预设里有不认识的动作类型，已跳过",
				"preset", p.Name, "kind", a.Kind)
		}
	}
	return n, nil
}
