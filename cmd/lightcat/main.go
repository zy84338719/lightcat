// Command acs 是LightCat 光猫管家 的入口。
package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/zy84338719/lightcat/internal/alert"
	"github.com/zy84338719/lightcat/internal/config"
	"github.com/zy84338719/lightcat/internal/cwmp"
	"github.com/zy84338719/lightcat/internal/ontweb"
	"github.com/zy84338719/lightcat/internal/restart"
	"github.com/zy84338719/lightcat/internal/store"
	"github.com/zy84338719/lightcat/internal/web"
)

func main() {
	// 子命令：`lightcat alias …` 管理「厂商私有参数 → 面板字段」的映射表（不进服务端主流程）。
	if len(os.Args) > 1 && os.Args[1] == "alias" {
		if err := runAlias(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "失败:", err)
			os.Exit(1)
		}
		return
	}
	// 子命令：`lightcat ontweb …` 华为光猫 Web 适配器 —— 普通账号也能把真机采进面板。
	if len(os.Args) > 1 && os.Args[1] == "ontweb" {
		if err := runOntWeb(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "失败:", err)
			os.Exit(1)
		}
		return
	}
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "启动失败:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cfg, err := config.Load(args)
	if err != nil {
		return err
	}
	log := newLogger(cfg)

	// TLS 配了就早验：证书读不了在启动时炸清楚，别等第一个请求才发现
	if cfg.TLSEnabled() {
		if _, err := tls.LoadX509KeyPair(cfg.TLSCert, cfg.TLSKey); err != nil {
			return fmt.Errorf("TLS 证书不可用（-tls-cert/-tls-key）: %w", err)
		}
	}

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	// 自动重启会换进程，pid 文件得由**新进程**自己写才准（见 writePidFile）
	defer writePidFile()()

	// 上次进程被杀时留下的 running 任务，启动时退回待办（NFR-6）
	// ConnectionRequest 的密码：没配置就生成一个**存进库**。
	// 不能每次重启都换 —— 否则每次启动都要把新凭据重新写进设备，
	// 中间那段时间主动唤醒必然 401。
	if cfg.ConnReqPass == "" {
		v, created, err := st.GetOrCreateSetting("connreq_pass", func() string { return randomHex16() })
		if err != nil {
			return fmt.Errorf("生成 ConnectionRequest 密码失败: %w", err)
		}
		cfg.ConnReqPass = v
		if created {
			log.Info("已自动生成 ConnectionRequest 密码并存入库（可用 -connreq-pass 覆盖）")
		}
	}

	// 历史保留上限（每台设备）：tasks / informs 都只增不减，跑久了会把库撑大。
	st.SetTaskHistoryLimit(cfg.TaskHistoryLimit)
	if n, err := st.PruneTasks(); err == nil && n > 0 {
		log.Info("已按保留上限清理任务历史", "deleted", n, "limit", cfg.TaskHistoryLimit)
	}
	st.SetInformHistoryLimit(cfg.InformHistoryLimit)
	if n, err := st.PruneInforms(); err == nil && n > 0 {
		log.Info("已按保留上限清理上报记录", "deleted", n, "limit", cfg.InformHistoryLimit)
	}
	// 光功率历史（趋势图数据）：写入时顺带按保留期裁剪
	st.SetOpticalHistoryRetention(cfg.OpticalHistoryDays)

	// 阈值告警：收光过低 / 光模块过温 / 光口异常 / 设备离线 → 入库留痕，
	// 面板「告警」页可查；配了 -alert-webhook 再额外 POST 一份 JSON（见 internal/alert）。
	alertMgr, err := alert.New(st, log, cfg.AlertWebhook, alert.Thresholds{
		RxLowDbm:  cfg.AlertRxLow,
		TempHighC: cfg.AlertTempHigh,
		Offline:   cfg.AlertOffline,
	}, cfg.AlertCooldown)
	if err != nil {
		return err
	}

	// ontweb 内建轮询（可选）：没有运营商超管时，服务端自己用光猫普通账号的
	// Web 接口定时采集（internal/ontweb）。进程退出即停，无需单独的调度器。
	// 注意：macOS 的 launchd 环境按二进制做本地网络授权，自编译二进制可能被
	// 静默拒绝 —— 那种环境请改用外层脚本 + `lightcat ontweb -from-dir`（见 README）。
	if cfg.OntWebURL != "" && cfg.OntWebInterval > 0 {
		cl, err := ontweb.New(cfg.OntWebURL, cfg.OntWebUser, cfg.OntWebPassword)
		if err != nil {
			return fmt.Errorf("ontweb 轮询初始化失败: %w", err)
		}
		log.Info("ontweb 内建轮询已启用", "url", cfg.OntWebURL, "interval", cfg.OntWebInterval)
		go func() {
			poll := func() {
				snap, err := cl.Snapshot()
				if err != nil {
					log.Warn("ontweb 采集失败", "err", err)
					return
				}
				// 软诊断如实报出来（某页解析不出、配方对不上…）—— 不静默失败
				for _, w := range snap.Warnings {
					log.Warn("ontweb 采集不完整", "detail", w)
				}
				if id, n, err := ontweb.ApplySnapshot(st, snap, cfg.OntWebURL); err != nil {
					log.Warn("ontweb 入库失败", "err", err)
				} else {
					log.Info("ontweb 采集完成", "params", n, "profile", snap.Profile)
					// Web 通道也参与阈值告警：用刚入库的那条光功率历史评估
					if last, ok, err := st.LastOpticalSample(id); err == nil && ok {
						alertMgr.CheckOptical(id, last)
					}
				}
			}
			poll()
			tick := time.NewTicker(cfg.OntWebInterval)
			defer tick.Stop()
			for range tick.C {
				poll()
			}
		}()
	}

	if n, err := st.ResetRunningTasks(); err == nil && n > 0 {
		log.Info("上次中断的任务已退回待办", "count", n)
	}

	srv := cwmp.NewServer(st, cwmp.Config{
		Path:                 cfg.Path,
		User:                 cfg.User,
		Password:             cfg.Password,
		SessionTimeout:       cfg.SessionTimeout,
		AutoFetchDeviceInfo:  cfg.AutoFetchInfo,
		AutoFetchWiFi:        cfg.AutoFetchWiFi,
		WiFiRefreshInterval:  cfg.AutoRefreshWiFi,
		ProbeCapabilities:    cfg.ProbeCapabilities,
		ConnReqEnabled:       cfg.ConnReqEnabled,
		ConnReqUser:          cfg.ConnReqUser,
		ConnReqPass:          cfg.ConnReqPass,
		ConnReqTimeout:       cfg.ConnReqTimeout,
		MaxBodyBytes:         cfg.MaxBodyBytes,
		LogRawSOAP:           cfg.LogRawSOAP,
		OfflineAfter:         cfg.OfflineAfter,
		OfflineProbe:         cfg.OfflineProbe,
		OfflineProbeFactor:   cfg.OfflineProbeFactor,
		OfflineProbeAttempts: cfg.OfflineProbeAttempts,
		OfflineProbeInterval: cfg.OfflineProbeInterval,
		OfflineProbeGrace:    cfg.OfflineProbeGrace,
		OfflineProbeMax:      cfg.OfflineProbeMax,
		OfflineCheckInterval: cfg.OfflineCheckInterval,
		MaxParamsPerRequest:  cfg.MaxParamsPerRequest,
	}, log)

	// TR-069 采集回执入库后，把认出来的光模块读数记进趋势历史并评估告警阈值。
	// 辨认与面板展示同一套口径（映射表 + 兜底启发式），图和表不会各说各话。
	srv.OnParamsStored = func(deviceID int64, params []store.Param) {
		recordOpticalHistory(st, log, alertMgr, deviceID, params)
	}
	// 离线巡检判定设备离线的那一刻记告警（探测多次无响应，见 offline.go）
	srv.OnDeviceOffline = alertMgr.DeviceOffline

	// 面板上改过的设置（settings 表）优先于启动参数 —— 管理员在界面上改完重启就该按它跑。
	acsAddr := settingOr(st, "listen", cfg.Listen)
	webAddr := settingOr(st, "web_listen", cfg.WebListen)
	authUser, authHash := "", ""
	// 首次启动时用启动参数里的账号密码种一次（之后以面板设置为准，环境变量不再覆盖）
	if _, ok, _ := st.GetSetting("web_seeded"); !ok {
		if cfg.WebUser != "" {
			authUser = cfg.WebUser
			if cfg.WebPass != "" {
				if h, err := web.HashPassword(cfg.WebPass); err == nil {
					authHash = h
				}
			}
			_ = st.SetSetting("web_user", authUser)
			_ = st.SetSetting("web_pass", authHash)
		}
		_ = st.SetSetting("web_seeded", "1")
	}
	if u, ok, _ := st.GetSetting("web_user"); ok {
		authUser = strings.TrimSpace(u)
	}
	if h, ok, _ := st.GetSetting("web_pass"); ok {
		authHash = strings.TrimSpace(h)
	}
	if cfg.WebAuthOff {
		// 救急：忘了面板密码又不想动库时，用环境变量强制关掉鉴权
		authUser, authHash = "", ""
	}
	// 面板登录态 cookie 的签名密钥：放库里，重启不失效
	//（否则每次重启都把所有人踢下线）。生成一次，之后一直用。
	secretHex, _, err := st.GetOrCreateSetting("panel_secret", func() string {
		return randomHex16() + randomHex16()
	})
	if err != nil {
		return fmt.Errorf("准备面板登录密钥失败: %w", err)
	}
	secret, _ := hex.DecodeString(secretHex)
	creds := web.NewCreds(authUser, authHash, secret)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 监听句柄在这里就建出来（而不是丢给 ListenAndServe 以后再说）：
	// 面板上「立即重启服务」要把这些 socket **交给新进程**——老进程还攥着没变的
	// 端口，新进程去重绑必然 address already in use。
	inherit := restart.DecodeInherit(os.Getenv(restart.InheritEnv))
	lnFor := func(addr string) (net.Listener, error) {
		if fd, ok := inherit[addr]; ok {
			if ln, err := restart.Adopt(fd); err == nil {
				log.Info("接管了上一个进程递过来的监听句柄", "addr", addr, "fd", fd)
				return ln, nil
			}
			log.Warn("接管监听句柄失败，改为自己绑", "addr", addr, "fd", fd)
		}
		return net.Listen("tcp", addr)
	}
	acsLn, err := lnFor(acsAddr)
	if err != nil {
		return fmt.Errorf("监听 %s 失败: %w", acsAddr, err)
	}
	defer acsLn.Close()
	var panelLn net.Listener
	if webAddr != "" && webAddr != acsAddr {
		panelLn, err = lnFor(webAddr)
		if err != nil {
			return fmt.Errorf("面板监听 %s 失败: %w", webAddr, err)
		}
		defer panelLn.Close()
	}

	// 当前进程手里活着的监听句柄：重启时把它们交出去（没变的端口靠它零断点接管）
	liveListeners := map[string]net.Listener{acsAddr: acsLn}
	if panelLn != nil {
		liveListeners[webAddr] = panelLn
	}

	webOpts := web.Options{
		Auth: creds,
		Log:  log,
		Runtime: web.RuntimeSettings{
			ACSListen: acsAddr,
			WebListen: webAddr,
			Path:      cfg.Path,
			ACSURL:    cfg.ACSURL,
			FilesDir:  cfg.FilesDir,
		},
		// /api 与 /metrics 的 Bearer token（脚本 / Prometheus 用）
		APIToken: cfg.APIToken,
		// 面板上改完监听地址可以「立即重启服务」，不用再去命令行
		Restart: restartService(st, log, acsAddr, webAddr, liveListeners, stop),
	}
	srv.StartJanitor(ctx)

	// CWMP 那套路由（设备侧）。
	//
	// dedicated = ACS 独享一个端口（面板在别的端口）：这时**任何路径都收**——
	// 运营商定制设备的 ACS URL 五花八门，有的配 /、有的配 /tr069、有的还带随机路径，
	// 我们没理由因为路径不同就把上报丢掉。反正这条端口只跑 CWMP，不会跟面板抢路由。
	//
	// 非 dedicated（面板共用同一个端口）时不能用兜底路由，否则面板页面会被 CWMP 抢走；
	// 这时只认配置的路径 + 根路径的 POST（真机/运维常见做法是把 ACS URL 配成
	// http://host:port/，不带 /acs）。
	cwmpMux := func(dedicated bool) *http.ServeMux {
		m := http.NewServeMux()
		if dedicated {
			// 双端口部署时设备只连 ACS 端口：固件取件端点必须挂在这边
			//（CPE 的 Download 是设备自己来拉 URL）。共用端口时 web.Register 会挂。
			m.HandleFunc("GET /files/{name}", web.FirmwareFileHandler(st, cfg.FilesDir))
			m.Handle("/", srv)
			return m
		}
		m.Handle(cfg.Path, srv)
		if cfg.Path != "/" {
			m.Handle("POST /{$}", srv)
		}
		return m
	}

	var acsSrv, panelSrv *http.Server
	if panelLn == nil {
		// 默认：一个端口既接设备上报、又开面板（一套路由两用）
		mux := cwmpMux(false)
		if err := web.Register(mux, st, srv, webOpts); err != nil {
			return err
		}
		acsSrv = &http.Server{
			Handler:           requestLogger(log, mux),
			ReadHeaderTimeout: 15 * time.Second,
		}
	} else {
		panelMux := http.NewServeMux()
		if err := web.Register(panelMux, st, srv, webOpts); err != nil {
			return err
		}
		acsSrv = &http.Server{
			Handler:           requestLogger(log, cwmpMux(true)),
			ReadHeaderTimeout: 15 * time.Second,
		}
		panelSrv = &http.Server{
			Handler:           requestLogger(log, panelMux),
			ReadHeaderTimeout: 15 * time.Second,
		}
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = acsSrv.Shutdown(shutdownCtx)
		if panelSrv != nil {
			_ = panelSrv.Shutdown(shutdownCtx)
		}
	}()

	scheme := "http://"
	if cfg.TLSEnabled() {
		scheme = "https://"
	}
	uiURL := scheme + uiAddr(acsAddr) + "/"
	if panelSrv != nil {
		uiURL = scheme + uiAddr(webAddr) + "/"
	}
	log.Info("ACS 已启动",
		"listen", acsAddr,
		"cwmp_endpoint", cfg.Path,
		"cwmp_url_alt", scheme+uiAddr(acsAddr)+"/",
		"ui", uiURL,
		"panel_listen", webAddr,
		"db", cfg.DBPath,
		"tls", cfg.TLSEnabled(),
		"cpe_auth", cfg.User != "",
		"panel_auth", creds.Enabled())

	// 配了 TLS 就两个监听一起切 HTTPS（运营商固件要求 ACS URL 走 https 的场景）；
	// 句柄交接的重启逻辑不受影响 —— ServeTLS 同样接管现成的 listener。
	serve := func(s *http.Server, ln net.Listener) error {
		if cfg.TLSEnabled() {
			return s.ServeTLS(ln, cfg.TLSCert, cfg.TLSKey)
		}
		return s.Serve(ln)
	}

	if panelSrv != nil {
		log.Info("面板单独监听一个端口", "panel_listen", webAddr)
		log.Info("ACS 端口独享：设备向任何路径提交都会受理", "listen", acsAddr)
		go func() {
			if err := serve(panelSrv, panelLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
				// 面板端口起不来要让人看见（多半是端口被占或被面板设置写错了）
				log.Error("面板监听失败", "addr", webAddr, "err", err)
				stop()
			}
		}()
	}

	if err := serve(acsSrv, acsLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	log.Info("ACS 已退出")
	return nil
}

