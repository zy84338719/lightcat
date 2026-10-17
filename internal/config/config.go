// Package config 负责加载 ACS 的运行配置。
//
// 原则（需求文档 R-CFG-1/2）：所有跟运行环境绑定的东西都能配、都有合理默认，
// 探测不到就给明确默认或明确报错，不写死「某台机器的事实」。
package config

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config 是全部运行参数。
type Config struct {
	Listen string // CWMP（ACS）监听地址
	// WebListen 是面板监听地址：
	//   ""             = 与 Listen 同一个端口（一套路由两用，默认，保持轻量）
	//   与 Listen 相同  = 同上
	//   其它            = 另起一个监听只服务面板
	// 面板上也能改（存在 settings 表里，重启生效），那会优先于这里。
	WebListen string
	// WebUser / WebPass: 面板账号密码保护的**初始**凭据。
	// 只在库里还没有设置过时生效一次（种进 settings 表），之后以面板上改的为准。
	WebUser string
	WebPass string
	// WebAuthOff: 强制关闭面板鉴权（救急用：忘了面板密码又不想动库）。
	WebAuthOff bool
	Path       string // CWMP 端点路径
	ACSURL     string // 写回 CPE 的 ACS URL（仅用于展示/下发配置，本服务不依赖它）
	DBPath     string

	User     string // CPE 认证账号，为空则不校验
	Password string
	Realm    string

	SessionTimeout time.Duration
	// OfflineAfter 是「没有周期信息时」的兜底：多久没上报算离线。
	OfflineAfter time.Duration

	// 离线判定：设备周期上报是它自己说的（PeriodicInformInterval），
	// 所以「多久没上报算不正常」应该按周期算，而不是拍一个固定分钟数。
	// 规则：超过 周期×OfflineProbeFactor 没上报 → 主动发 Connection Request 探测，
	// 最多探测 OfflineProbeAttempts 次；还是没回音 → 标记离线（见 internal/cwmp/offline.go）。
	OfflineProbe         bool          // 关掉就退回「纯超时」判定
	OfflineProbeFactor   int           // 周期倍数，默认 2
	OfflineProbeAttempts int           // 最多探测几次，默认 3
	OfflineProbeInterval time.Duration // 两次探测的最小间隔
	OfflineProbeGrace    time.Duration // 最后一次探测后再等多久才判离线
	OfflineProbeMax      time.Duration // 周期×倍数 的上限（0 = 不设）
	OfflineCheckInterval time.Duration // 后台多久巡检一次
	// ProbeConcurrency 是离线探测同时在飞的 ConnectionRequest 上限，默认 20。
	// 一批设备同时掉线时每台都要等满 ConnReqTimeout，不设上限就是
	// N 台 = N 个并发连接（见 internal/cwmp/offline.go 的闸说明）。
	ProbeConcurrency int
	MaxBodyBytes     int64
	LogRawSOAP       bool
	AutoFetchInfo    bool
	AutoFetchWiFi    bool
	// AutoRefreshWiFi 是无线/终端概况的自动刷新间隔。
	// 面板上那个「采集」时间只有真的去读一次设备参数才会动，所以除了首次纳管，
	// 再按这个间隔自动来一次。0 = 只在首次纳管 / BOOTSTRAP 时采一次。
	AutoRefreshWiFi   time.Duration
	ProbeCapabilities bool

	// 主动唤醒（Connection Request）：给设备发一个 HTTP GET，让它立刻回连开一次会话，
	// 于是排队的任务不用等下一次周期上报。
	//
	// 认证上有个必须知道的坑：真机的 ConnectionRequestURL 要 HTTP Digest，
	// 而 ConnectionRequestUsername / Password 两个参数设备**不回读**（实测华为返回空串），
	// 所以只能由我们自己 provision（它们是可写的）。
	ConnReqEnabled bool
	ConnReqUser    string
	ConnReqPass    string
	ConnReqTimeout time.Duration

	// MaxParamsPerRequest: 单次 GetParameterValues 最多带多少个参数名。
	// 真机实测（华为 HN8145X6N）单次最多只回 256 个，超出静默丢弃，所以必须分批。
	MaxParamsPerRequest int
	// TaskHistoryLimit / InformHistoryLimit: 每台设备保留多少条历史记录（0 = 不限）。
	// tasks / informs 两张表只增不减，跑久了会把库撑大；历史只有最近的才有用。
	// 其中上报记录是增长最快的（每 120 秒一条 Inform）。
	TaskHistoryLimit   int
	InformHistoryLimit int

	// ontweb 内建轮询（可选）：没有运营商超管时，服务端自己用光猫普通账号的
	// Web 接口定时采集（见 internal/ontweb）。URL/账号 非空且间隔 > 0 才启用。
	// 密码建议用环境变量 ACS_ONTWEB_PASSWORD 传，不落命令行历史。
	OntWebURL      string
	OntWebUser     string
	OntWebPassword string
	OntWebInterval time.Duration

	LogLevel string
	LogJSON  bool

	// OpticalHistoryDays: 光功率历史（趋势图数据）保留多少天（0 = 不限）。
	// 每 10 分钟一条也就四千多条/月，默认 30 天足够画出趋势又不撑大库。
	OpticalHistoryDays int

	// FilesDir: 固件/配置文件库的磁盘目录（FR-11）。面板上传落盘到这里，
	// 内置文件服务（/files/<随机名>）也从这里读 —— CPE 的 Download 是自己来拉 URL。
	FilesDir string

	// TLSCert / TLSKey: HTTPS 证书与私钥路径（PEM）。两个都配了才启用 ——
	// ACS 与面板两个监听**一起**切到 TLS（运营商固件要求 ACS URL 走 https 的
	// 场景，面板顺带受益）。只配一个视为配置错误，启动时直接报错。
	TLSCert string
	TLSKey  string

	// APIToken: /api 与 /metrics 的 Bearer token（FR-13 / FR-14）。
	// 配了它，脚本与 Prometheus 不用面板登录态也能访问；空 = 只认登录态。
	APIToken string

	// 阈值告警（见 internal/alert）：读数越限 / 光口异常 / 设备离线时记入库，
	// 面板「告警」页可查；配了 webhook 再额外 POST 一份 JSON（接 Telegram /
	// 企业微信 / Home Assistant 等由用户自己中转，本项目不内置第三方 SDK）。
	AlertWebhook  string        // 推送地址（空 = 只记录不推送）
	AlertRxLow    float64       // 收光低于此值告警，dBm（0 = 关；GPON 灵敏度下限常见 -27~-28）
	AlertTempHigh float64       // 光模块温度高于此值告警，℃（0 = 关）
	AlertOffline  bool          // 设备判定离线时告警
	AlertCooldown time.Duration // 同设备同类告警的冷却窗口（持续越限不轰炸）
}

