// Package store 是 ACS 的持久化层。
//
// 选型：SQLite（modernc.org/sqlite，纯 Go 实现，不需要 cgo），
// 这样整个 ACS 能保持「单个静态二进制 + 一个 .db 文件」，符合需求文档 G3/NFR-1。
package store

import (
	"context"
	"database/sql"
	stderrors "errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/oekit/pkg/errors"
	_ "modernc.org/sqlite"
)

// Store 包住 *sql.DB。
type Store struct {
	db *sql.DB

	// 关停护栏（见 Close）：closeOnce 保证幂等，closed 让后台协程能在写库前
	// 自我判断「库已经关了，这次写就别做了」，closed 先于 db.Close 落位，
	// 让关停窗口期进来的写尽量早点折返而不是排队等一次必然失败的 IO。
	closeOnce sync.Once
	closeErr  error
	closed    atomic.Bool

	// taskHistoryLimit / informHistoryLimit 是「每台设备保留多少条历史记录」（0 = 不限）。
	// 设成字段而不是常量：由配置决定，测试里也要能调小。
	taskHistoryLimit   int
	informHistoryLimit int

	// opticalHistoryDays 是光功率历史保留天数（0 = 不限），见 SetOpticalHistoryRetention。
	opticalHistoryDays int
}

// schema 是当前版本的建表语句。
// 目前是「一次性建表 + IF NOT EXISTS」，后续加字段时需要引入真正的版本迁移
// （见 docs/requirements.md §5 与 M-milestone 计划）。
const schema = `
CREATE TABLE IF NOT EXISTS devices (
  id                INTEGER PRIMARY KEY AUTOINCREMENT,
  oui               TEXT    NOT NULL DEFAULT '',
  product_class     TEXT    NOT NULL DEFAULT '',
  serial_number     TEXT    NOT NULL,
  manufacturer      TEXT    NOT NULL DEFAULT '',
  model_name        TEXT    NOT NULL DEFAULT '',
  data_model_root   TEXT    NOT NULL DEFAULT '',
  software_version  TEXT    NOT NULL DEFAULT '',
  hardware_version  TEXT    NOT NULL DEFAULT '',
  spec_version      TEXT    NOT NULL DEFAULT '',
  provisioning_code TEXT    NOT NULL DEFAULT '',
  external_ip       TEXT    NOT NULL DEFAULT '',
  conn_request_url  TEXT    NOT NULL DEFAULT '',
  periodic_interval INTEGER NOT NULL DEFAULT 0,
  user_agent        TEXT    NOT NULL DEFAULT '',
  source_ip         TEXT    NOT NULL DEFAULT '',
  last_events       TEXT    NOT NULL DEFAULT '',
  first_seen_at     TEXT    NOT NULL DEFAULT '',
  last_inform_at    TEXT    NOT NULL DEFAULT '',
  last_boot_at      TEXT    NOT NULL DEFAULT '',
  online            INTEGER NOT NULL DEFAULT 0,
  UNIQUE (oui, product_class, serial_number)
);
CREATE INDEX IF NOT EXISTS idx_devices_serial ON devices(serial_number);

CREATE TABLE IF NOT EXISTS device_params (
  device_id  INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  name       TEXT    NOT NULL,
  value      TEXT    NOT NULL DEFAULT '',
  value_type TEXT    NOT NULL DEFAULT 'string',
  writable   INTEGER NOT NULL DEFAULT 0,
  source     TEXT    NOT NULL DEFAULT 'inform',
  updated_at TEXT    NOT NULL DEFAULT '',
  PRIMARY KEY (device_id, name)
);
CREATE INDEX IF NOT EXISTS idx_params_device ON device_params(device_id);

CREATE TABLE IF NOT EXISTS informs (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  device_id    INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  events       TEXT    NOT NULL DEFAULT '',
  command_key  TEXT    NOT NULL DEFAULT '',
  retry_count  INTEGER NOT NULL DEFAULT 0,
  current_time TEXT    NOT NULL DEFAULT '',
  source_ip    TEXT    NOT NULL DEFAULT '',
  param_count  INTEGER NOT NULL DEFAULT 0,
  created_at   TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_informs_device ON informs(device_id, created_at DESC);

CREATE TABLE IF NOT EXISTS tasks (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  device_id   INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  kind        TEXT    NOT NULL,
  payload     TEXT    NOT NULL DEFAULT '',
  command_key TEXT    NOT NULL DEFAULT '',
  status      TEXT    NOT NULL DEFAULT 'pending',
  result      TEXT    NOT NULL DEFAULT '',
  retry_count INTEGER NOT NULL DEFAULT 0,
  created_at  TEXT    NOT NULL DEFAULT '',
  started_at  TEXT    NOT NULL DEFAULT '',
  finished_at TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_tasks_device_status ON tasks(device_id, status, id);
`

