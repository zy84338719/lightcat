package web

import (
	"context"
	"io/fs"
	"mime"
	"path"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
)

// StaticPathPrefix 是面板静态资源的 URL 前缀。模板里以字面量写了它
// （templates/*.html 的 <link href="/static/style.css">），改这里要同步改模板。
const StaticPathPrefix = "/static"

// RegisterStaticRoutes 把面板静态资源挂成 Hertz 原生路由。
//
// 为什么值得单独做（而不是继续让 mux 兜）：面板每次打开都要取 style.css + app.js，
// 这是整条面板路径上最热的一条，而它没有任何动态逻辑。以前它走
// NewHertzEngine 的 "/*any" 兜底 —— 也就是整棵 http.ServeMux 经 adaptor.HertzHandler
// 进来，每次请求都要多走一跳 net/http.Request ⇄ hertz.RequestContext 的转换，
// 外加 http.FileServer 的一整套目录/重定向/Range 处理。
//
// 路由优先级："/static/*filepath" 是 Hertz 原生注册的具体路由，radix 树里
// 静态段优先于 "/*any" 通配，所以它压得住兜底（前提已由
// TestHealthEndpoints_BeatCatchAll 在 /healthz 上验证过同一条机制）。
//
// 注意别用 oekit/hertz 的 adaptor.StaticFSHandler 顶替：它内部最后一行仍然是
// adaptor.HertzHandler(http.StripPrefix(...FileServer))，**照样转换、照样不省**，
// 实测每请求 CPU 与改动前持平（见 docs/notes/loadtest.md）；
// 而且它不带 Cache-Control，会把「改完 app.js 刷新就用上」这条行为悄悄弄丢。
func RegisterStaticRoutes(e *server.Hertz) {
	sub, err := fs.Sub(assets, "static")
	if err != nil {
		// assets 是编译期 //go:embed 进来的常量，"static" 目录由同一条指令保证存在，
		// 这里失败只可能是 embed 声明被改坏 —— 那属于构建期错误，直接崩掉更好过线上 404。
		panic("web: 取出内嵌 static 目录失败: " + err.Error())
	}
	h := staticFSHandler(sub)
	e.GET(StaticPathPrefix+"/*filepath", h)
	// HEAD 必须显式注册：hertz 按方法精确匹配，只挂 GET 的话 HEAD 会掉进 "/*any"
	// 兜底，行为与改前不一致（改前 mux 的 "GET" 模式本身也匹配 HEAD）。
	e.HEAD(StaticPathPrefix+"/*filepath", h)
}

// staticFSHandler 直接读 embed.FS 并写 Hertz 响应，全程不经过 adaptor。
func staticFSHandler(sub fs.FS) app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		// filepath 是 "/static/" 之后剩下的那段，例如 "style.css"。
		name := strings.TrimPrefix(c.Param("filepath"), "/")
		data, err := fs.ReadFile(sub, name)
		if err != nil {
			// embed.FS 的 Open 会先过 fs.ValidPath：绝对路径、".."、越界一律拒，
			// 所以这里不存在穿越读的可能，err 一律当「没这个文件」。
			//
			// 404 不带 Cache-Control，与 http.FileServer 改前的表现一致
			// （net/http 的 serveError 会主动摘掉错误响应上的缓存头，
			// 免得一个临时 404 被浏览器缓存住）。
			c.Response.Header.Set("X-Content-Type-Options", "nosniff")
			c.Data(consts.StatusNotFound, "text/plain; charset=utf-8", []byte("404 page not found\n"))
			return
		}
		// Content-Type 用 mime.TypeByExtension —— 和 net/http.FileServer 同一个函数，
		// 所以扩展名到 MIME 的映射与改前逐字一致（含 charset）。
		ct := mime.TypeByExtension(path.Ext(name))
		if ct == "" {
			ct = "application/octet-stream"
		}
		// no-cache 是有意为之：没做指纹/ETag，缓存住就等于改了 app.js / style.css
		// 之后刷新还拿旧脚本。embed.FS 的 ModTime 恒为零，所以 FileServer 时代
		// 本来也发不出 Last-Modified（也就没有 304），这里保持一致。
		c.Response.Header.Set("Cache-Control", "no-cache")
		c.Data(consts.StatusOK, ct, data)
	}
}
