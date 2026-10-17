package web

import (
	"encoding/csv"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/oekit/pkg/errors"

	"github.com/zy84338719/lightcat/internal/store"
)

// /api/* 的服务层：逻辑唯一出处。
//
// 两种壳共用它：
//   - net/http 壳（Register 里注册的老路由，单元测试直接打它）；
//   - hz 生成路由的壳（biz/handler/api，生产流量走这条）。
// 壳只负责「解析请求 → 调这里 → 按自己的协议写回」，JSON 形状与状态码口径
// 由 415 项 e2e 断言钉死，任何一边都不许私自改。

// ACS 侧业务错误码（8xxx 段）。
//
// 编号约定来自 requirements §2.4 修正②：**8xxx 是 ACS 自己产生的错误码，
// 9xxx 留给 CPE→ACS 方向**（设备报上来的错误码），两边不共用号段，
// 所以看一个码就知道是谁说的。
//
// 为什么光有 HTTP 状态码不够：脚本轮询 /api 时，500 可能是「库写不进去」
// 也可能是「这台设备正在被改」；400 可能是「ID 格式错」也可能是「body 不是 JSON」。
// 状态码只能表达大类，抓取方没法据此分支，只能去 match 错误文案 —— 文案会改、会本地化，
// 拿来当接口契约必然烂掉。code 是稳定契约，error 文案不是。
const (
	CodeInternal      = 8000 // 内部错误（未归类；含库写不进去等）
	CodeBadRequest    = 8001 // 请求不合法：设备 ID 格式错、body 不是 JSON、必填参数为空
	CodeUnauthorized  = 8003 // 缺少或无效的凭据
	CodeNotFound      = 8004 // 目标不存在（设备不存在 / 端点不存在）
	CodeUnavailable   = 8005 // 该能力当前不可用（如未接入控制接口）
	CodeInternalWrite = 8006 // 持久化失败（写库/读库报错）
)

// APIError 带 HTTP 状态码 + ACS 业务码的错误：服务层不碰任何写侧，
// 错误与状态一起返回，两个壳各自翻译成本协议的错误格式。
//
// 底层是 oekit 的 BizError（业务码 + HTTP 码双轨），但这里覆写了 Error()
// 只回消息文本 —— 否则 BizError.Error() 会带上 "biz_code:8004 " 前缀，
// 直接暴到用户可见的 {"error": "..."} 里，既难看又会随文案漂移。
type APIError struct {
	Status int
	Code   int
	Err    *errors.BizError
}

func (e *APIError) Error() string { return e.Err.Message }

// codeForStatus 是状态码 → 业务码的默认映射。
//
// 只覆盖真正有语义的几个：能一一对应的直接给专属码，
// 其余（405/429 等本项目 API 不会产生的）都归到通用档，
// 宁可粗一点也不要编出一堆从没出现过的码 —— 那会让号段失去信号。
func codeForStatus(status int) int {
	switch status {
	case http.StatusBadRequest:
		return CodeBadRequest
	case http.StatusUnauthorized, http.StatusForbidden:
		return CodeUnauthorized
	case http.StatusNotFound:
		return CodeNotFound
	default:
		return CodeInternal
	}
}

// apiErr 造一个带 HTTP 状态码的错误，业务码按状态码取默认值。
func apiErr(status int, format string, args ...any) *APIError {
	return apiErrCode(codeForStatus(status), status, format, args...)
}

// apiErrCode 显式指定业务码造错误。
//
// 状态码说「大概是什么问题」，业务码说「确切是什么问题」：两者分开才能
// 在不改 HTTP 语义的前提下细分（比如 500 里区分「库挂了」与「能力不可用」）。
func apiErrCode(code, status int, format string, args ...any) *APIError {
	return &APIError{
		Status: status,
		Code:   code,
		Err:    errors.NewBizErrorWithStatus(code, status, fmt.Sprintf(format, args...)),
	}
}

