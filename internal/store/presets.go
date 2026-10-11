package store

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// PresetValue 是一个要写进设备的参数（SetParameterValues 动作的一项）。
type PresetValue struct {
	Name  string `json:"name"`
	Value string `json:"value"`
	Type  string `json:"type"` // string / unsignedInt / boolean…（空按 string）
}

// PresetAction 是预设的一个动作。按序入队，同一会话内依次下发。
//
// kind 只有两种（够覆盖「纳管即配好」的绝大多数场景，也和任务队列的
// 能力对齐）：
//   - SetParameterValues：写参数（values）
//   - GetParameterValues：读参数（names，采回来入库供面板展示）
type PresetAction struct {
	Kind   string        `json:"kind"`
	Values []PresetValue `json:"values,omitempty"`
	Names  []string      `json:"names,omitempty"`
}

// 预设动作类型常量（与任务队列的 Kind 同值 —— 预设动作就是往队列里放这两种任务）。
// 放在 store 层：cwmp 与 web 都用得到，而 web 不依赖 cwmp（Controller 接口同一原则）。
const (
	PresetActionSet = "SetParameterValues"
	PresetActionGet = "GetParameterValues"
)

// Preset 是一条预设：触发条件 + 动作序列（需求 FR-10）。
type Preset struct {
	ID      int64
	Name    string
	Enabled bool

	// 触发：Events 命中任一 Inform 事件码，或 NewDevice（首次纳管）。
	// 两者都空 = 这条预设永远不会被事件触发（面板上会标注）。
	Events    []string
	NewDevice bool

	// 条件（可选）：设备参数里按**名字后缀**找 CondParam，用 CondOp 比较。
	// CondParam 为空 = 无条件。op 取值见 CondOps。
	CondParam string
	CondOp    string
	CondValue string

	Actions       []PresetAction
	OncePerDevice bool // 每台设备至多触发一次（preset_fires 记录）
	Weight        int  // 同时命中时按 weight、id 排序执行

	CreatedAt time.Time
	UpdatedAt time.Time
}

// CondOps 是条件支持的比较符（简单可比即可，不做表达式引擎）。
var CondOps = []string{"eq", "ne", "contains", "exists"}

// MarshalActions / unmarshalActions：actions 列是 JSON 数组。
func marshalActions(a []PresetAction) string {
	if len(a) == 0 {
		return "[]"
	}
	b, err := json.Marshal(a)
	if err != nil {
		return "[]"
	}
	return string(b)
}

func unmarshalActions(s string) []PresetAction {
	var out []PresetAction
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	return out
}

func joinEvents(e []string) string {
	out := make([]string, 0, len(e))
	for _, x := range e {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return strings.Join(out, ",")
}

func splitEvents(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// CreatePreset 新建一条预设，返回自增 ID。
func (s *Store) CreatePreset(p *Preset) (int64, error) {
	if strings.TrimSpace(p.Name) == "" {
		return 0, fmt.Errorf("预设名不能为空")
	}
	now := ts(time.Now())
	res, err := s.db.Exec(`INSERT INTO presets
		(name, enabled, events, new_device, cond_param, cond_op, cond_value,
		 actions, once_per_device, weight, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.Name, boolInt(p.Enabled), joinEvents(p.Events), boolInt(p.NewDevice),
		p.CondParam, p.CondOp, p.CondValue, marshalActions(p.Actions),
		boolInt(p.OncePerDevice), p.Weight, now, now)
	if err != nil {
		return 0, fmt.Errorf("新建预设失败: %w", err)
	}
	p.CreatedAt, p.UpdatedAt = time.Now(), time.Now()
	return res.LastInsertId()
}

// UpdatePreset 整条更新，并清掉它的触发记录 —— 改过的预设应该重新生效一次
// （比如把「写周期上报间隔」的值从 300 改成 600，不该因为已经触发过就不下发）。
func (s *Store) UpdatePreset(p *Preset) error {
	if p.ID == 0 {
		return fmt.Errorf("预设 ID 缺失")
	}
	_, err := s.db.Exec(`UPDATE presets SET
		name = ?, enabled = ?, events = ?, new_device = ?,
		cond_param = ?, cond_op = ?, cond_value = ?,
		actions = ?, once_per_device = ?, weight = ?, updated_at = ?
		WHERE id = ?`,
		p.Name, boolInt(p.Enabled), joinEvents(p.Events), boolInt(p.NewDevice),
		p.CondParam, p.CondOp, p.CondValue, marshalActions(p.Actions),
		boolInt(p.OncePerDevice), p.Weight, ts(time.Now()), p.ID)
	if err != nil {
		return fmt.Errorf("更新预设失败: %w", err)
	}
	_, _ = s.db.Exec(`DELETE FROM preset_fires WHERE preset_id = ?`, p.ID)
	return nil
}

// SetPresetEnabled 启停一条预设（不清触发记录：停用再启用不该导致重复下发）。
func (s *Store) SetPresetEnabled(id int64, enabled bool) error {
	_, err := s.db.Exec(`UPDATE presets SET enabled = ?, updated_at = ? WHERE id = ?`,
		boolInt(enabled), ts(time.Now()), id)
	return err
}

// DeletePreset 删除预设及其触发记录。
func (s *Store) DeletePreset(id int64) error {
	if _, err := s.db.Exec(`DELETE FROM preset_fires WHERE preset_id = ?`, id); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM presets WHERE id = ?`, id)
	return err
}

// ListPresets 列出全部预设（面板管理页），按 weight、id 排序。
func (s *Store) ListPresets() ([]Preset, error) {
	return s.listPresets(`SELECT id, name, enabled, events, new_device,
		cond_param, cond_op, cond_value, actions, once_per_device, weight, created_at, updated_at
		FROM presets ORDER BY weight, id`)
}

// ListEnabledPresets 列出启用的预设（Inform 触发评估用）。
func (s *Store) ListEnabledPresets() ([]Preset, error) {
	return s.listPresets(`SELECT id, name, enabled, events, new_device,
		cond_param, cond_op, cond_value, actions, once_per_device, weight, created_at, updated_at
		FROM presets WHERE enabled = 1 ORDER BY weight, id`)
}

func (s *Store) listPresets(q string) ([]Preset, error) {
	rows, err := s.db.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Preset
	for rows.Next() {
		var p Preset
		var (
			enabled, newDev, once int
			events, actions       string
			createdAt, updatedAt  string
		)
		if err := rows.Scan(&p.ID, &p.Name, &enabled, &events, &newDev,
			&p.CondParam, &p.CondOp, &p.CondValue, &actions, &once, &p.Weight,
			&createdAt, &updatedAt); err != nil {
			return nil, err
		}
		p.Enabled = enabled != 0
		p.NewDevice = newDev != 0
		p.OncePerDevice = once != 0
		p.Events = splitEvents(events)
		p.Actions = unmarshalActions(actions)
		p.CreatedAt = parseTS(createdAt)
		p.UpdatedAt = parseTS(updatedAt)
		out = append(out, p)
	}
	return out, rows.Err()
}

// PresetFired 查一条预设是否已经对某台设备触发过。
func (s *Store) PresetFired(presetID, deviceID int64) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM preset_fires WHERE preset_id = ? AND device_id = ?`,
		presetID, deviceID).Scan(&n)
	return n > 0, err
}

// MarkPresetFired 记一次触发（幂等）。
func (s *Store) MarkPresetFired(presetID, deviceID int64) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO preset_fires (preset_id, device_id, fired_at)
		VALUES (?, ?, ?)`, presetID, deviceID, ts(time.Now()))
	return err
}
