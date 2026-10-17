// Command acs 是LightCat 光猫管家 的入口。
package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/oekit/concurrency/async"
	"github.com/oekit/concurrency/gopool"
	oekitversion "github.com/oekit/core/version"
	"github.com/oekit/pkg/errors"

	hertzserver "github.com/cloudwego/hertz/pkg/app/server"

	apibiz "github.com/zy84338719/lightcat/biz/handler/api"
	bizrouter "github.com/zy84338719/lightcat/biz/router"
	"github.com/zy84338719/lightcat/internal/alert"
	"github.com/zy84338719/lightcat/internal/config"
	"github.com/zy84338719/lightcat/internal/cwmp"
	"github.com/zy84338719/lightcat/internal/ontweb"
	"github.com/zy84338719/lightcat/internal/restart"
	"github.com/zy84338719/lightcat/internal/store"
	"github.com/zy84338719/lightcat/internal/web"
)

// backgroundDrainTimeout 是关停时等待后台协程收工的上限。
//
// 取 5 秒：Hertz 的优雅退出也是 5 秒，两者同量级，运维看到的最长停机窗口可预期。
// 超过它就强行关库 —— 宁可丢最后几条探测结果，也不能让一个卡住的 webhook
// 把整个进程吊着不退。
const backgroundDrainTimeout = 5 * time.Second

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

	// 全局生命周期 ctx：SIGINT / SIGTERM 一到就取消，所有后台循环据此收工。
	// 建得早一点 —— ontweb 轮询、告警推送这些后台任务从启动那一刻起就得能感知。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// TLS 配了就早验：证书读不了在启动时炸清楚，别等第一个请求才发现
	if cfg.TLSEnabled() {
		if _, err := tls.LoadX509KeyPair(cfg.TLSCert, cfg.TLSKey); err != nil {
			return errors.Wrap(err, "TLS 证书不可用（-tls-cert/-tls-key）")
		}
	}

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	// 进程级后台协程池（OEK-2）：巡检循环、离线探测、ontweb 轮询、webhook 推送
	// 都从这儿走，panic 由池兜住，关库前由 pool.Close 排空。
	//
	// 必须建在 alert / ontweb / cwmp 之前 —— 它们都要用它提交后台任务。
	bg := newBackgroundPool()
	// 兜底：任何一条提前 return 的错误路径都会走到这里。
	// 正常路径在 run 末尾已经显式关过；gopool 的 Close 幂等，重复调用无害。
	defer shutdownBackground(bg, log)

	// 自动重启会换进程，pid 文件得由**新进程**自己写才准（见 writePidFile）
	defer writePidFile()()

	// 上次进程被杀时留下的 running 任务，启动时退回待办（NFR-6）
	// ConnectionRequest 的密码：没配置就生成一个**存进库**。
	// 不能每次重启都换 —— 否则每次启动都要把新凭据重新写进设备，
	// 中间那段时间主动唤醒必然 401。
	if cfg.ConnReqPass == "" {
		v, created, err := st.GetOrCreateSetting("connreq_pass", func() string { return randomHex16() })
		if err != nil {
			return errors.Wrap(err, "生成 ConnectionRequest 密码失败")
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
	alert.WithBackground(alertMgr, bg)
	// 推送重试的退避等待受进程 ctx 约束：SIGINT/SIGTERM 到达时正在退避的
	// webhook 立刻收手，不会把关停窗口（5s）睡满。
	alert.WithContext(alertMgr, ctx)

	// ontweb 内建轮询（可选）：没有运营商超管时，服务端自己用光猫普通账号的
	// Web 接口定时采集（internal/ontweb）。进程退出即停，无需单独的调度器。
	// 注意：macOS 的 launchd 环境按二进制做本地网络授权，自编译二进制可能被
	// 静默拒绝 —— 那种环境请改用外层脚本 + `lightcat ontweb -from-dir`（见 README）。
	if cfg.OntWebURL != "" && cfg.OntWebInterval > 0 {
		cl, err := ontweb.New(cfg.OntWebURL, cfg.OntWebUser, cfg.OntWebPassword)
		if err != nil {
			return errors.Wrap(err, "ontweb 轮询初始化失败")
		}
		log.Info("ontweb 内建轮询已启用", "url", cfg.OntWebURL, "interval", cfg.OntWebInterval)
		// 轮询跑在后台协程池里（进程退出即排空，见 shutdownBackground）。
		// 两层保护：
		//   - 外层（池的任务）保住 tick 循环本身；
		//   - 内层只包单轮采集 —— ontweb 是真机抓取，厂商网页改版/返回怪结构
		//     都可能踩到 panic，单轮失败只跳过本轮，下一轮照常采。
		// 循环按 ctx 退出：原来写的是 `for range tick.C`，SIGTERM 根本叫不醒它，
		// 关库时它还在往 store 写（OEK-2）。
		bg.Go(func() {
			poll := func() {
				async.GoSafe(func() {
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
				})
			}
			poll()
			tick := time.NewTicker(cfg.OntWebInterval)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
					poll()
				}
			}
		})
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
		ProbeConcurrency:     cfg.ProbeConcurrency,
		MaxParamsPerRequest:  cfg.MaxParamsPerRequest,
	}, log)
	cwmp.WithBackground(srv, bg)

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
		return errors.Wrap(err, "准备面板登录密钥失败")
	}
	secret, _ := hex.DecodeString(secretHex)
	creds := web.NewCreds(authUser, authHash, secret)

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
		return errors.Wrap(err, "监听 "+acsAddr+" 失败")
	}
	defer acsLn.Close()
	var panelLn net.Listener
	if webAddr != "" && webAddr != acsAddr {
		panelLn, err = lnFor(webAddr)
		if err != nil {
			return errors.Wrap(err, "面板监听 "+webAddr+" 失败")
		}
		defer panelLn.Close()
	}

	// 当前进程手里活着的监听句柄：重启时把它们交出去（没变的端口靠它零断点接管）
	liveListeners := map[string]net.Listener{acsAddr: acsLn}
	if panelLn != nil {
		liveListeners[webAddr] = panelLn
	}

	// 面板账号没启用但配了 API Token：这组合最容易被误解 —— 设置页写着
	// 「Token 给 /api 与 /metrics 用」，运维会以为机器端点受保护，而实际上
	// 面板本来就是公开的（/api 含序列号、IP、全部参数）。说清楚比默默生效好。
	if !creds.Enabled() {
		if tok := settingOr(st, "api_token", cfg.APIToken); tok != "" {
			log.Warn("已配置 API Token 但未启用面板账号密码：/api 与 /metrics 现在需要携带该 Token 才能访问，" +
				"面板页面本身仍然对能访问本端口的人公开（含设备序列号、IP、参数值）")
		}
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

	// HTTP 引擎：CloudWeGo Hertz（监听收养、TLS、优雅退出都由它管；
	// 零断点重启靠把活着的 listener 交给新进程，Hertz 用 WithListener 收养 —— 见 internal/web/hertz.go）
	var tlsCfg *tls.Config
	if cfg.TLSEnabled() {
		tlsCfg, err = web.TLSCfgFromFiles(cfg.TLSCert, cfg.TLSKey)
		if err != nil {
			return errors.Wrap(err, "构造 TLS 配置失败")
		}
	}

	var acsEngine, panelEngine *hertzserver.Hertz
	// panelHertz 是**挂着面板的那台引擎**：单端口时就是 ACS 引擎（面板与 CWMP 共用），
	// 双端口时是面板引擎。/static/ 与 /api 都只挂这一台 —— 双端口时 ACS 端口只跑
	// CWMP 协议，本来就没有面板静态资源，不必顺手扩大它的服务面。
	var panelHertz *hertzserver.Hertz
	if panelLn == nil {
		// 默认：一个端口既接设备上报、又开面板（一套路由两用）
		mux := cwmpMux(false)
		srvWeb, err := web.Register(mux, st, srv, webOpts)
		if err != nil {
			return err
		}
		acsEngine = web.NewHertzEngine(requestLogger(log, mux), acsLn, tlsCfg)
		panelHertz = acsEngine
		// hz 生成的 /api 路由原生挂上 Hertz（比 adaptor 兜底更具体，优先命中）；
		// mux 里的老 /api 路由在生产被影子覆盖，只留给单元测试直接打
		apibiz.SetDeps(srvWeb)
		bizrouter.GeneratedRegister(acsEngine)
	} else {
		panelMux := http.NewServeMux()
		srvWeb, err := web.Register(panelMux, st, srv, webOpts)
		if err != nil {
			return err
		}
		acsEngine = web.NewHertzEngine(requestLogger(log, cwmpMux(true)), acsLn, tlsCfg)
		panelEngine = web.NewHertzEngine(requestLogger(log, panelMux), panelLn, tlsCfg)
		panelHertz = panelEngine
		// 双端口：/api 只挂面板侧（与 net/http 时代的 Register 口径一致）
		apibiz.SetDeps(srvWeb)
		bizrouter.GeneratedRegister(panelEngine)
	}

	// 面板静态资源（/static/）走 Hertz 原生路由，压过 "/*any" 兜底，
	// 省掉每个 CSS/JS 请求的 adaptor 转换跳。口径与 /api 一致：只挂面板侧。
	web.RegisterStaticRoutes(panelHertz)

	syncOekitVersion()

	// 存活/就绪/版本三个探针端点（/pingz、/healthz、/buildVersion）。
	// **两个引擎都挂**：单端口时只有一个，双端口时 ACS 端口与面板端口各一份。
	// 两个都挂是为了让探针配置不必猜端口 —— 之前 Dockerfile 的 HEALTHCHECK
	// 只能探面板端口，一到双端口部署就必须手工设 LIGHTCAT_HEALTH_PORT，
	// 否则 GET 打在 CWMP 端口上是 405，容器永远 unhealthy。
	// 静态路径优先于 NewHertzEngine 的 "/*any" 兜底，不会被面板 mux 截走。
	// 不需要鉴权 —— k8s 探针不带 cookie，也不该需要。
	for _, e := range dedupEngines(panelEngine, acsEngine) {
		web.RegisterHealthRoutes(e, st)
	}

	async.GoSafe(func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if panelEngine != nil {
			_ = panelEngine.Shutdown(shutdownCtx)
		}
		_ = acsEngine.Shutdown(shutdownCtx)
	})

	scheme := "http://"
	if cfg.TLSEnabled() {
		scheme = "https://"
	}
	uiURL := scheme + uiAddr(acsAddr) + "/"
	if panelEngine != nil {
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
		"panel_auth", creds.Enabled(),
		"engine", "hertz")

	if panelEngine != nil {
		log.Info("面板单独监听一个端口", "panel_listen", webAddr)
		log.Info("ACS 端口独享：设备向任何路径提交都会受理", "listen", acsAddr)
		async.GoSafe(func() {
			if err := panelEngine.Run(); err != nil {
				// 面板端口起不来要让人看见（多半是端口被占或被面板设置写错了）
				log.Error("面板监听失败", "addr", webAddr, "err", err)
				stop()
			}
		})
	}

	if err := acsEngine.Run(); err != nil {
		shutdownBackground(bg, log)
		return err
	}
	// 正常退出路径：引擎已停，接下来排空后台协程。defer 里还会再调一次
	// shutdownBackground，但 gopool.Close 幂等，所以只是多一句日志。
	shutdownBackground(bg, log)
	log.Info("ACS 已退出")
	return nil
}

