// Package alert 是阈值告警：光模块读数越限、光口异常、设备离线时留痕并推送。
//
// 定位（与项目「单二进制、零外部中间件」一致）：
//   - 记录先进库（alerts 表），面板「告警」页永远可查 —— webhook 只是可选的旁路；
//   - 推送走通用 webhook（POST JSON），接什么系统（Telegram 机器人、企业微信、
//     Home Assistant、n8n…）由用户自己中转，本包不内置任何第三方 SDK；
//   - 同类告警有冷却窗口，持续越限不会每轮采集都轰炸一遍。
//
// 触发点都在采集路径上（见 cmd/lightcat 的接线）：
//   - TR-069 参数入库钩子 / ontweb 轮询 → CheckOptical（收光过低、过温、光口状态）
//   - 离线巡检判定设备离线 → DeviceOffline
package alert

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/oekit/concurrency/async"
	"github.com/oekit/concurrency/gopool"

	"github.com/zy84338719/lightcat/internal/store"
)

func sqlFloat(v float64) sql.NullFloat64 { return sql.NullFloat64{Float64: v, Valid: true} }

// 告警类型（alerts.kind 的取值，面板按它翻译标签）。
const (
	KindRxLow     = "rx_low"     // 收光功率低于阈值（光路劣化：弯折、脏污、局端问题…）
	KindTempHigh  = "temp_high"  // 光模块温度高于阈值
	KindOpticDown = "optic_down" // 光口状态不是 Up（LOS 等，仅 Web 通道上报状态）
	KindOffline   = "offline"    // 设备判定离线（探测多次无响应）
)

// Thresholds 是告警规则。数值阈值为 0 表示该条规则关闭
// （0 dBm 的收光、0 ℃ 的过温上限都不是讲得通的配置，拿 0 当哨兵不会误伤）。
type Thresholds struct {
	RxLowDbm  float64 // 收光低于这个值告警（GPON 常见灵敏度下限 -27 ~ -28）
	TempHighC float64 // 光模块温度高于这个值告警
	Offline   bool    // 设备离线是否告警
}

// Manager 评估规则、写告警记录、推 webhook。并发安全（无内存状态，
// 冷却窗口直接查库），可以在会话钩子和轮询 goroutine 里共用一个实例。
type Manager struct {
	st       *store.Store
	log      *slog.Logger
	th       Thresholds
	cooldown time.Duration

	webhook string       // 空 = 只记录不推送
	hc      *http.Client // 推送专用：短超时，不跟随重定向（推送目标不该拐弯）

	// bg 提交 webhook 推送任务。接了进程级池就能在关库前排空（OEK-2）；
	// nil 时退回 async.GoSafe。
	bg *gopool.Pool
}

// WithBackground 让 webhook 推送走进程级协程池，便于关停前排空。
func WithBackground(m *Manager, bg *gopool.Pool) { m.bg = bg }

