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
// onlineByDevice 取每台设备当前的在线状态，用来判断一条离线告警是否「已经恢复」。
func (s *Server) onlineByDevice() map[int64]bool {
	out := map[int64]bool{}
	devs, err := s.store.ListDevices()
	if err != nil {
		return out
	}
	for _, d := range devs {
		out[d.ID] = d.Online
	}
	return out
}

// alertRows 把告警转成模板行。online 是设备当前在线状态（id → online）。
//
// Resolved 只对**离线类**告警有意义：设备后来又报上了，那条「判定离线」就已经是
// 历史，不该再和正在发生的故障同色显示。原来两类都打红徽章，用户扫一眼告警页
// 会以为设备现在还离线着（告警是留痕的，不会自动消失）。
//
// 设备从库里被删掉时 map 里查不到，按「已恢复」处理 —— 人都不在了，谈不上还离线。
func alertRows(alerts []store.Alert, online map[int64]bool) []map[string]any {
	rows := make([]map[string]any, 0, len(alerts))
	for _, a := range alerts {
		resolved := false
		if a.Kind == alert.KindOffline {
			// 两值查找：分不出「设备仍离线」和「设备已从库里删掉」是同一个零值。
			// 后者按已恢复处理 —— 人都不在了，谈不上还离线。
			on, known := online[a.DeviceID]
			resolved = on || !known
		}
		rows = append(rows, map[string]any{
			"ID":        a.ID,
			"DeviceID":  a.DeviceID,
			"Serial":    a.SerialNumber,
			"Model":     a.ModelName,
			"Kind":      a.Kind,
			"KindLabel": alertKindLabel(a.Kind),
			"Message":   a.Message,
			"Notified":  a.Notified,
			"Resolved":  resolved,
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
	rows := alertRows(alerts, s.onlineByDevice())
	s.renderLang(w, r, "alerts.html", map[string]any{
		"Section": "alerts",
		"Alerts":  rows,
		"Count":   len(rows),
	})
}

// apiAlerts 返回告警 JSON（自动化/脚本用）。
// 读数摊平成普通数字（NullFloat64 直接序列化会变成 {Float64,Valid} 对象，没法用）。
func (s *Server) apiAlerts(w http.ResponseWriter, r *http.Request) {
	payload, err := s.APIAlerts()
	writeJSONOrError(w, payload, err)
}
