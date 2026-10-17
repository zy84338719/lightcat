package web

import (
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/oekit/middleware/hmw"
)

// RegisterTraceMiddleware 给每个请求挂上 trace_id。
//
// 直接用 oekit/middleware/hmw，而不是自己手搓一个 —— 它除了生成、透传、桥接到
// 标准 context，还做了一件我们手写版**漏掉**的事：对客户端传入的 X-Trace-Id 做
// 白名单校验（非空、长度 ≤ 64、字符集 [0-9a-zA-Z-]），不合规就丢弃并重新生成。
//
// 为什么这道校验要紧：trace_id 是客户端可控的，会原样进每一条日志行。没有校验时，
// 一个带 2000 字节 X-Trace-Id 的请求就能把该请求的十几行日志全部撑大（日志洪水），
// 再配上能换行的值还能伪造日志条目。实测过（手写版）。
//
// 为什么它能一路传到 internal/cwmp：Hertz 路由链把 ctx 传下去，而面板与 CWMP 的
// 处理器是经 adaptor.HertzHandler 包成 http.Handler 的 —— 那个包装用的是
// http.NewRequestWithContext(ctx, …)，**ctx 会原样进到 http.Request**。
// 所以 cwmp 那边直接 r.Context() 就能拿到，不必再解一次 header。
//
// 挂载位置：必须在**任何路由注册之前**。Hertz 的 RouterGroup.Use 是把中间件追加进
// group.Handlers，而 handle() 在注册时就快照了这条链 —— 晚挂对已注册路由无效。
// 所以调用点在 NewHertzEngine 工厂里，而不是各 Register* 里。
func RegisterTraceMiddleware(e *server.Hertz) {
	e.Use(hmw.Middleware()...)
}