// statusWriter 记住实际写出的状态码。
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// requestLogger 记录每个进来的 HTTP 请求。
//
// 为什么需要：真机接不上时，必须能区分「设备压根没打过来」、「打过来了但路径/方法不对」
// 和「打过来了但我们处理出错」这三种情况。所以：
//   - 所有请求在 debug 级别记录；
//   - 「打过来了但我们没接住」（404/405）在 warn 级别记录，这是最需要人看的信号。
func requestLogger(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		if sw.status == 0 {
			sw.status = http.StatusOK
		}
		dur := time.Since(start)

		switch {
		case sw.status == http.StatusNotFound || sw.status == http.StatusMethodNotAllowed:
			log.Warn("=> 有请求打进来但没被处理（多半是方法不对；共用端口时也可能是路径不在路由里）",
				"method", r.Method, "path", r.URL.Path, "status", sw.status,
				"from", r.RemoteAddr, "ua", r.UserAgent())
		case r.URL.Path != "/static/style.css" && r.URL.Path != "/static/app.js":
			log.Debug("HTTP",
				"method", r.Method, "path", r.URL.Path, "status", sw.status,
				"from", r.RemoteAddr, "dur", dur.Round(time.Millisecond), "ua", r.UserAgent())
		}
	})
}

func uiAddr(listen string) string {
	if strings.HasPrefix(listen, ":") {
		return "127.0.0.1" + listen
	}
	return listen
}