// migrations 是增量迁移，按顺序执行；执行到哪一步记在 PRAGMA user_version 里。
//
// 约定：baseSchema 永远是「第 0 版」，**新加字段一律走迁移**，不要直接改 baseSchema ——
// 否则已存在的库升不上来，而新建的库又会因为重复建列而报错。
var migrations = []string{
	// 1：设备备注（概览页要展示、要能搜索）
	`ALTER TABLE devices ADD COLUMN note TEXT NOT NULL DEFAULT ''`,
	// 2：通用键值配置。
	// 目前用来存自动生成的 ConnectionRequest 密码 —— 不能每次重启都换，
	// 否则会把设备上的凭据写来写去。
	`CREATE TABLE IF NOT EXISTS settings (k TEXT PRIMARY KEY, v TEXT NOT NULL)`,
	// 3：离线探测状态 —— 设备超期没上报时我们会主动发 Connection Request 探测，
	// 这里记「探了几次、上一次什么时候探的」。设备一上报就清零。
	//
	// 迁移必须**追加在末尾**：序号对应 PRAGMA user_version，插在中间会让已经升到
	// 老版本的库跳过这一步（老库 user_version=2，只会执行 #3 及以后）。
	`ALTER TABLE devices ADD COLUMN probe_count INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE devices ADD COLUMN probe_at TEXT NOT NULL DEFAULT ''`,
	// 5：厂商私有参数 → 面板标准字段 的映射表（含原始值换算）。
	//
	// 为什么放库里：光功率/温度/电压这些参数**各家名字都不一样**，华为、中兴、烽火
	// 各写各的；更坑的是同一台设备上还有按光模块寄存器（SFF-8472）编码的**原始值**
	// （如 RXPower=254 其实是 -15.95 dBm）。这些知识写在代码里就得改代码、发版；
	// 放库里以后支持一台新光猫只需加几行数据。
	//
	// field：面板字段规范名（见 internal/web 的 panelFields：rx_power / tx_power /
	//        temperature / voltage / bias_current）
	// match_contains / match_suffix：参数名（小写）必须满足的包含/后缀条件（两者都空 = 不匹配任何参数）
	// decode：原始值换算（identity / dbm_01uw / div256 / mv / mv01 / ua2 …）
	// priority：同一字段命中多条时数字大的优先（精确的原始值 > 整数级近似值）
	`CREATE TABLE IF NOT EXISTS param_aliases (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  field          TEXT NOT NULL,
  vendor         TEXT NOT NULL DEFAULT '',
  match_contains TEXT NOT NULL DEFAULT '',
  match_suffix   TEXT NOT NULL DEFAULT '',
  decode         TEXT NOT NULL DEFAULT 'identity',
  priority       INTEGER NOT NULL DEFAULT 0,
  note           TEXT NOT NULL DEFAULT '',
  enabled        INTEGER NOT NULL DEFAULT 1,
  updated_at     TEXT NOT NULL DEFAULT '',
  UNIQUE (field, match_contains, match_suffix)
)`,
	// 6：种子映射。
	//
	// 全是 2026-09-30 在真机上对出来的（联通版 V271-20，PON，V5R023C10S200）：
	// `X_CU_WANEdgeONTPONInterfaceConfig.OpticalTransceiver.*` 那组是光模块寄存器原始值，
	// 换算后与设备自己页面的读数**完全一致**（收光 -15.95 dBm / 发光 0.00 dBm /
	// 温度 43.0 ℃ / 电压 3.226 V / 偏流 29.0 mA）；`X_GponInterafceConfig.*` 那组是
	// 同一台设备的整数近似值（精确到个位），所以优先用前者。
	`INSERT OR IGNORE INTO param_aliases (field, vendor, match_contains, match_suffix, decode, priority, note, updated_at) VALUES
  ('rx_power',     'Huawei', 'opticaltransceiver.', '.rxpower',                'dbm_01uw', 20, '光模块寄存器原始值：254 → -15.95 dBm（与设备自己页面一致）', ''),
  ('tx_power',     'Huawei', 'opticaltransceiver.', '.txpower',                'dbm_01uw', 20, '光模块寄存器原始值：10000 → 0.00 dBm', ''),
  ('temperature',  'Huawei', 'opticaltransceiver.', '.temperature',            'div256',   20, '11008 → 43.0 ℃', ''),
  ('voltage',      'Huawei', 'opticaltransceiver.', '.vcc',                    'mv01',     20, '32260 → 3.226 V', ''),
  ('bias_current', 'Huawei', 'opticaltransceiver.', '.txbias',                 'ua2',      20, '14500 → 29.0 mA', ''),
  ('rx_power',     'Huawei', 'x_gponinterafceconfig.', '.rxpower',             'identity', 10, '整数近似值（精确到个位）：-15', ''),
  ('tx_power',     'Huawei', 'x_gponinterafceconfig.', '.txpower',             'identity', 10, '整数近似值：0', ''),
  ('temperature',  'Huawei', 'x_gponinterafceconfig.', '.transceivertemperature', 'identity', 10, '43（℃）', ''),
  ('voltage',      'Huawei', 'x_gponinterafceconfig.', '.supplyvoltage',       'mv',       10, '3226 mV → 3.226 V', ''),
  ('bias_current', 'Huawei', 'x_gponinterafceconfig.', '.biascurrent',         'identity', 10, '29（mA）', '')`,
	// 7：光功率历史。ontweb 每次采集记一条，面板画趋势图用。
	//
	// 为什么单独一张表：device_params 是覆盖式的「当前值」，趋势需要时序。
	// 存换算后的读数而不是原始值：图不该受映射表后续改动的影响。
	`CREATE TABLE IF NOT EXISTS optical_history (
  id        INTEGER PRIMARY KEY AUTOINCREMENT,
  device_id INTEGER NOT NULL,
  ts        TEXT NOT NULL,
  tx_dbm    REAL,
  temp_c    REAL,
  volt_v    REAL,
  bias_ma   REAL,
  status    TEXT NOT NULL DEFAULT ''
)`,
	// 8：历史按设备取最近一段，索引建在 (device_id, id) 上。
	`CREATE INDEX IF NOT EXISTS idx_optical_history_dev ON optical_history (device_id, id)`,
	// 9：收光功率列。ontweb 的普通账号读不到上行收光（存 NULL），
	// TR-069 通道能读到（RXPower）—— 两条通道共用同一张历史表。
	`ALTER TABLE optical_history ADD COLUMN rx_dbm REAL`,
	// 10：告警记录。阈值（收光过低 / 光模块过温 / 光口异常）与设备离线在这里留痕，
	// 面板「告警」页与 webhook 推送共用同一份数据。
	//
	// kind：rx_low / temp_high / optic_down / offline（代码里的 Kind* 常量）
	// value / threshold：触发时的读数与阈值（离线这类没有读数的存 NULL）
	// notified：webhook 是否推送成功（没配 webhook 或推送失败都是 0）
	`CREATE TABLE IF NOT EXISTS alerts (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  device_id  INTEGER NOT NULL,
  kind       TEXT NOT NULL,
  message    TEXT NOT NULL DEFAULT '',
  value      REAL,
  threshold  REAL,
  notified   INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL DEFAULT ''
)`,
	// 11：告警按「设备 + 类型」查最近一条（冷却窗口判定），按时间倒序列出。
	`CREATE INDEX IF NOT EXISTS idx_alerts_device_kind ON alerts (device_id, kind, id)`,
	// 12：预设（Preset，需求 FR-10）。「什么事件 / 什么条件下，自动对设备做哪串操作」
	// 全部是数据：面板上增删改，不改代码不发版。
	//
	// events：逗号分隔的 Inform 事件码（如 "0 BOOTSTRAP,1 BOOT"）；空 = 不按事件触发
	// cond_*：可选的参数条件（按参数名后缀匹配 + 简单比较），空 = 无条件
	// actions：JSON 数组（见 PresetAction），按序入队，同一会话内依次下发
	`CREATE TABLE IF NOT EXISTS presets (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  name           TEXT NOT NULL,
  enabled        INTEGER NOT NULL DEFAULT 1,
  events         TEXT NOT NULL DEFAULT '',
  new_device     INTEGER NOT NULL DEFAULT 0,
  cond_param     TEXT NOT NULL DEFAULT '',
  cond_op        TEXT NOT NULL DEFAULT '',
  cond_value     TEXT NOT NULL DEFAULT '',
  actions        TEXT NOT NULL DEFAULT '[]',
  once_per_device INTEGER NOT NULL DEFAULT 1,
  weight         INTEGER NOT NULL DEFAULT 0,
  created_at     TEXT NOT NULL DEFAULT '',
  updated_at     TEXT NOT NULL DEFAULT ''
)`,
	// 13：预设的「每台设备只触发一次」记录（once_per_device=1 时用）。
	// 编辑预设会清掉它的触发记录 —— 改过的预设应该重新生效一次。
	`CREATE TABLE IF NOT EXISTS preset_fires (
  preset_id  INTEGER NOT NULL,
  device_id  INTEGER NOT NULL,
  fired_at   TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (preset_id, device_id)
)`,
	// 14：固件/配置文件库（需求 FR-11）。CPE 的 Download 是**设备自己来拉 URL**，
	// 所以要有内置文件服务；元数据（校验和、适用型号、版本）在这张表里。
	//
	// url_path 是内置文件服务对外的路径（/files/<随机十六进制名>）——
	// 名字由服务端生成，不含用户输入，杜绝路径穿越；磁盘文件名与它同名。
	`CREATE TABLE IF NOT EXISTS files (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  name       TEXT NOT NULL,
  file_type  TEXT NOT NULL DEFAULT 'firmware',
  url_path   TEXT NOT NULL,
  size       INTEGER NOT NULL DEFAULT 0,
  sha256     TEXT NOT NULL DEFAULT '',
  version    TEXT NOT NULL DEFAULT '',
  model      TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL DEFAULT ''
)`,
	// 15：光功率历史按保留期裁剪是「每次写入顺带 DELETE WHERE ts < 截止点」——
	// 没有 ts 索引的话，行数一大（几百台 × 每 10 分钟一条 × 30 天）每次写入都全表扫。
	`CREATE INDEX IF NOT EXISTS idx_optical_history_ts ON optical_history (ts)`,
	// 16：设备标签 —— 运维分组（按位置 / 用途 / 批次），概览页可按标签筛选。
	// 一个标签一行而不是逗号串：筛选与聚合都能走索引，联合主键天然去重，
	// 设备删除时随外键级联清掉。
	`CREATE TABLE IF NOT EXISTS device_tags (
  device_id INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  tag       TEXT NOT NULL,
  PRIMARY KEY (device_id, tag)
)`,
	// 17：按标签聚合全量标签（概览页筛选 chips 的计数）。
	`CREATE INDEX IF NOT EXISTS idx_device_tags_tag ON device_tags (tag)`,
}