// newBackgroundPool 建进程级后台协程池。
//
// 参数取值：常驻 64 个 worker 足够铺满巡检/探测/webhook 的常态并发；
// 队列 256 是突发缓冲。超出队列的任务 gopool 会退化成一次性 goroutine 直跑，
// 所以这里不会"悄悄丢任务"。
func newBackgroundPool() *gopool.Pool {
	return gopool.NewPool("lightcat-bg", gopool.PoolOption{
		MaxIdleWorkers: 64,
		WorkerMaxAge:   time.Minute,
		TaskChanBuffer: 256,
	})
}

// shutdownBackground 排空后台协程。**必须在 store.Close 之前调用。**
//
// 关停顺序是 OEK-2 的核心：巡检循环、离线探测、ontweb 轮询都要写库，
// 不先等它们收工就关库，这些写在关停窗口里会撞上 "database is closed"，
// 结果就是探测结果与 Inform 流水白丢（日志里只剩一堆无意义的写库错误）。
//
// 幂等：gopool.Close 可重复调用，所以既能在这里显式调，也能留给 defer 兜底。
func shutdownBackground(bg *gopool.Pool, log *slog.Logger) {
	if bg == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), backgroundDrainTimeout)
	defer cancel()
	started := time.Now()
	if err := bg.Close(ctx); err != nil {
		log.Warn("后台协程未在超时内排空（关库后可能仍有任务在写）",
			"timeout", backgroundDrainTimeout, "err", err)
		return
	}
	log.Info("后台协程已排空", "workers", bg.CurrentWorkers(), "took", time.Since(started).Round(time.Millisecond))
}

// syncOekitVersion 把面板展示的版本号同步给 oekit/core/version。
//
// /buildVersion 端点读的是后者（包级变量，默认 "dev"）。不对齐的话，用户
// 报障时贴出来的是 "dev"，等于没有版本信息 —— 运维与用户看到的必须是同一个号。
func syncOekitVersion() { oekitversion.Version = web.Version }

// dedupEngines 列出需要挂健康端点的引擎并去重。
//
// 单端口模式下 panelEngine 为 nil，两者本就是同一个 Hertz 实例；
// 同一个引擎上重复注册同名路由 Hertz 会直接 panic，所以这里必须去重。
func dedupEngines(panel, acs *hertzserver.Hertz) []*hertzserver.Hertz {
	if panel == nil || panel == acs {
		return []*hertzserver.Hertz{acs}
	}
	return []*hertzserver.Hertz{panel, acs}
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