// bizCodeOf 取任意错误上的 ACS 业务码；不是业务错误时返回 0。
//
// 给调用方（抓取器、中间件、日志）一个统一的读法，
// 不用先 errors.As 成 APIError 再看字段。
func bizCodeOf(err error) int {
	var ae *APIError
	if stderrors.As(err, &ae) {
		return ae.Code
	}
	var be *errors.BizError
	if stderrors.As(err, &be) {
		return be.BizCode
	}
	return 0
}

// fmtNull 把可空读数格式化成 CSV 单元格（NULL = 空，不编数）。
func fmtNull(v float64, valid bool) string {
	if !valid {
		return ""
	}
	return strconv.FormatFloat(v, 'f', 2, 64)
}

// ParseDeviceID 解析路径里的设备 ID（非法 = 400，两个壳口径一致）。
func ParseDeviceID(v string) (int64, error) {
	id, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, apiErr(http.StatusBadRequest, "设备 ID 非法")
	}
	return id, nil
}

// APIDevices 设备清单；tag 非空时按标签筛选（与面板同款过滤）。
func (s *Server) APIDevices(tag string) (any, error) {
	devices, err := s.store.ListDevices()
	if err != nil {
		return nil, apiErr(http.StatusInternalServerError, "%v", err)
	}
	if tag != "" {
		set, err := s.store.DevicesWithTag(tag)
		if err != nil {
			return nil, apiErr(http.StatusInternalServerError, "%v", err)
		}
		filtered := make([]*store.Device, 0, len(set))
		for _, d := range devices {
			if set[d.ID] {
				filtered = append(filtered, d)
			}
		}
		devices = filtered
	}
	return map[string]any{"data": devices, "count": len(devices)}, nil
}

// APIDevice 单台设备详情（参数 / 任务 / 待办 / 上报记录）。
func (s *Server) APIDevice(id int64) (any, error) {
	d, err := s.store.GetDevice(id)
	if err != nil {
		return nil, apiErr(http.StatusNotFound, "设备不存在")
	}
	params, _ := s.store.ListParams(id)
	tasks, _ := s.store.ListTasks(id, 50)
	pending, _ := s.store.PendingTaskCount(id)
	informs, _ := s.store.ListInforms(id, 20)
	return map[string]any{
		"device":        d,
		"params":        params,
		"tasks":         tasks,
		"pending_tasks": pending,
		"informs":       informs,
	}, nil
}

// APIAlerts 告警列表（最近 500 条，与面板告警页同源）。
func (s *Server) APIAlerts() (any, error) {
	alerts, err := s.store.ListAlerts(500)
	if err != nil {
		return nil, apiErr(http.StatusInternalServerError, "%v", err)
	}
	rows := make([]map[string]any, 0, len(alerts))
	for _, a := range alerts {
		row := map[string]any{
			"ID": a.ID, "DeviceID": a.DeviceID, "Kind": a.Kind, "Message": a.Message,
			"Notified": a.Notified, "CreatedAt": a.CreatedAt,
			"SerialNumber": a.SerialNumber, "ModelName": a.ModelName,
		}
		if a.Value.Valid {
			row["Value"] = a.Value.Float64
		}
		if a.Threshold.Valid {
			row["Threshold"] = a.Threshold.Float64
		}
		rows = append(rows, row)
	}
	return map[string]any{"data": rows, "count": len(rows)}, nil
}

// APIPresets 预设列表。
func (s *Server) APIPresets() (any, error) {
	presets, err := s.store.ListPresets()
	if err != nil {
		return nil, apiErr(http.StatusInternalServerError, "%v", err)
	}
	return map[string]any{"data": presets, "count": len(presets)}, nil
}

// APIFiles 固件/文件库列表。
func (s *Server) APIFiles() (any, error) {
	files, err := s.store.ListFiles()
	if err != nil {
		return nil, apiErr(http.StatusInternalServerError, "%v", err)
	}
	return map[string]any{"data": files, "count": len(files)}, nil
}

