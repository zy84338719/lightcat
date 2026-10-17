package web

import (
	"crypto/tls"
	"net"
	"net/http"
	"time"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/adaptor"
	"github.com/cloudwego/hertz/pkg/common/config"
)

// CloudWeGo Hertz 引擎装配。
//
// 重构分两步走（保证每一步都可验证、树常绿）：
//  1. 本文件：Hertz 接管监听/生命周期（含零断点重启的 listener 交接、TLS、优雅退出），
//     既有 http.Handler 树经 adaptor.HertzHandler 整体挂载 —— 行为零变化；
//  2. 后续把面板/API 处理器逐组改写成 Hertz 原生 context handler（路由表、中间件、
//     biz 分层），adaptor 只留给 internal/cwmp 协议内核（它是 http.Handler 形态的
//     会话状态机，经 415 项 e2e 与 GenieACS 互通验证，不值得为换引擎重写）。
//
// 为什么 WithListener 是关键：面板「立即重启服务」的零断点交接把活着的
// net.Listener 交给新进程（restart.go），Hertz 必须能收养现成 listener 才能保留
// 这个能力 —— WithListener 正是为此。
func NewHertzEngine(h http.Handler, ln net.Listener, tlsCfg *tls.Config) *server.Hertz {
	opts := []config.Option{
		server.WithListener(ln),
		// 优雅退出窗口：与 http.Server 时代的 5s Shutdown 同口径
		server.WithExitWaitTime(5 * time.Second),
		// 请求体上限放宽到 128MB：面板固件上传是 multipart 请求体（几十 MB 级）；
		// CWMP 侧的小上限由协议内核自己按 MaxBodyBytes 执行，不靠引擎一刀切
		server.WithMaxRequestBodySize(128 << 20),
		server.WithHandleMethodNotAllowed(true),
		server.WithDisablePrintRoute(true),
		// 空闲连接回收：设备侧 Inform 间隔分钟级，别把连接池攒太大
		server.WithIdleTimeout(90 * time.Second),
	}
	if tlsCfg != nil {
		opts = append(opts, server.WithTLS(tlsCfg))
	}
	e := server.New(opts...)
	// trace_id 中间件必须在**任何路由注册之前**挂上：Hertz 的 RouterGroup.Use 是把
	// 中间件追加进 group.Handlers，而 handle() 在**注册时**就把这条链快照下来 ——
	// 晚于路由注册再 Use，对已注册的路由不生效。所以放在工厂里、早于兜底路由。
	RegisterTraceMiddleware(e)
	handler := adaptor.HertzHandler(h)
	// 根路径与通配都挂：Hertz 的 "/" 只匹配根，"/*any" 兜其余全部路径
	e.Any("/", handler)
	e.Any("/*any", handler)
	return e
}

// TLSCfgFromFiles 由证书/密钥文件构造 tls.Config（启动时已验过可读，见 main）。
func TLSCfgFromFiles(certFile, keyFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, nil
}
