// Package api 是 hz 生成的 /api/* 路由的 handler 层（idl/lightcat_api.proto）。
//
// 分工：本包只做「解析请求 → 调服务层 → 按 Hertz 协议写回」。业务逻辑的唯一出处是
// internal/web/service.go 的服务方法（net/http 壳与本壳共用，JSON 形状由 415 项 e2e 钉死）。
package api

import (
	"bytes"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	"github.com/zy84338719/lightcat/internal/web"
)

// deps 是装配层（cmd/lightcat）注入的服务层依赖。生成 handler 不自己 new 任何东西。
//
// 为什么用 atomic.Pointer 而不是裸包级变量（OEK-7）：
// 裸 `var Deps *web.Server` 靠「先 SetDeps、再 GeneratedRegister」的装配顺序保证安全 ——
// 但那是**约定**不是**保证**：Hertz 的路由注册与请求处理并发，一旦哪天有人调整顺序
// （比如注册时就发预热请求），裸指针就是一次 data race。atomic.Pointer 让
// 「注入」与「读」在语言层面成立，不再依赖谁记得顺序。
//
// 生命周期上它只在启动期写一次（之后只读），所以用 atomic.Pointer 而不是 RWMutex：
// 读路径无锁，Handler 里每次取依赖都是一次普通 load。
var deps atomic.Pointer[web.Server]

// SetDeps 注入服务层依赖。
func SetDeps(s *web.Server) { deps.Store(s) }

// Deps 取当前注入的服务层；未注入时返回 nil。
//
// 返回值供调用方判空（如鉴权中间件），nil 安全。
func Deps() *web.Server { return deps.Load() }

// depsOrFail 取服务层；未注入时直接写 500 并返回 nil，让调用方早退。
//
// 不注入就跑起来只可能是装配层漏了 SetDeps —— 那是个进程启动即定的错误，
// 每个 Handler 都为此判一次空，比在第一处 nil 解引用直接崩进程好定位得多。
func depsOrFail(c *app.RequestContext) *web.Server {
	if d := deps.Load(); d != nil {
		return d
	}
	c.JSON(consts.StatusInternalServerError, map[string]string{"error": "服务层未注入"})
	return nil
}

// writeAPI 把服务层的 (payload, error) 翻译成 Hertz 响应：
// APIError 带状态码与 ACS 业务码，其余错误一律 500。
// 响应体与 net/http 壳同形（{"code": …, "error": …}），口径单一。
func writeAPI(c *app.RequestContext, payload any, err error) {
	if err != nil {
		var ae *web.APIError
		if errors.As(err, &ae) {
			c.JSON(ae.Status, map[string]any{"code": ae.Code, "error": ae.Error()})
			return
		}
		c.JSON(consts.StatusInternalServerError,
			map[string]any{"code": web.CodeInternal, "error": err.Error()})
		return
	}
	c.JSON(consts.StatusOK, payload)
}

// writeCSV 走「先取数后写头」：记录取失败时还能改回 JSON 错误；
// 成功则 BOM + CSV 一次写出（与 net/http 壳共用 web.WriteCSV，口径单一）。
func writeCSV(c *app.RequestContext, base string, header []string, recs [][]string) {
	var buf bytes.Buffer
	if err := web.WriteCSV(&buf, header, recs); err != nil {
		c.JSON(consts.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	c.Response.Header.Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="%s-%s.csv"`, base, time.Now().Format("20060102-1504")))
	c.Data(consts.StatusOK, "text/csv; charset=utf-8", buf.Bytes())
}
