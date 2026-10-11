package store

import (
	"database/sql"
	"fmt"
	"time"
)

// OpticalSample 是光功率历史里的一条读数（换算后的值，单位见字段名）。
// 读数为 NULL 表示那次采集没读到（比如 Web 通道拿不到上行收光）—— 宁缺毋编。
type OpticalSample struct {
	ID     int64
	At     time.Time
	RxDbm  sql.NullFloat64
	TxDbm  sql.NullFloat64
	TempC  sql.NullFloat64
	VoltV  sql.NullFloat64
	BiasMa sql.NullFloat64
	Status string
}

// SetOpticalHistoryRetention 设置光功率历史保留多少天（0 = 不限）。
// 与 taskHistoryLimit 同款做法：配置决定、字段承载、写入时顺带清理。
func (s *Store) SetOpticalHistoryRetention(days int) {
	if days < 0 {
		days = 0
	}
	s.opticalHistoryDays = days
}

// InsertOpticalSample 记一条光功率历史。nil 表示这次没读到该读数
// （比如 Web 通道拿不到上行收光）—— 宁可空着也不编数。
// 写入后顺带按保留期清理旧行（清理失败不影响本次写入）。
func (s *Store) InsertOpticalSample(deviceID int64, at time.Time,
	rx, tx, temp, volt, bias *float64, status string) error {
	_, err := s.db.Exec(`INSERT INTO optical_history
		(device_id, ts, rx_dbm, tx_dbm, temp_c, volt_v, bias_ma, status)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		deviceID, ts(at), nullFloat(rx), nullFloat(tx), nullFloat(temp), nullFloat(volt), nullFloat(bias), status)
	if err != nil {
		return fmt.Errorf("写光功率历史失败: %w", err)
	}
	if s.opticalHistoryDays > 0 {
		_, _ = s.PruneOpticalHistory(at.AddDate(0, 0, -s.opticalHistoryDays))
	}
	return nil
}

// OpticalHistory 返回某台设备最近的 limit 条光功率历史，按时间升序。
func (s *Store) OpticalHistory(deviceID int64, limit int) ([]OpticalSample, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.Query(`SELECT id, ts, rx_dbm, tx_dbm, temp_c, volt_v, bias_ma, status
		FROM (SELECT * FROM optical_history WHERE device_id = ? ORDER BY id DESC LIMIT ?)
		ORDER BY id`, deviceID, limit)
	if err != nil {
		return nil, fmt.Errorf("读光功率历史失败: %w", err)
	}
	defer rows.Close()
	var out []OpticalSample
	for rows.Next() {
		var o OpticalSample
		var t string
		if err := rows.Scan(&o.ID, &t, &o.RxDbm, &o.TxDbm, &o.TempC, &o.VoltV, &o.BiasMa, &o.Status); err != nil {
			return nil, err
		}
		o.At = parseTS(t)
		out = append(out, o)
	}
	return out, rows.Err()
}

// LastOpticalSample 返回某台设备最近一条光功率历史（没有则 ok=false）。
func (s *Store) LastOpticalSample(deviceID int64) (OpticalSample, bool, error) {
	hist, err := s.OpticalHistory(deviceID, 1)
	if err != nil || len(hist) == 0 {
		return OpticalSample{}, false, err
	}
	return hist[0], true, nil
}

// RecordOpticalSample 记一条读数，并把「同一次采集」的多批回执并成一个点：
// 该设备最近一条历史与 at 相差不超过 window 时，合并进那条（只用这次新认出的
// 读数补空，不把已有值覆盖掉），否则插新行。window <= 0 时总是插新行。
//
// 为什么要合并：TR-069 的 GetParameterValues 是分批下发的（单批最多几百个
// 参数名，真机上限 256），一次「重新获取」会回来十几批，光口读数散在其中
// 几批里 —— 不合并的话趋势图会画出一堆几乎重叠的点。
func (s *Store) RecordOpticalSample(deviceID int64, at time.Time,
	rx, tx, temp, volt, bias *float64, status string, window time.Duration) error {
	// window > 0 时要先看最近一条再决定合并还是插新行 —— 这两步必须在一个
	// 事务里：SetMaxOpenConns(1) 只保证单条语句串行，不保证「读 → 改」原子。
	// 否则 TR-069 分批回执与 ontweb 轮询撞进同一个窗口时，两路都会判定
	// 「需插新行」，趋势图上就会多出一个重叠点（正是这个函数要消除的东西）。
	//
	// 注意：事务内只能用 txs，不能回调 s.db（单连接下会自锁）。
	if window > 0 {
		txs, err := s.db.Begin()
		if err != nil {
			return fmt.Errorf("开始光功率合并事务失败: %w", err)
		}
		defer txs.Rollback()

		var lastID int64
		var tsRaw string
		row := txs.QueryRow(`SELECT id, ts FROM optical_history
			WHERE device_id = ? ORDER BY ts DESC LIMIT 1`, deviceID)
		switch err := row.Scan(&lastID, &tsRaw); {
		case err == sql.ErrNoRows:
			// 没有历史行：直接插入，走下面的公共路径
		case err != nil:
			return fmt.Errorf("读取最近光功率样本失败: %w", err)
		default:
			// ts 列存的是字符串（与 OpticalHistory 的读法一致，用 parseTS 转回来）
			lastAt := parseTS(tsRaw)
			d := at.Sub(lastAt)
			if d < 0 {
				d = -d
			}
			if d <= window {
				// 读数「先到的优先、空位才补」（COALESCE(旧, 新)）：同一轮采集的
				// 多批回执里，同一读数不该被后到的批次改写；
				// status 是时点状态，反过来「新的非空值优先」。
				if _, err := txs.Exec(`UPDATE optical_history SET
					rx_dbm = COALESCE(rx_dbm, ?), tx_dbm = COALESCE(tx_dbm, ?),
					temp_c = COALESCE(temp_c, ?), volt_v = COALESCE(volt_v, ?),
					bias_ma = COALESCE(bias_ma, ?),
					status = CASE WHEN ? != '' THEN ? ELSE status END
					WHERE id = ?`,
					nullFloat(rx), nullFloat(tx), nullFloat(temp), nullFloat(volt), nullFloat(bias),
					status, status, lastID); err != nil {
					return fmt.Errorf("合并光功率历史失败: %w", err)
				}
				return txs.Commit()
			}
		}
		if err := txs.Commit(); err != nil {
			return fmt.Errorf("提交光功率合并事务失败: %w", err)
		}
	}
	return s.InsertOpticalSample(deviceID, at, rx, tx, temp, volt, bias, status)
}

// PruneOpticalHistory 删掉早于 before 的历史行，返回删除条数。
func (s *Store) PruneOpticalHistory(before time.Time) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM optical_history WHERE ts < ?`, ts(before))
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// RxWorst 是「收光最弱」榜的一行：某台设备最近一次的收光读数。
type RxWorst struct {
	DeviceID int64
	Serial   string
	Model    string
	RxDbm    float64
	At       time.Time
}

// WorstRx 按「每台设备最近一次收光读数」从低到高返回前 limit 台。
// 没有收光数据的设备不参与（Web 通道读不到上行收光，不该拿 NULL 冒充垫底）。
// 取「每台最近一条」用相关子查询走 (device_id, id) 索引，几百台设备毫秒级。
func (s *Store) WorstRx(limit int) ([]RxWorst, error) {
	if limit <= 0 {
		limit = 5
	}
	rows, err := s.db.Query(`
		SELECT o.device_id, d.serial_number, d.model_name, o.rx_dbm, o.ts
		FROM optical_history o
		JOIN devices d ON d.id = o.device_id
		WHERE o.rx_dbm IS NOT NULL
		  AND o.id = (SELECT MAX(o2.id) FROM optical_history o2
		              WHERE o2.device_id = o.device_id AND o2.rx_dbm IS NOT NULL)
		ORDER BY o.rx_dbm ASC, o.device_id ASC
		LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("查收光最弱榜失败: %w", err)
	}
	defer rows.Close()
	var out []RxWorst
	for rows.Next() {
		var w RxWorst
		var t string
		if err := rows.Scan(&w.DeviceID, &w.Serial, &w.Model, &w.RxDbm, &t); err != nil {
			return nil, err
		}
		w.At = parseTS(t)
		out = append(out, w)
	}
	return out, rows.Err()
}

func nullFloat(v *float64) any {
	if v == nil {
		return nil
	}
	return *v
}
