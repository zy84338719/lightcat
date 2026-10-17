package store

import (
	"strings"
	"unicode/utf8"

	"github.com/oekit/pkg/errors"
)

// 设备标签：运维分组（按位置 / 用途 / 批次），概览页可按标签筛选。
// 一个标签一行（device_tags 表，迁移 #16），写入口统一走 SetDeviceTags（整组替换）。

const (
	// MaxTagsPerDevice 单台设备最多几个标签（防误粘贴一整篇文章进来）。
	MaxTagsPerDevice = 16
	// MaxTagLen 单个标签最长多少个字符（按 rune 数，中文按字算）。
	MaxTagLen = 24
)

// TagCount 是一个标签和「打了几台设备」的计数（概览页筛选 chips 用）。
type TagCount struct {
	Tag   string
	Count int
}

// NormalizeTags 把用户输入洗成规范标签：按逗号/分号/空白拆分、去首尾空、
// 去空项、超长截断、去重（保持出现顺序）、限量。
// 全部洗没了就返回空切片 —— 等价于「清空标签」，不算错误。
func NormalizeTags(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		switch r {
		case ',', '，', ';', '；', ' ', '\t', '\n', '\r':
			return true
		}
		return false
	})
	seen := make(map[string]bool, len(fields))
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		t := strings.TrimSpace(f)
		if t == "" || seen[t] {
			continue
		}
		if utf8.RuneCountInString(t) > MaxTagLen {
			t = string([]rune(t)[:MaxTagLen])
		}
		seen[t] = true
		out = append(out, t)
		if len(out) >= MaxTagsPerDevice {
			break
		}
	}
	return out
}

// SetDeviceTags 整组替换一台设备的标签（tags 应为 NormalizeTags 的产物）。
// 传空切片 = 清空。整体在一个事务里，不会留半套。
func (s *Store) SetDeviceTags(deviceID int64, tags []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return errors.Wrap(err, "开事务失败")
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM device_tags WHERE device_id = ?`, deviceID); err != nil {
		return errors.Wrap(err, "清旧标签失败")
	}
	for _, t := range tags {
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO device_tags (device_id, tag) VALUES (?, ?)`, deviceID, t); err != nil {
			return errors.Wrap(err, "写标签失败")
		}
	}
	if err := tx.Commit(); err != nil {
		return errors.Wrap(err, "提交标签失败")
	}
	return nil
}

// DeviceTags 返回一台设备的标签（按标签名排序，展示稳定）。
func (s *Store) DeviceTags(deviceID int64) ([]string, error) {
	rows, err := s.db.Query(`SELECT tag FROM device_tags WHERE device_id = ? ORDER BY tag`, deviceID)
	if err != nil {
		return nil, errors.Wrap(err, "读标签失败")
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TagsByDevice 一次拿回所有设备的标签（device_id → 标签列表）。
// 列表页整页填充用，避免 N+1。
func (s *Store) TagsByDevice() (map[int64][]string, error) {
	rows, err := s.db.Query(`SELECT device_id, tag FROM device_tags ORDER BY device_id, tag`)
	if err != nil {
		return nil, errors.Wrap(err, "读标签失败")
	}
	defer rows.Close()
	out := map[int64][]string{}
	for rows.Next() {
		var id int64
		var t string
		if err := rows.Scan(&id, &t); err != nil {
			return nil, err
		}
		out[id] = append(out[id], t)
	}
	return out, rows.Err()
}

// AllTags 返回全部标签，按「用的设备数」降序、同数按标签名升序（chips 条的展示序）。
func (s *Store) AllTags() ([]TagCount, error) {
	rows, err := s.db.Query(`SELECT tag, COUNT(*) FROM device_tags GROUP BY tag ORDER BY COUNT(*) DESC, tag`)
	if err != nil {
		return nil, errors.Wrap(err, "读标签统计失败")
	}
	defer rows.Close()
	var out []TagCount
	for rows.Next() {
		var tc TagCount
		if err := rows.Scan(&tc.Tag, &tc.Count); err != nil {
			return nil, err
		}
		out = append(out, tc)
	}
	return out, rows.Err()
}

// DevicesWithTag 返回带某标签的设备 ID 集合（筛选走集合判定，列表页一次拉全）。
func (s *Store) DevicesWithTag(tag string) (map[int64]bool, error) {
	rows, err := s.db.Query(`SELECT device_id FROM device_tags WHERE tag = ?`, tag)
	if err != nil {
		return nil, errors.Wrap(err, "按标签读设备失败")
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}
