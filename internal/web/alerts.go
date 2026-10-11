package web

import (
	"net/http"

	"github.com/zy84338719/lightcat/internal/alert"
	"github.com/zy84338719/lightcat/internal/store"
)

// alertKindLabel 把告警类型翻成面板上给人看的标签（i18n 的 key）。
func alertKindLabel(kind string) string {
	switch kind {
	case alert.KindRxLow:
		return "收光过低"
	case alert.KindTempHigh:
		return "光模块过温"
	case alert.KindOpticDown:
		return "光口异常"
	case alert.KindOffline:
		return "设备离线"
	}
	return kind // 未知类型原样显示（不编文案）
}

// alertRows 把告警记录铺成模板要用的行（告警页与设备详情页共用）。
func alertRows(alerts []store.Alert) []map[string]any {
	rows := make([]map[string]any, 0, len(alerts))
	for _, a := range alerts {
		rows = append(rows, map[string]any{
			"ID":        a.ID,
			"DeviceID":  a.DeviceID,
			"Serial":    a.SerialNumber,
			"Model":     a.ModelName,
			"Kind":      a.Kind,
			"KindLabel": alertKindLabel(a.Kind),
			"Message":   a.Message,
			"Notified":  a.Notified,
			"At":        a.CreatedAt,
		})
	}
	return rows
}

// handleAlerts 渲染「告警」页：最近的告警记录（时间倒序）。
func (s *Server) handleAlerts(w http.ResponseWriter, r *http.Request) {
	alerts, err := s.store.ListAlerts(200)
	if err != nil {
		http.Error(w, "读取告警失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	rows := alertRows(alerts)
	s.renderLang(w, r, "alerts.html", map[string]any{
		"Section": "alerts",
		"Alerts":  rows,
		"Count":   len(rows),
	})
}

// apiAlerts 返回告警 JSON（自动化/脚本用）。
// 读数摊平成普通数字（NullFloat64 直接序列化会变成 {Float64,Valid} 对象，没法用）。
func (s *Server) apiAlerts(w http.ResponseWriter, r *http.Request) {
	alerts, err := s.store.ListAlerts(500)
	if err != nil {
		writeJSONError(w, err, http.StatusInternalServerError)
		return
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
	writeJSON(w, map[string]any{"data": rows, "count": len(rows)})
}
