package store

import (
	"database/sql"
	"time"

	"github.com/oekit/pkg/errors"
)

// Alert 是一条告警记录（阈值触发或设备离线时写入，见 internal/alert）。
type Alert struct {
	ID        int64
	DeviceID  int64
	Kind      string // rx_low / temp_high / optic_down / offline
	Message   string // 给人看的一句话（含读数与阈值）
	Value     sql.NullFloat64
	Threshold sql.NullFloat64
	Notified  bool // webhook 推送成功过
	CreatedAt time.Time

	// 列表页联查出来的设备信息（单条读写时为空）。
	SerialNumber string
	ModelName    string
}

// alertHistoryLimit 是告警表的全局保留条数。告警很稀疏（冷却窗口挡着），
// 一千条足够回溯几个月；超了裁最老的。
const alertHistoryLimit = 1000

// InsertAlert 写一条告警，并顺带裁剪超出保留条数的最老记录。
func (s *Store) InsertAlert(a *Alert) error {
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now()
	}
	res, err := s.db.Exec(`INSERT INTO alerts
		(device_id, kind, message, value, threshold, notified, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		a.DeviceID, a.Kind, a.Message, nullFloat64(a.Value), nullFloat64(a.Threshold),
		boolInt(a.Notified), ts(a.CreatedAt))
	if err != nil {
		return errors.Wrap(err, "写告警失败")
	}
	if a.ID, err = res.LastInsertId(); err != nil {
		return err
	}
	_, _ = s.db.Exec(`DELETE FROM alerts WHERE id <= (
		SELECT id FROM alerts ORDER BY id DESC LIMIT 1 OFFSET ?)`, alertHistoryLimit)
	return nil
}

// LastAlertAt 返回某设备某类告警最近一次的时间（冷却窗口判定用）。
func (s *Store) LastAlertAt(deviceID int64, kind string) (time.Time, bool, error) {
	var t string
	err := s.db.QueryRow(`SELECT created_at FROM alerts
		WHERE device_id = ? AND kind = ? ORDER BY id DESC LIMIT 1`, deviceID, kind).Scan(&t)
	if err == sql.ErrNoRows {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	return parseTS(t), true, nil
}

// MarkAlertNotified 把一条告警标记为「webhook 已推送成功」。
func (s *Store) MarkAlertNotified(id int64) error {
	_, err := s.db.Exec(`UPDATE alerts SET notified = 1 WHERE id = ?`, id)
	return err
}

const alertCols = `a.id, a.device_id, a.kind, a.message, a.value, a.threshold, a.notified, a.created_at,
	COALESCE(d.serial_number, ''), COALESCE(d.model_name, '')`

func scanAlert(sc interface{ Scan(...any) error }) (Alert, error) {
	var a Alert
	var t string
	var notified int
	if err := sc.Scan(&a.ID, &a.DeviceID, &a.Kind, &a.Message, &a.Value, &a.Threshold,
		&notified, &t, &a.SerialNumber, &a.ModelName); err != nil {
		return Alert{}, err
	}
	a.Notified = notified != 0
	a.CreatedAt = parseTS(t)
	return a, nil
}

// ListAlerts 按时间倒序列出最近的告警（联查设备序列号与型号）。
func (s *Store) ListAlerts(limit int) ([]Alert, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.Query(`SELECT `+alertCols+` FROM alerts a
		LEFT JOIN devices d ON d.id = a.device_id
		ORDER BY a.id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Alert
	for rows.Next() {
		a, err := scanAlert(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListDeviceAlerts 列出某台设备最近的告警（时间倒序）。
func (s *Store) ListDeviceAlerts(deviceID int64, limit int) ([]Alert, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(`SELECT `+alertCols+` FROM alerts a
		LEFT JOIN devices d ON d.id = a.device_id
		WHERE a.device_id = ? ORDER BY a.id DESC LIMIT ?`, deviceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Alert
	for rows.Next() {
		a, err := scanAlert(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// CountAlerts 数告警总条数（导航栏徽标用）。
func (s *Store) CountAlerts() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM alerts`).Scan(&n)
	return n, err
}

func nullFloat64(v sql.NullFloat64) any {
	if !v.Valid {
		return nil
	}
	return v.Float64
}