// Load 按「默认值 -> 环境变量 -> 命令行参数」的顺序装配配置。
func Load(args []string) (*Config, error) {
	c := &Config{
		Listen:               ":7547",
		WebListen:            "",
		Path:                 "/acs",
		DBPath:               "lightcat.db",
		Realm:                "acs",
		SessionTimeout:       60 * time.Second,
		OfflineAfter:         10 * time.Minute,
		OfflineProbe:         true,
		OfflineProbeFactor:   2,
		OfflineProbeAttempts: 3,
		OfflineProbeInterval: 15 * time.Second,
		OfflineProbeGrace:    30 * time.Second,
		OfflineProbeMax:      0,
		OfflineCheckInterval: 30 * time.Second,
		ProbeConcurrency:     20,
		MaxBodyBytes:         4 << 20,
		MaxParamsPerRequest:  200,
		TaskHistoryLimit:     500,
		InformHistoryLimit:   500,
		AutoFetchInfo:        true,
		AutoFetchWiFi:        true,
		AutoRefreshWiFi:      10 * time.Minute,
		ProbeCapabilities:    true,
		ConnReqEnabled:       true,
		ConnReqUser:          "acs",
		ConnReqTimeout:       10 * time.Second,
		LogLevel:             "info",
		OpticalHistoryDays:   30,
		FilesDir:             "files",
		AlertRxLow:           -27,
		AlertTempHigh:        70,
		AlertOffline:         true,
		AlertCooldown:        6 * time.Hour,
	}

	// 环境变量
	fromEnv(c)

	fs := flag.NewFlagSet("acs", flag.ContinueOnError)
	fs.StringVar(&c.Listen, "listen", c.Listen, "CWMP（ACS）监听地址")
	fs.StringVar(&c.WebListen, "web-listen", c.WebListen,
		"面板监听地址（留空 = 与 ACS 同一个端口）")
	fs.StringVar(&c.WebUser, "web-user", c.WebUser, "面板账号（启用面板鉴权时用；只在首次启动种一次）")
	fs.StringVar(&c.WebPass, "web-pass", c.WebPass, "面板密码（同上）")
	fs.StringVar(&c.Path, "path", c.Path, "CWMP 端点路径")
	fs.StringVar(&c.DBPath, "db", c.DBPath, "SQLite 数据库文件")
	fs.StringVar(&c.ACSURL, "acs-url", c.ACSURL, "ACS 自身 URL（写回 CPE 用）")
	fs.StringVar(&c.User, "user", c.User, "CPE 认证账号（留空不校验）")
	fs.StringVar(&c.Password, "password", c.Password, "CPE 认证密码")
	fs.DurationVar(&c.SessionTimeout, "session-timeout", c.SessionTimeout, "会话空闲超时")
	fs.DurationVar(&c.OfflineAfter, "offline-after", c.OfflineAfter,
		"多久没上报算离线（设备没上报周期信息时的兜底）")
	fs.BoolVar(&c.OfflineProbe, "offline-probe", c.OfflineProbe,
		"判定离线前主动发 Connection Request 探测；关掉=纯超时")
	fs.IntVar(&c.OfflineProbeFactor, "offline-probe-factor", c.OfflineProbeFactor,
		"超过 设备上报周期×这个倍数 没上报就开始探测")
	fs.IntVar(&c.OfflineProbeAttempts, "offline-probe-attempts", c.OfflineProbeAttempts,
		"最多探测几次（都没回音才判离线）")
	fs.DurationVar(&c.OfflineProbeInterval, "offline-probe-interval", c.OfflineProbeInterval,
		"两次探测之间的最小间隔")
	fs.DurationVar(&c.OfflineProbeGrace, "offline-probe-grace", c.OfflineProbeGrace,
		"最后一次探测后再等多久才判离线")
	fs.DurationVar(&c.OfflineProbeMax, "offline-probe-max", c.OfflineProbeMax,
		"周期×倍数 的上限（0=不设；设备上报的周期很大时用得上）")
	fs.DurationVar(&c.OfflineCheckInterval, "offline-check-interval", c.OfflineCheckInterval,
		"后台多久巡检一次在线状态")
	fs.IntVar(&c.ProbeConcurrency, "probe-concurrency", c.ProbeConcurrency,
		"离线探测同时在飞的 ConnectionRequest 上限（默认 20）")
	fs.Int64Var(&c.MaxBodyBytes, "max-body", c.MaxBodyBytes, "单请求体上限（字节）")
	fs.BoolVar(&c.LogRawSOAP, "log-soap", c.LogRawSOAP, "是否记录原始 SOAP 报文")
	fs.BoolVar(&c.AutoFetchInfo, "auto-fetch-info", c.AutoFetchInfo, "Inform 后自动取设备基本信息")
	fs.BoolVar(&c.AutoFetchWiFi, "auto-fetch-wifi", c.AutoFetchWiFi, "首次纳管/BOOTSTRAP 时自动采集无线概况（看板用）")
	fs.DurationVar(&c.AutoRefreshWiFi, "auto-refresh-wifi", c.AutoRefreshWiFi,
		"无线/终端概况自动刷新间隔（面板「采集」时间跟着动；0 = 只在首次纳管时采一次）")
	fs.BoolVar(&c.ProbeCapabilities, "probe-capabilities", c.ProbeCapabilities, "首次纳管时探测设备能力（如有没有 FTTR 子设备）")
	fs.BoolVar(&c.ConnReqEnabled, "connection-request", c.ConnReqEnabled, "允许主动唤醒设备（发 Connection Request）")
	fs.StringVar(&c.ConnReqUser, "connreq-user", c.ConnReqUser, "主动唤醒的用户名（会写进设备的 ConnectionRequestUsername）")
	fs.StringVar(&c.ConnReqPass, "connreq-pass", c.ConnReqPass, "主动唤醒的密码（会写进设备的 ConnectionRequestPassword；留空则由 main 生成并存在库里）")
	fs.IntVar(&c.MaxParamsPerRequest, "max-params-per-request", c.MaxParamsPerRequest,
		"单次 GetParameterValues 最多带多少个参数名（真机单次上限可能只有 256）")
	fs.IntVar(&c.TaskHistoryLimit, "task-history-limit", c.TaskHistoryLimit,
		"每台设备保留多少条任务记录（0 = 不限；只裁已结束的任务）")
	fs.IntVar(&c.InformHistoryLimit, "inform-history-limit", c.InformHistoryLimit,
		"每台设备保留多少条上报记录（0 = 不限）")
	fs.IntVar(&c.OpticalHistoryDays, "optical-history-days", c.OpticalHistoryDays,
		"光功率历史（趋势图）保留多少天（0 = 不限）")
	fs.StringVar(&c.FilesDir, "files-dir", c.FilesDir,
		"固件/配置文件库目录（面板上传与内置文件服务用）")
	fs.StringVar(&c.TLSCert, "tls-cert", c.TLSCert,
		"HTTPS 证书路径（PEM；与 -tls-key 一起配置后 ACS 与面板都切到 TLS）")
	fs.StringVar(&c.TLSKey, "tls-key", c.TLSKey, "HTTPS 私钥路径（PEM）")
	fs.StringVar(&c.APIToken, "api-token", c.APIToken,
		"/api 与 /metrics 的 Bearer token（脚本/Prometheus 用；空 = 只认面板登录态）")
	fs.StringVar(&c.AlertWebhook, "alert-webhook", c.AlertWebhook,
		"告警 webhook 地址（POST JSON；空 = 只记录不推送）")
	fs.Float64Var(&c.AlertRxLow, "alert-rx-low", c.AlertRxLow,
		"收光功率低于此值告警，dBm（0 = 关闭该规则）")
	fs.Float64Var(&c.AlertTempHigh, "alert-temp-high", c.AlertTempHigh,
		"光模块温度高于此值告警，℃（0 = 关闭该规则）")
	fs.BoolVar(&c.AlertOffline, "alert-offline", c.AlertOffline, "设备判定离线时告警")
	fs.DurationVar(&c.AlertCooldown, "alert-cooldown", c.AlertCooldown,
		"同设备同类告警的冷却窗口（默认 6h）")
	fs.StringVar(&c.OntWebURL, "ontweb-url", c.OntWebURL,
		"ontweb 轮询：光猫 Web 地址（配合 -ontweb-interval 启用内建轮询）")
	fs.StringVar(&c.OntWebUser, "ontweb-user", c.OntWebUser,
		"ontweb 轮询：光猫普通账号")
	fs.StringVar(&c.OntWebPassword, "ontweb-pass", c.OntWebPassword,
		"ontweb 轮询：光猫密码（建议用 ACS_ONTWEB_PASSWORD 环境变量）")
	fs.DurationVar(&c.OntWebInterval, "ontweb-interval", c.OntWebInterval,
		"ontweb 轮询间隔（如 10m；0 = 关闭内建轮询）")
	fs.StringVar(&c.LogLevel, "log-level", c.LogLevel, "日志级别 debug/info/warn/error")
	fs.BoolVar(&c.LogJSON, "log-json", c.LogJSON, "日志用 JSON 格式")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	if c.OfflineProbeFactor < 1 {
		c.OfflineProbeFactor = 1
	}
	if c.OfflineProbeAttempts < 0 {
		c.OfflineProbeAttempts = 0
	}
	if c.OfflineProbeInterval <= 0 {
		c.OfflineProbeInterval = 15 * time.Second
	}
	if c.OfflineCheckInterval <= 0 {
		c.OfflineCheckInterval = 30 * time.Second
	}
	if c.ProbeConcurrency <= 0 {
		c.ProbeConcurrency = 20
	}
	c.Path = normalizePath(c.Path)
	if c.Listen == "" {
		return nil, fmt.Errorf("监听地址不能为空")
	}
	if c.DBPath == "" {
		return nil, fmt.Errorf("数据库路径不能为空")
	}
	// TLS 要么都配、要么都不配：只配一半多半是笔误，启动时就拦住
	if (c.TLSCert == "") != (c.TLSKey == "") {
		return nil, fmt.Errorf("-tls-cert 与 -tls-key 必须成对配置（只配了一个）")
	}
	return c, nil
}

