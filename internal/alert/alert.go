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
	"context"
	"database/sql"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/oekit/concurrency/async"
	"github.com/oekit/concurrency/gopool"
	"github.com/oekit/concurrency/retry"

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

	// ctx 是推送重试的父上下文（退避等待 + HTTP 请求都受它约束）。
	// 由 WithContext 接进程 ctx：SIGINT/SIGTERM 到达时正在退避的任务立刻收手，
	// 不会把关停窗口（backgroundDrainTimeout = 5s）睡满。
	ctx context.Context

	// pushRetry / pushBudget 是推送重试参数。做成字段而不是纯常量，
	// 是为了让测试能把退避缩到毫秒级 —— 用真实退避测重试逻辑，一个用例就要等 6 秒。
	pushCfg retry.Config
	budget  time.Duration // 单条告警推送的总时长上限（兜住「每次都卡满超时」的组合）
}

// WithBackground 让 webhook 推送走进程级协程池，便于关停前排空。
func WithBackground(m *Manager, bg *gopool.Pool) { m.bg = bg }

// WithContext 接进程 ctx，让推送重试的退避等待可被关停打断。
func WithContext(m *Manager, ctx context.Context) { m.ctx = ctx }

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
		pushCfg: retry.Config{
			Attempts:    webhookAttempts,
			Delay:       webhookBaseDelay,
			MaxDelay:    webhookMaxDelay,
			Exponential: true,
			Jitter:      webhookJitter,
			Retryable:   retryablePushErr,
		},
		budget: webhookTotalBudget,
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
		ctx := m.ctx
		if ctx == nil {
			ctx = context.Background()
		}
		notify := func() { m.notify(ctx, *a) }
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

// webhook 推送的重试参数。
//
// 为什么从「完全不重试」改成「有界重试」：原注释的理由是「告警主体在库里，面板
// 永远看得到；重试风暴对家庭网络没好处」。前半句是对的 —— 所以绝不该做无限重试；
// 但「完全不重试」对**幂等的 webhook 接收端**（Grafana / Alertmanager / 自建转发器
// 都是）偏保守：一次网络抖动导致的推送是白白丢的，而接收端根本不知道你来过。
//
// 四个上界缺一不可，少一个就退化成另一种灾难：
//   - 次数封顶：不给重试风暴留口子；
//   - 退避封顶：接收端长时间不可用时不至于把单次投递拖到天荒地老；
//   - 抖动：打散一批告警**同时**失败后的重试（否则它们会齐刷刷地一起再来一轮）；
//   - 总时长封顶：兜住「每次都卡满超时」的组合，仍早于关停窗口结束。
const (
	webhookAttempts    = 3 // 总尝试次数（含首次）：首次 + 2 次重试
	webhookBaseDelay   = 2 * time.Second
	webhookMaxDelay    = 8 * time.Second
	webhookJitter      = 500 * time.Millisecond
	webhookTotalBudget = 30 * time.Second // 单条告警推送的总时长上限
)

// pushErr 是一次推送失败的分类结果：只有「可重试」的失败才值得再来一次。
// 传输层错误里要单独把 ctx 取消/超时摘出来 —— 那不是接收端的错，重试没有意义。
type pushErr struct {
	err       error
	retryable bool
	status    int // HTTP 状态码；0 = 传输层失败（压根没拿到响应）
}

func (e *pushErr) Error() string { return e.err.Error() }
func (e *pushErr) Unwrap() error { return e.err }

// retryablePushErr 判断一条推送错误值不值得重试。
func retryablePushErr(err error) bool {
	var pe *pushErr
	if !stderrors.As(err, &pe) {
		// 不是我们分类出来的（例如 ctx 在退避等待中被取消）→ 不重试。
		return false
	}
	return pe.retryable
}

// pushOnce 推一次 webhook，返回带分类的错误（nil = 成功）。
func (m *Manager) pushOnce(ctx context.Context, a store.Alert, body []byte, attempt int) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.webhook, bytes.NewReader(body))
	if err != nil {
		// 构造请求失败是我们自己的配置问题，重试多少次都一样。
		return &pushErr{err: fmt.Errorf("构造推送请求失败：%w", err)}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.hc.Do(req)
	if err != nil {
		// ctx 取消/超时说明是**我们**在收手（或总预算到了），不是接收端的锅。
		if stderrors.Is(err, context.Canceled) || stderrors.Is(err, context.DeadlineExceeded) {
			return &pushErr{err: err}
		}
		// 连不上 / 超时 / 对方提前断链：典型的可重试。
		m.log.Warn("告警推送失败，准备重试", "alert_id", a.ID, "attempt", attempt, "err", err)
		return &pushErr{err: err, retryable: true}
	}
	defer resp.Body.Close()
	// 读掉响应体：不读只 Close 的话连接回不到池里，重试时每次都得重新握手。
	// 接收端出错时返回的往往是一小段错误文本，顺带 drain 掉就够了，不限量。
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
		return nil
	}
	// 429（限流）与 5xx（服务端故障）都值得再来一次；
	// 其余 4xx 是接收端**明确拒收**——重试一个被拒的请求毫无意义，
	// 反而会让对方再拒一次、白白占满退避窗口。
	retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
	m.log.Warn("告警推送被拒", "alert_id", a.ID, "status", resp.StatusCode, "retryable", retryable)
	return &pushErr{
		err:       fmt.Errorf("接收端返回 %s", resp.Status),
		retryable: retryable,
		status:    resp.StatusCode,
	}
}

// notify 推一次 webhook：**有界**重试 —— 只对可重试错误（连接失败 / 超时 / 429 / 5xx）
// 重试 2 次，指数退避 + 抖动，总时长封顶；退避等待可被关停打断。
//
// 告警主体在库里，面板永远看得到，所以这里不值得为它冒重试风暴的风险 ——
// 四个上界（次数 / 单次退避 / 抖动 / 总时长）就是为了守住这条。
func (m *Manager) notify(ctx context.Context, a store.Alert) {
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

	// 总时长封顶：即使每次尝试都卡满 hc.Timeout，也不会无限拖下去。
	budget := m.budget
	if budget <= 0 {
		budget = webhookTotalBudget
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	err = retry.Do(ctx, m.pushCfg, func(attempt int) error {
		return m.pushOnce(ctx, a, body, attempt)
	})
	if err != nil {
		m.log.Warn("告警推送最终失败（记录仍在面板可查）", "alert_id", a.ID, "err", err)
		return
	}
	if err := m.st.MarkAlertNotified(a.ID); err != nil {
		m.log.Warn("标记告警已推送失败", "alert_id", a.ID, "err", err)
	}
}
