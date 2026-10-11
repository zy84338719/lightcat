package web

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/zy84338719/lightcat/internal/store"
)

// 设备标签与 CSV 导出的 net/http 壳。
//
// 标签是纯面板侧的运维分组（位置 / 用途 / 批次），只存本系统、不下发设备；
// CSV 是给「拿去做报表 / 喂给别的系统」的出口。逻辑唯一出处在 service.go，
// hz 生成路由的壳（biz/handler/api）共用同一套服务方法。

// handleDeviceTags 保存设备标签（整组替换）。
func (s *Server) handleDeviceTags(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.handleNotFound(w, r)
		return
	}
	if _, err := s.store.GetDevice(id); err != nil {
		s.handleNotFound(w, r)
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

// setCSVHeaders 布好 CSV 响应头（BOM 由 WriteCSV 写，保证两个壳口径一致）。
func setCSVHeaders(w http.ResponseWriter, base string) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="%s-%s.csv"`, base, time.Now().Format("20060102-1504")))
}

// apiDevicesCSV 导出设备清单（含最近一次光功率读数）。?tag= 与面板同款筛选。
func (s *Server) apiDevicesCSV(w http.ResponseWriter, r *http.Request) {
	header, recs, err := s.DevicesCSVRecords(strings.TrimSpace(r.URL.Query().Get("tag")))
	if err != nil {
		writeJSONOrError(w, nil, err)
		return
	}
	setCSVHeaders(w, "lightcat-devices")
	if err := WriteCSV(w, header, recs); err != nil && s.log != nil {
		s.log.Warn("设备清单 CSV 写出失败", "err", err)
	}
}

// apiDeviceOpticalCSV 导出一台设备的光功率历史。?limit= 默认 1000、上限 10000。
func (s *Server) apiDeviceOpticalCSV(w http.ResponseWriter, r *http.Request) {
	id, err := ParseDeviceID(r.PathValue("id"))
	if err != nil {
		writeJSONOrError(w, nil, err)
		return
	}
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	header, recs, err := s.OpticalCSVRecords(id, limit)
	if err != nil {
		writeJSONOrError(w, nil, err)
		return
	}
	setCSVHeaders(w, "lightcat-optical-"+strconv.FormatInt(id, 10))
	if err := WriteCSV(w, header, recs); err != nil && s.log != nil {
		s.log.Warn("光功率历史 CSV 写出失败", "err", err)
	}
}