// TLSEnabled 表示是否启用 HTTPS（ACS 与面板一起切）。
func (c *Config) TLSEnabled() bool { return c.TLSCert != "" && c.TLSKey != "" }

func fromEnv(c *Config) {
	if v := os.Getenv("ACS_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("ACS_PATH"); v != "" {
		c.Path = v
	}
	if v := os.Getenv("ACS_URL"); v != "" {
		c.ACSURL = v
	}
	if v := os.Getenv("ACS_DB"); v != "" {
		c.DBPath = v
	}
	if v := os.Getenv("ACS_USER"); v != "" {
		c.User = v
	}
	if v := os.Getenv("ACS_PASSWORD"); v != "" {
		c.Password = v
	}
	if v := os.Getenv("ACS_REALM"); v != "" {
		c.Realm = v
	}
	if v := os.Getenv("ACS_LOG_LEVEL"); v != "" {
		c.LogLevel = v
	}
	if v := os.Getenv("ACS_LOG_JSON"); v != "" {
		c.LogJSON = parseBool(v)
	}
	if v := os.Getenv("ACS_LOG_SOAP"); v != "" {
		c.LogRawSOAP = parseBool(v)
	}
	if v := os.Getenv("ACS_AUTO_FETCH_INFO"); v != "" {
		c.AutoFetchInfo = parseBool(v)
	}
	if v := os.Getenv("ACS_AUTO_FETCH_WIFI"); v != "" {
		c.AutoFetchWiFi = parseBool(v)
	}
	if v := os.Getenv("ACS_AUTO_REFRESH_WIFI"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			c.AutoRefreshWiFi = d
		}
	}
	if v := os.Getenv("ACS_PROBE_CAPABILITIES"); v != "" {
		c.ProbeCapabilities = parseBool(v)
	}
	if v := os.Getenv("ACS_CONNREQ"); v != "" {
		c.ConnReqEnabled = parseBool(v)
	}
	if v := os.Getenv("ACS_CONNREQ_USER"); v != "" {
		c.ConnReqUser = v
	}
	if v := os.Getenv("ACS_CONNREQ_PASS"); v != "" {
		c.ConnReqPass = v
	}
	if v := os.Getenv("ACS_SESSION_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			c.SessionTimeout = d
		}
	}
	if v := os.Getenv("ACS_OFFLINE_AFTER"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			c.OfflineAfter = d
		}
	}
	if v := os.Getenv("ACS_OFFLINE_PROBE"); v != "" {
		c.OfflineProbe = parseBool(v)
	}
	if v := os.Getenv("ACS_OFFLINE_PROBE_FACTOR"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.OfflineProbeFactor = n
		}
	}
	if v := os.Getenv("ACS_OFFLINE_PROBE_ATTEMPTS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.OfflineProbeAttempts = n
		}
	}
	if v := os.Getenv("ACS_OFFLINE_PROBE_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			c.OfflineProbeInterval = d
		}
	}
	if v := os.Getenv("ACS_OFFLINE_PROBE_GRACE"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			c.OfflineProbeGrace = d
		}
	}
	if v := os.Getenv("ACS_OFFLINE_PROBE_MAX"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			c.OfflineProbeMax = d
		}
	}
	if v := os.Getenv("ACS_OFFLINE_CHECK_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			c.OfflineCheckInterval = d
		}
	}
	if v := os.Getenv("ACS_PROBE_CONCURRENCY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.ProbeConcurrency = n
		}
	}
	if v := os.Getenv("ACS_MAX_PARAMS_PER_REQUEST"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.MaxParamsPerRequest = n
		}
	}
	if v := os.Getenv("ACS_WEB_LISTEN"); v != "" {
		c.WebListen = v
	}
	if v := os.Getenv("ACS_WEB_USER"); v != "" {
		c.WebUser = v
	}
	if v := os.Getenv("ACS_WEB_PASS"); v != "" {
		c.WebPass = v
	}
	if v := os.Getenv("ACS_WEB_AUTH"); strings.EqualFold(v, "off") || v == "0" || strings.EqualFold(v, "false") {
		c.WebAuthOff = true
	}
	if v := os.Getenv("ACS_TASK_HISTORY_LIMIT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.TaskHistoryLimit = n
		}
	}
	if v := os.Getenv("ACS_INFORM_HISTORY_LIMIT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.InformHistoryLimit = n
		}
	}
	if v := os.Getenv("ACS_OPTICAL_HISTORY_DAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.OpticalHistoryDays = n
		}
	}
	if v := os.Getenv("ACS_FILES_DIR"); v != "" {
		c.FilesDir = v
	}
	if v := os.Getenv("ACS_TLS_CERT"); v != "" {
		c.TLSCert = v
	}
	if v := os.Getenv("ACS_TLS_KEY"); v != "" {
		c.TLSKey = v
	}
	if v := os.Getenv("ACS_API_TOKEN"); v != "" {
		c.APIToken = v
	}
	if v := os.Getenv("ACS_ALERT_WEBHOOK"); v != "" {
		c.AlertWebhook = v
	}
	if v := os.Getenv("ACS_ALERT_RX_LOW"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			c.AlertRxLow = f
		}
	}
	if v := os.Getenv("ACS_ALERT_TEMP_HIGH"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			c.AlertTempHigh = f
		}
	}
	if v := os.Getenv("ACS_ALERT_OFFLINE"); v != "" {
		c.AlertOffline = parseBool(v)
	}
	if v := os.Getenv("ACS_ALERT_COOLDOWN"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			c.AlertCooldown = d
		}
	}
	if v := os.Getenv("ACS_ONTWEB_URL"); v != "" {
		c.OntWebURL = v
	}
	if v := os.Getenv("ACS_ONTWEB_USER"); v != "" {
		c.OntWebUser = v
	}
	if v := os.Getenv("ACS_ONTWEB_PASSWORD"); v != "" {
		c.OntWebPassword = v
	}
	if v := os.Getenv("ACS_ONTWEB_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			c.OntWebInterval = d
		}
	}
	if v := os.Getenv("ACS_MAX_BODY"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			c.MaxBodyBytes = n
		}
	}
}

func normalizePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return "/acs"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
}

func parseBool(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on", "y":
		return true
	}
	return false
}