// APIFetch 入队「读取参数子树」（脚本入口；界面表单走 handleFetch）。
func (s *Server) APIFetch(id int64, body io.Reader) (any, error) {
	var req struct {
		Path    string   `json:"path"`
		Exclude []string `json:"exclude"`
		Max     int      `json:"max"`
	}
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		return nil, apiErr(http.StatusBadRequest, "请求体不是合法 JSON: %v", err)
	}
	if strings.TrimSpace(req.Path) == "" {
		return nil, apiErr(http.StatusBadRequest, "path 不能为空")
	}
	if s.ctrl == nil {
		return nil, apiErrCode(CodeUnavailable, http.StatusInternalServerError, "未接入控制接口")
	}
	if err := s.ctrl.FetchSubtree(id, req.Path, req.Exclude, req.Max); err != nil {
		return nil, apiErr(http.StatusInternalServerError, "%v", err)
	}
	return map[string]any{
		"queued":  true,
		"path":    req.Path,
		"exclude": req.Exclude,
		"max":     req.Max,
		"note":    "任务会在设备下次 Inform 时下发",
	}, nil
}

// APIFetchNames 入队「只枚举参数名」。
func (s *Server) APIFetchNames(id int64, body io.Reader) (any, error) {
	var req struct {
		Path      string `json:"path"`
		NextLevel bool   `json:"next_level"`
	}
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		return nil, apiErr(http.StatusBadRequest, "请求体不是合法 JSON: %v", err)
	}
	if strings.TrimSpace(req.Path) == "" {
		return nil, apiErr(http.StatusBadRequest, "path 不能为空")
	}
	if s.ctrl == nil {
		return nil, apiErrCode(CodeUnavailable, http.StatusInternalServerError, "未接入控制接口")
	}
	if err := s.ctrl.FetchNames(id, req.Path, req.NextLevel); err != nil {
		return nil, apiErr(http.StatusInternalServerError, "%v", err)
	}
	return map[string]any{
		"queued":     true,
		"path":       req.Path,
		"next_level": req.NextLevel,
		"note":       "只枚举名字不取值；任务会在设备下次 Inform 时下发",
	}, nil
}

// APIWifi 入队「采集无线概况」。
func (s *Server) APIWifi(id int64) (any, error) {
	if s.ctrl == nil {
		return nil, apiErrCode(CodeUnavailable, http.StatusInternalServerError, "未接入控制接口")
	}
	if err := s.ctrl.FetchWiFi(id); err != nil {
		return nil, apiErr(http.StatusInternalServerError, "%v", err)
	}
	return map[string]any{
		"queued": true,
		"note":   "任务会在设备下次 Inform 时下发；只采集 SSID/开关/信道/标准/加密/终端数等摘要字段",
	}, nil
}

// DevicesCSVRecords 设备清单 CSV 的表头与记录（BOM/编码由 WriteCSV 统一做）。
// 先取数后写头，404/500 才能在响应头落定前改走 JSON 错误。
func (s *Server) DevicesCSVRecords(tag string) ([]string, [][]string, error) {
	payload, err := s.APIDevices(tag)
	if err != nil {
		return nil, nil, err
	}
	devices := payload.(map[string]any)["data"].([]*store.Device)
	header := []string{
		"id", "serial_number", "oui", "manufacturer", "model", "software_version",
		"online", "last_inform_at", "param_count", "tags", "note",
		"rx_dbm", "tx_dbm", "temp_c",
	}
	recs := make([][]string, 0, len(devices))
	for _, d := range devices {
		var rx, tx, temp string
		if smp, ok, err := s.store.LastOpticalSample(d.ID); err == nil && ok {
			rx = fmtNull(smp.RxDbm.Float64, smp.RxDbm.Valid)
			tx = fmtNull(smp.TxDbm.Float64, smp.TxDbm.Valid)
			temp = fmtNull(smp.TempC.Float64, smp.TempC.Valid)
		}
		last := ""
		if !d.LastInformAt.IsZero() {
			last = d.LastInformAt.Format(time.RFC3339)
		}
		recs = append(recs, []string{
			strconv.FormatInt(d.ID, 10), d.SerialNumber, d.OUI, d.Manufacturer, d.ModelName,
			d.SoftwareVersion, strconv.FormatBool(d.Online), last, strconv.Itoa(d.ParamCount),
			strings.Join(d.Tags, "|"), d.Note, rx, tx, temp,
		})
	}
	return header, recs, nil
}