// migrate 把库升到当前版本。幂等：已升过的直接跳过。
func (s *Store) migrate() error {
	var v int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return errors.Wrap(err, "读取数据库版本失败")
	}
	if v > len(migrations) {
		return fmt.Errorf("数据库版本 %d 高于本程序支持的 %d，请升级程序", v, len(migrations))
	}
	for i := v; i < len(migrations); i++ {
		if _, err := s.db.Exec(migrations[i]); err != nil {
			return errors.Wrap(err, fmt.Sprintf("数据库迁移 #%d 失败", i+1))
		}
		if _, err := s.db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			return err
		}
	}
	return nil
}

// GetSetting 读一个键值配置。
func (s *Store) GetSetting(k string) (string, bool, error) {
	var v string
	err := s.db.QueryRow(`SELECT v FROM settings WHERE k = ?`, k).Scan(&v)
	if stderrors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

// SetSetting 写一个键值配置。
func (s *Store) SetSetting(k, v string) error {
	_, err := s.db.Exec(`INSERT INTO settings (k, v) VALUES (?, ?)
		ON CONFLICT (k) DO UPDATE SET v = excluded.v`, k, v)
	return err
}

// GetOrCreateSetting 取出配置；没有就生成一个存下来（用于必须跨重启保持的密钥）。
func (s *Store) GetOrCreateSetting(k string, generate func() string) (string, bool, error) {
	if v, ok, err := s.GetSetting(k); err != nil || ok {
		return v, false, err
	}
	v := generate()
	if err := s.SetSetting(k, v); err != nil {
		return "", false, err
	}
	return v, true, nil
}

// Open 打开（必要时创建）数据库并建表。
func Open(path string) (*Store, error) {
	dsn := "file:" + path +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(1)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, errors.Wrap(err, "打开数据库失败")
	}
	// 轻量场景下最省心的做法：串行化访问，彻底避开 SQLITE_BUSY。
	// 设备规模上来后可以放宽并配合写队列，见 NFR-3。
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(0)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, errors.Wrap(err, "连接数据库失败")
	}

	s := &Store{db: db}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, errors.Wrap(err, "建表失败")
	}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// ErrClosed 是库已关闭后的统一错误。
