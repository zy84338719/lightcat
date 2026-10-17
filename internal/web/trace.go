package web

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"

	"github.com/zy84338719/lightcat/internal/trace"
)

// RegisterTraceMiddleware 给每个请求挂上 trace_id。
//
// 为什么它能一路传到 internal/cwmp：Hertz 路由链把 ctx 传下去，而面板与 CWMP
// 的处理器是经 adaptor.HertzHandler 包成 http.Handler 的 —— 那个包装用的是
// http.NewRequestWithContext(ctx, …)，**ctx 会原样进到 http.Request**。
// 所以 cwmp 那边直接 r.Context() 就能拿到，不必再解一次 header。
//
// 上游（反代 / WAF / 日志采集）已经带了 X-Trace-Id 就沿用，没带才自己生成：
// 一次请求穿过多层代理时 trace_id 不该每层都变，否则按 trace_id 搜日志会断成几段。
//
// 路由优先级无关：中间件对所有路由生效，不改变 radix 树的匹配结果。
func RegisterTraceMiddleware(e *server.Hertz) {
	e.Use(func(ctx context.Context, c *app.RequestContext) {
		id := string(c.GetHeader(trace.Header))
		if id == "" {
			id = trace.New()
		}
		// 回写到**响应头**，便于前端与排障直接看到（c.Set 存的是 RequestContext 的
		// 用户数据 map，不会变成 HTTP 头 —— 这里踩过一次）。
		c.Response.Header.Set(trace.Header, id)
		c.Next(trace.With(ctx, id))
	})
}
