package web

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/zy84338719/lightcat/internal/store"
)

// 设备标签与 CSV 导出。
//
// 标签是纯面板侧的运维分组（位置 / 用途 / 批次），只存本系统、不下发设备；
// CSV 是给「拿去做报表 / 喂给别的系统」的出口，走 apiGuard（面板登录态或 Bearer token）。

// handleDeviceTags 保存设备标签（整组替换）。
func (s *Server) handleDeviceTags(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if _, err := s.store.GetDevice(id); err != nil {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "表单解析失败", http.StatusBadRequest)
		return
	}
	tags := store.NormalizeTags(r.FormValue("tags"))
	if err := s.store.SetDeviceTags(id, tags); err != nil {
		redirectDeviceNotice(w, r, id, "保存标签失败: "+err.Error(), true)
		return
	}
	redirectDeviceNotice(w, r, id, "已更新标签", false)
}

// setCSVHeaders 布好 CSV 响应头并写 UTF-8 BOM —— Excel 双击打开中文不乱码。
func setCSVHeaders(w http.ResponseWriter, base string) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="%s-%s.csv"`, base, time.Now().Format("20060102-1504")))
	_, _ = w.Write([]byte{0xEF, 0xBB, 0xBF})
}

// fmtNull 把可空读数格式化成 CSV 单元格（NULL = 空，不编数）。
func fmtNull(v float64, valid bool) string {
	if !valid {
		return ""
	}
	return strconv.FormatFloat(v, 'f', 2, 64)
}

// apiDevicesCSV 导出设备清单（含最近一次光功率读数）。?tag= 与面板同款筛选。
func (s *Server) apiDevicesCSV(w http.ResponseWriter, r *http.Request) {
	devices, err := s.store.ListDevices()
	if err != nil {
		writeJSONError(w, err, http.StatusInternalServerError)
		return
	}
	if tag := strings.TrimSpace(r.URL.Query().Get("tag")); tag != "" {
		set, err := s.store.DevicesWithTag(tag)
		if err != nil {
			writeJSONError(w, err, http.StatusInternalServerError)
			return
		}
		filtered := make([]*store.Device, 0, len(set))
		for _, d := range devices {
			if set[d.ID] {
				filtered = append(filtered, d)
			}
		}
		devices = filtered
	}

	setCSVHeaders(w, "lightcat-devices")
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{
		"id", "serial_number", "oui", "manufacturer", "model", "software_version",
		"online", "last_inform_at", "param_count", "tags", "note",
		"rx_dbm", "tx_dbm", "temp_c",
	})
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
		_ = cw.Write([]string{
			strconv.FormatInt(d.ID, 10), d.SerialNumber, d.OUI, d.Manufacturer, d.ModelName,
			d.SoftwareVersion, strconv.FormatBool(d.Online), last, strconv.Itoa(d.ParamCount),
			strings.Join(d.Tags, "|"), d.Note, rx, tx, temp,
		})
	}
	cw.Flush()
}

// apiDeviceOpticalCSV 导出一台设备的光功率历史。?limit= 默认 1000、上限 10000。
func (s *Server) apiDeviceOpticalCSV(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSONError(w, fmt.Errorf("设备 ID 非法"), http.StatusBadRequest)
		return
	}
	if _, err := s.store.GetDevice(id); err != nil {
		writeJSONError(w, fmt.Errorf("设备不存在"), http.StatusNotFound)
		return
	}
	limit := 1000
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 10000 {
		limit = 10000
	}
	hist, err := s.store.OpticalHistory(id, limit)
	if err != nil {
		writeJSONError(w, err, http.StatusInternalServerError)
		return
	}

	setCSVHeaders(w, "lightcat-optical-"+strconv.FormatInt(id, 10))
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"ts", "rx_dbm", "tx_dbm", "temp_c", "volt_v", "bias_ma", "status"})
	for _, o := range hist {
		_ = cw.Write([]string{
			o.At.Format(time.RFC3339),
			fmtNull(o.RxDbm.Float64, o.RxDbm.Valid),
			fmtNull(o.TxDbm.Float64, o.TxDbm.Valid),
			fmtNull(o.TempC.Float64, o.TempC.Valid),
			fmtNull(o.VoltV.Float64, o.VoltV.Valid),
			fmtNull(o.BiasMa.Float64, o.BiasMa.Valid),
			o.Status,
		})
	}
	cw.Flush()
}
