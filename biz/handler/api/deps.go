// Package api 是 hz 生成的 /api/* 路由的 handler 层（idl/lightcat_api.proto）。
//
// 分工：本包只做「解析请求 → 调服务层 → 按 Hertz 协议写回」。业务逻辑的唯一出处是
// internal/web/service.go 的服务方法（net/http 壳与本壳共用，JSON 形状由 415 项 e2e 钉死）。
package api

import (
	"bytes"
	"errors"
	"fmt"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	"github.com/zy84338719/lightcat/internal/web"
)

// Deps 由装配层（cmd/lightcat）注入。生成 handler 不自己 new 任何东西。
var Deps *web.Server

// SetDeps 注入服务层依赖。
func SetDeps(s *web.Server) { Deps = s }

// writeAPI 把服务层的 (payload, error) 翻译成 Hertz 响应：
// APIError 带状态码，其余错误一律 500；错误体与 net/http 壳同形（{"error": …}）。
func writeAPI(c *app.RequestContext, payload any, err error) {
	if err != nil {
		var ae *web.APIError
		if errors.As(err, &ae) {
			c.JSON(ae.Status, map[string]string{"error": ae.Err.Error()})
			return
		}
		c.JSON(consts.StatusInternalServerError, map[string]string{"error": err.Error()})
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