// OpticalCSVRecords 单台设备光功率历史的表头与记录（limit<=0 取默认 1000，上限 10000）。
func (s *Server) OpticalCSVRecords(id int64, limit int) ([]string, [][]string, error) {
	if _, err := s.store.GetDevice(id); err != nil {
		return nil, nil, apiErr(http.StatusNotFound, "设备不存在")
	}
	if limit <= 0 {
		limit = 1000
	}
	if limit > 10000 {
		limit = 10000
	}
	hist, err := s.store.OpticalHistory(id, limit)
	if err != nil {
		return nil, nil, apiErr(http.StatusInternalServerError, "%v", err)
	}
	header := []string{"ts", "rx_dbm", "tx_dbm", "temp_c", "volt_v", "bias_ma", "status"}
	recs := make([][]string, 0, len(hist))
	for _, o := range hist {
		recs = append(recs, []string{
			o.At.Format(time.RFC3339),
			fmtNull(o.RxDbm.Float64, o.RxDbm.Valid),
			fmtNull(o.TxDbm.Float64, o.TxDbm.Valid),
			fmtNull(o.TempC.Float64, o.TempC.Valid),
			fmtNull(o.VoltV.Float64, o.VoltV.Valid),
			fmtNull(o.BiasMa.Float64, o.BiasMa.Valid),
			o.Status,
		})
	}
	return header, recs, nil
}

// WriteCSV 写 UTF-8 BOM + CSV 记录（Excel 双击不乱码）。两个壳共用。
func WriteCSV(w io.Writer, header []string, recs [][]string) error {
	if _, err := w.Write([]byte{0xEF, 0xBB, 0xBF}); err != nil {
		return err
	}
	cw := csv.NewWriter(w)
	if err := cw.Write(header); err != nil {
		return err
	}
	for _, r := range recs {
		if err := cw.Write(r); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// APIAuthOK 机器端点的双通道鉴权（面板会话 cookie 或 Bearer token），
// 与 apiGuard 同一口径；hz 路由的 guard 中间件走这里。
//
// 配了 token 就必须校验，不因面板账号未启用而放行（理由见 web.go 的 apiGuard
// 注释）：否则「配了 token」看起来有防护、实际 /api 全网可读。
func (s *Server) APIAuthOK(sessionCookie, bearer string) bool {
	// 有面板登录态就放行（人在浏览器里用），与 net/http 侧 apiGuard 的口径一致。
	//
	// 注意判断「是否已登录」不能只看 LoggedInCookie 的返回值：面板未启用账号时
	// 它恒为 true。这里用 Enabled() 把两种情况分开：
	//   面板启用 → LoggedInCookie 才有意义（验签）；
	//   面板未启用 → 本来就是公开部署，不构成一道门。
	if s.opt.Auth != nil && s.opt.Auth.Enabled() && s.opt.Auth.LoggedInCookie(sessionCookie) {
		return true
	}
	// 配了 token 就必须校验，不因面板账号未启用而放行（理由见 web.go 的 apiGuard
	// 注释）：否则「配了 token」看起来有防护、实际 /api 全网可读。
	tok := currentAPIToken(s.store, s.opt)
	if tok == "" {
		// 没配 token：按面板自身的鉴权口径（未开账号 = 公开）
		return s.opt.Auth == nil || !s.opt.Auth.Enabled()
	}
	return bearer != "" && constantTimeEq(bearer, tok)
}