func newLogger(cfg *config.Config) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(cfg.LogLevel) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}
	var h slog.Handler
	if cfg.LogJSON {
		h = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		h = slog.NewTextHandler(os.Stdout, opts)
	}
	return slog.New(h)
}

// randomHex16 生成 16 字节的随机十六进制串（ConnectionRequest 密码用）。
func randomHex16() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// 拿不到随机数就退化成时间戳，至少不会空密码
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// recordOpticalHistory 把 TR-069 刚入库的一批参数里认出的光模块读数记进趋势历史。
//
// 两个细节：
//   - 一个读数都认不出来就不记 —— 无线/WAN 之类无关采集不该在历史里留空行；
//   - 同一次采集会被拆成十几批 GetParameterValues（真机单批上限 256 个参数名），
//     5 分钟窗口内合并成同一个点，不然趋势图上全是一轮采集的重复点。
func recordOpticalHistory(st *store.Store, log *slog.Logger, alerts *alert.Manager,
	deviceID int64, params []store.Param) {
	aliases, err := st.EnabledAliases()
	if err != nil {
		log.Debug("读光口映射表失败，只按启发式辨认", "device_id", deviceID, "err", err)
	}
	rd := web.OpticalReadingsFrom(aliases, params)
	if rd.Empty() {
		return
	}
	if err := st.RecordOpticalSample(deviceID, time.Now(),
		rd.Rx, rd.Tx, rd.TempC, rd.VoltV, rd.BiasMa, "", 5*time.Minute); err != nil {
		log.Warn("写光功率历史失败", "device_id", deviceID, "err", err)
		return
	}
	// 阈值告警按**这轮认出的读数**评估（合并窗口不影响告警判定：
	// 冷却窗口在 Manager 里挡重复）
	if alerts != nil {
		alerts.CheckOptical(deviceID, rd.Sample(""))
	}
}

// settingOr 读 settings 表里的值；没有或为空就用启动参数给的值。
func settingOr(st *store.Store, key, fallback string) string {
	if v, ok, err := st.GetSetting(key); err == nil && ok {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return fallback
}
