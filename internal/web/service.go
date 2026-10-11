package web

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/zy84338719/lightcat/internal/store"
)

// /api/* 的服务层：逻辑唯一出处。
//
// 两种壳共用它：
//   - net/http 壳（Register 里注册的老路由，单元测试直接打它）；
//   - hz 生成路由的壳（biz/handler/api，生产流量走这条）。
// 壳只负责「解析请求 → 调这里 → 按自己的协议写回」，JSON 形状与状态码口径
// 由 415 项 e2e 断言钉死，任何一边都不许私自改。

// APIError 带 HTTP 状态码的错误：服务层不碰任何写侧，
// 错误与状态一起返回，两个壳各自翻译成本协议的错误格式。
type APIError struct {
	Status int
	Err    error
}

func (e *APIError) Error() string { return e.Err.Error() }

// apiErr 造一个带 HTTP 状态码的错误。
//
// 注意用 %v 而不是 %w：APIError.Err 只是给人看的**消息文本** —— 两个壳最终都把它
// 拼成 {"error": "..."} 返回，没有任何地方对它做 errors.Is/As，包装链是死代码。
// 用 %w 只会让人误以为这里存在可追溯的错误链。
func apiErr(status int, format string, args ...any) *APIError {
	return &APIError{Status: status, Err: fmt.Errorf(format, args...)}
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
		return nil, apiErr(http.StatusInternalServerError, "未接入控制接口")
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
		return nil, apiErr(http.StatusInternalServerError, "未接入控制接口")
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
		return nil, apiErr(http.StatusInternalServerError, "未接入控制接口")
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
func (s *Server) APIAuthOK(sessionCookie, bearer string) bool {
	if s.opt.Auth == nil || !s.opt.Auth.Enabled() {
		return true
	}
	// token 面板上可配（settings 表优先于启动参数），动态读取 —— 改完立即生效
	if tok := currentAPIToken(s.store, s.opt); tok != "" && bearer != "" && constantTimeEq(bearer, tok) {
		return true
	}
	return s.opt.Auth.LoggedInCookie(sessionCookie)
}