//
// 关停竞态的兜底（配合 OEK-2 的「先排空后台协程、再关库」顺序）：
// 即便顺序对，后台回调里漏网的写也会撞上已关闭的库。让调用方能 errors.Is
// 识别出「这只是关停期的正常收尾」，而不是把它当成真故障上报。
var ErrClosed = stderrors.New("store: 已关闭")

// Close 关闭数据库，**幂等**（sync.Once，重复调用返回同个结果，不报错）。
//
// 为什么幂等：关停路径上关库可能被两条路径触发（正常 SIGTERM 与
// 面板「立即重启服务」），而 *sql.DB.Close 本身对第二次调用直接返回
// sql.ErrConnDone —— 那是个无信息量的假故障，会把真问题淹没在日志里。
//
// 签名保持 Close() error 而非 oekit 规范的 Close(ctx) error：
// *sql.DB 没有 ctx 语义的关闭动作（ctx 在该层用于「取消在飞的查询」，
// 而 database/sql 自 Go 1.9 起就由 ctx 管查询取消，不归 Close），
// 这里照抄 ctx 参数只会得到一个恒为 nil 的 context，是个假接口。
func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		s.closeErr = s.db.Close()
	})
	return s.closeErr
}

// Closed 报告库是否已关闭。关停流程用它判断后台协程是否还可能写库。
func (s *Store) Closed() bool { return s.closed.Load() }

