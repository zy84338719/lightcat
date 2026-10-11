package web

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/zy84338719/lightcat/internal/store"
)

// 预设可选的触发事件（Inform 事件码，见需求 §2.3）。
var presetEventChoices = []string{
	"0 BOOTSTRAP", "1 BOOT", "2 PERIODIC", "4 VALUE CHANGE",
	"6 CONNECTION REQUEST", "7 TRANSFER COMPLETE", "M Reboot",
}

// parsePresetActions 解析动作输入框：每行一个动作，
//
//	SET 参数名=值            写参数（类型默认 string）
//	SET 参数名=值|unsignedInt 写参数并指定类型
//	GET 参数名               读参数（采回来入库，面板可见）
//
// 空行与 # 开头的注释行跳过。相邻的同类行合并成一个动作（一条
// SetParameterValues 写完多个参数，省会话往返）；SET/GET 交替时保持用户写的顺序。
func parsePresetActions(text string) ([]store.PresetAction, error) {
	var out []store.PresetAction
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		verb, rest, _ := strings.Cut(line, " ")
		rest = strings.TrimSpace(rest)
		switch strings.ToUpper(strings.TrimSpace(verb)) {
		case "SET":
			name, value, hasEq := strings.Cut(rest, "=")
			name = strings.TrimSpace(name)
			// 必须有等号；「SET 参数名=」是合法的（显式写空值，比如清掉 WiFi 密码）
			if !hasEq || name == "" {
				return nil, fmt.Errorf("第 %d 行：SET 要写成「参数名=值」", i+1)
			}
			typ := "string"
			if v, t, ok := strings.Cut(value, "|"); ok {
				value, typ = v, strings.TrimSpace(t)
				if typ == "" {
					typ = "string"
				}
			}
			pv := store.PresetValue{Name: name, Value: strings.TrimSpace(value), Type: typ}
			if n := len(out); n > 0 && out[n-1].Kind == store.PresetActionSet {
				out[n-1].Values = append(out[n-1].Values, pv)
				continue
			}
			out = append(out, store.PresetAction{Kind: store.PresetActionSet, Values: []store.PresetValue{pv}})
		case "GET":
			if rest == "" {
				return nil, fmt.Errorf("第 %d 行：GET 后面要写参数名", i+1)
			}
			if n := len(out); n > 0 && out[n-1].Kind == store.PresetActionGet {
				out[n-1].Names = append(out[n-1].Names, rest)
				continue
			}
			out = append(out, store.PresetAction{Kind: store.PresetActionGet, Names: []string{rest}})
		default:
			return nil, fmt.Errorf("第 %d 行：不认识的动作 %q（只支持 SET / GET）", i+1, verb)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("至少要有一个动作（SET 参数名=值 或 GET 参数名）")
	}
	return out, nil
}

// presetSummary 给列表页一个「一眼看懂」的动作摘要。
func presetSummary(p store.Preset) string {
	var parts []string
	for _, a := range p.Actions {
		switch a.Kind {
		case store.PresetActionSet:
			names := make([]string, 0, len(a.Values))
			for _, v := range a.Values {
				if i := strings.LastIndex(v.Name, "."); i >= 0 {
					names = append(names, v.Name[i+1:])
				} else {
					names = append(names, v.Name)
				}
			}
			parts = append(parts, fmt.Sprintf("写 %d 项（%s）", len(a.Values), strings.Join(names, "、")))
		case store.PresetActionGet:
			parts = append(parts, fmt.Sprintf("读 %d 项", len(a.Names)))
		}
	}
	return strings.Join(parts, " → ")
}