// New 构造 Manager。webhookURL 非空时必须是 http/https 且带主机名，
// 否则返回错误 —— 配置错误在启动时就炸，不留到第一次告警才发现。
func New(st *store.Store, log *slog.Logger, webhookURL string, th Thresholds, cooldown time.Duration) (*Manager, error) {
	if log == nil {
		log = slog.Default()
	}
	if cooldown <= 0 {
		cooldown = 6 * time.Hour
	}
	webhookURL = strings.TrimSpace(webhookURL)
	if webhookURL != "" {
		u, err := url.Parse(webhookURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("告警 webhook 地址无效（要 http/https 且带主机名）：%q", webhookURL)
		}
	}
	return &Manager{
		st:       st,
		log:      log,
		th:       th,
		cooldown: cooldown,
		webhook:  webhookURL,
		hc: &http.Client{
			Timeout: 10 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

// Enabled 表示有没有任何一条规则开着（全关时接线处可以直接不挂钩子）。
func (m *Manager) Enabled() bool {
	return m.th.RxLowDbm != 0 || m.th.TempHighC != 0 || m.th.Offline
}

// CheckOptical 按光模块读数评估阈值规则。读数缺失（NULL）的项跳过 ——
// 没读到不告警，宁缺毋编的同一口径。
//
// 消息里的数字先格式化成字符串再用 %s 拼：面板的 TS 智能翻译只认
// %d/%s 这类单字符占位符（见 internal/i18n 的 buildSmartPatterns），
// %.2f 会把它的正则拆坏。
func (m *Manager) CheckOptical(deviceID int64, s store.OpticalSample) {
	if m.th.RxLowDbm != 0 && s.RxDbm.Valid && s.RxDbm.Float64 < m.th.RxLowDbm {
		m.raise(deviceID, KindRxLow,
			fmt.Sprintf("收光功率 %s dBm，低于阈值 %s dBm",
				fmtFloat(s.RxDbm.Float64, 2), fmtFloat(m.th.RxLowDbm, 2)),
			s.RxDbm.Float64, m.th.RxLowDbm)
	}
	if m.th.TempHighC != 0 && s.TempC.Valid && s.TempC.Float64 > m.th.TempHighC {
		m.raise(deviceID, KindTempHigh,
			fmt.Sprintf("光模块温度 %s ℃，高于阈值 %s ℃",
				fmtFloat(s.TempC.Float64, 1), fmtFloat(m.th.TempHighC, 1)),
			s.TempC.Float64, m.th.TempHighC)
	}
	if st := strings.TrimSpace(s.Status); st != "" && !strings.EqualFold(st, "Up") {
		m.raise(deviceID, KindOpticDown,
			fmt.Sprintf("光口状态异常：%s", st), 0, 0)
	}
}

func fmtFloat(v float64, prec int) string {
	return strconv.FormatFloat(v, 'f', prec, 64)
}

// DeviceOffline 记一条设备离线告警（离线巡检判定后调用）。
func (m *Manager) DeviceOffline(deviceID int64) {
	if !m.th.Offline {
		return
	}
	m.raise(deviceID, KindOffline, "设备判定离线（多次探测无响应）", 0, 0)
}

// raise 是统一的落库 + 推送入口：冷却窗口内同类告警直接吞掉
// （持续越限不该每轮采集都记一条），落库成功后异步推 webhook。
func (m *Manager) raise(deviceID int64, kind, message string, value, threshold float64) {
	if last, ok, err := m.st.LastAlertAt(deviceID, kind); err == nil && ok &&
		time.Since(last) < m.cooldown {
		return
	}
	a := &store.Alert{DeviceID: deviceID, Kind: kind, Message: message}
	if kind == KindRxLow || kind == KindTempHigh {
		a.Value = sqlFloat(value)
		a.Threshold = sqlFloat(threshold)
	}
	if err := m.st.InsertAlert(a); err != nil {
		m.log.Warn("写告警记录失败", "device_id", deviceID, "kind", kind, "err", err)
		return
	}
	m.log.Warn("告警", "device_id", deviceID, "kind", kind, "message", message)
	if m.webhook != "" {
		notify := func() { m.notify(*a) }
		if m.bg != nil {
			m.bg.Go(notify)
		} else {
			async.GoSafe(notify)
		}
	}
}

// webhookPayload 是推给 webhook 的 JSON。字段名用蛇形小写，
// 收方（Telegram 中转 / Home Assistant / n8n…）好解析。
type webhookPayload struct {
	Source    string   `json:"source"` // 固定 "lightcat"
	Kind      string   `json:"kind"`
	Message   string   `json:"message"`
	Value     *float64 `json:"value,omitempty"`
	Threshold *float64 `json:"threshold,omitempty"`
	DeviceID  int64    `json:"device_id"`
	Serial    string   `json:"serial,omitempty"`
	Model     string   `json:"model,omitempty"`
	At        string   `json:"at"` // RFC3339 UTC
}

// notify 推一次 webhook。失败只记日志不重试 —— 告警主体在库里，
// 面板永远看得到；推送是尽力而为的旁路（重试风暴对家庭网络没好处）。
func (m *Manager) notify(a store.Alert) {
	p := webhookPayload{
		Source: "lightcat", Kind: a.Kind, Message: a.Message,
		DeviceID: a.DeviceID, At: a.CreatedAt.UTC().Format(time.RFC3339),
	}
	if a.Value.Valid {
		v := a.Value.Float64
		p.Value = &v
	}
	if a.Threshold.Valid {
		v := a.Threshold.Float64
		p.Threshold = &v
	}
	if d, err := m.st.GetDevice(a.DeviceID); err == nil {
		p.Serial, p.Model = d.SerialNumber, d.ModelName
	}
	body, err := json.Marshal(p)
	if err != nil {
		m.log.Warn("告警推送序列化失败", "alert_id", a.ID, "err", err)
		return
	}
	req, err := http.NewRequest(http.MethodPost, m.webhook, bytes.NewReader(body))
	if err != nil {
		m.log.Warn("告警推送构造请求失败", "alert_id", a.ID, "err", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.hc.Do(req)
	if err != nil {
		m.log.Warn("告警推送失败（记录仍在面板可查）", "alert_id", a.ID, "err", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		m.log.Warn("告警推送被拒", "alert_id", a.ID, "status", resp.StatusCode)
		return
	}
	if err := m.st.MarkAlertNotified(a.ID); err != nil {
		m.log.Warn("标记告警已推送失败", "alert_id", a.ID, "err", err)
	}
}