// Ping 探活：给 /healthz 这类就绪探针用。
//
// 为什么单独给一个方法而不是把 *sql.DB 暴露出去：sql.DB 是并发安全的，
// 但把连接池交出去就等于把「能不能改 MaxOpenConns / 换驱动」也一并交出去了。
// 对外只给「还能不能读写」这一个问句，实现换了也不影响调用方。
func (s *Store) Ping(ctx context.Context) error {
	if s.closed.Load() {
		return ErrClosed
	}
	return s.db.PingContext(ctx)
}

// ---------- 时间与统计工具 ----------

// ts 统一用 RFC3339(UTC) 存时间：定长字符串，字典序即时间序，方便直接比大小。
func ts(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func parseTS(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// Stats 是概览页与 /metrics 要用的几个数字。
type Stats struct {
	Devices     int
	Online      int
	Probing     int // 正在被探测（超期未上报、已发过 Connection Request）
	Params      int
	PendingTask int
	RunningTask int
	FailedTask  int
	// 以下三项是流水表**当前存量**（都按保留上限裁剪过，不是累计计数）：
	// /metrics 里如实按 gauge 暴露，不冒充 counter。
	InformRecords int
	AlertRecords  int
	OpticalRows   int
}

// Stats 汇总当前状态。
func (s *Store) Stats() (Stats, error) {
	var st Stats
	row := s.db.QueryRow(`
		SELECT
			(SELECT COUNT(*) FROM devices),
			(SELECT COUNT(*) FROM devices WHERE online = 1),
			(SELECT COUNT(*) FROM devices WHERE online = 1 AND probe_count > 0),
			(SELECT COUNT(*) FROM device_params),
			(SELECT COUNT(*) FROM tasks WHERE status = 'pending'),
			(SELECT COUNT(*) FROM tasks WHERE status = 'running'),
			(SELECT COUNT(*) FROM tasks WHERE status = 'failed'),
			(SELECT COUNT(*) FROM informs),
			(SELECT COUNT(*) FROM alerts),
			(SELECT COUNT(*) FROM optical_history)`)
	if err := row.Scan(&st.Devices, &st.Online, &st.Probing, &st.Params,
		&st.PendingTask, &st.RunningTask, &st.FailedTask,
		&st.InformRecords, &st.AlertRecords, &st.OpticalRows); err != nil {
		return st, err
	}
	return st, nil
}