// handlePresets 渲染「预设」页：列表 + 新建表单。
func (s *Server) handlePresets(w http.ResponseWriter, r *http.Request) {
	presets, err := s.store.ListPresets()
	if err != nil {
		http.Error(w, "读取预设失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	rows := make([]map[string]any, 0, len(presets))
	for _, p := range presets {
		rows = append(rows, map[string]any{
			"ID":        p.ID,
			"Name":      p.Name,
			"Enabled":   p.Enabled,
			"Trigger":   presetTriggerText(p),
			"Cond":      presetCondText(p),
			"Summary":   presetSummary(p),
			"Once":      p.OncePerDevice,
			"UpdatedAt": p.UpdatedAt,
		})
	}
	s.renderLang(w, r, "presets.html", map[string]any{
		"Section":      "presets",
		"Presets":      rows,
		"EventChoices": presetEventChoices,
		"CondOps":      store.CondOps,
		"Notice":       strings.TrimSpace(r.URL.Query().Get("msg")),
		"NoticeErr":    r.URL.Query().Get("err") == "1",
	})
}

// presetTriggerText 拼触发条件的展示文案（后端拼串，走 TS 智能翻译）。
func presetTriggerText(p store.Preset) string {
	var parts []string
	if p.NewDevice {
		parts = append(parts, "首次纳管")
	}
	parts = append(parts, p.Events...)
	return strings.Join(parts, " / ")
}

func presetCondText(p store.Preset) string {
	if p.CondParam == "" {
		return ""
	}
	return fmt.Sprintf("%s %s %s", p.CondParam, p.CondOp, p.CondValue)
}

// handlePresetCreate 处理新建预设的表单。
func (s *Server) handlePresetCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.presetBack(w, r, "表单解析失败", true)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		s.presetBack(w, r, "预设名不能为空", true)
		return
	}
	events := r.Form["events"]
	for _, e := range events {
		if !containsStr(presetEventChoices, e) {
			s.presetBack(w, r, "不认识的事件码："+e, true)
			return
		}
	}
	newDevice := r.FormValue("new_device") == "on"
	if len(events) == 0 && !newDevice {
		s.presetBack(w, r, "至少要选一个触发条件（事件码或「首次纳管」）", true)
		return
	}
	actions, err := parsePresetActions(r.FormValue("actions"))
	if err != nil {
		s.presetBack(w, r, err.Error(), true)
		return
	}
	op := r.FormValue("cond_op")
	condParam := strings.TrimSpace(r.FormValue("cond_param"))
	if condParam != "" && !containsStr(store.CondOps, op) {
		s.presetBack(w, r, "不认识的条件比较符："+op, true)
		return
	}
	if condParam == "" {
		op = ""
	}
	p := &store.Preset{
		Name:          name,
		Enabled:       r.FormValue("enabled") != "off",
		Events:        events,
		NewDevice:     newDevice,
		CondParam:     condParam,
		CondOp:        op,
		CondValue:     r.FormValue("cond_value"),
		Actions:       actions,
		OncePerDevice: r.FormValue("once") == "on",
	}
	if _, err := s.store.CreatePreset(p); err != nil {
		s.presetBack(w, r, "保存失败: "+err.Error(), true)
		return
	}
	s.presetBack(w, r, "预设「"+name+"」已创建，设备下次上报时生效", false)
}

// handlePresetToggle 启停一条预设。
func (s *Server) handlePresetToggle(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.presetBack(w, r, "预设 ID 非法", true)
		return
	}
	enable := r.FormValue("enable") == "1"
	if err := s.store.SetPresetEnabled(id, enable); err != nil {
		s.presetBack(w, r, "操作失败: "+err.Error(), true)
		return
	}
	if enable {
		s.presetBack(w, r, "预设已启用", false)
	} else {
		s.presetBack(w, r, "预设已停用", false)
	}
}

// handlePresetDelete 删除一条预设（连同它的触发记录）。
func (s *Server) handlePresetDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.presetBack(w, r, "预设 ID 非法", true)
		return
	}
	if err := s.store.DeletePreset(id); err != nil {
		s.presetBack(w, r, "删除失败: "+err.Error(), true)
		return
	}
	s.presetBack(w, r, "预设已删除", false)
}

func (s *Server) presetBack(w http.ResponseWriter, r *http.Request, msg string, isErr bool) {
	q := "?msg=" + url.QueryEscape(msg)
	if isErr {
		q += "&err=1"
	}
	http.Redirect(w, r, "/presets"+q, http.StatusSeeOther)
}

func containsStr(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// apiPresets 返回预设 JSON（自动化用）。
func (s *Server) apiPresets(w http.ResponseWriter, r *http.Request) {
	payload, err := s.APIPresets()
	writeJSONOrError(w, payload, err)
}
