package web

import (
	"fmt"
	"net/http"
	"strings"
)

// handleMetrics 输出 Prometheus 文本格式（version 0.0.4）。
//
// 零依赖：几行 gauge 手拼就够（项目原则：不引第三方库）。所有数字来自
// store.Stats() 的**一条** SQL —— 抓取频率再高也不会把库扫疼。
//
// 口径如实：流水表（informs/alerts/optical_history）都按保留上限裁剪过，
// 暴露的是「当前存量」gauge，不冒充只增不减的 counter。
//
// 鉴权与 /api 同一套：面板登录态或 Bearer API token（见 Register 的 apiGuard）。
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	st, err := s.store.Stats()
	if err != nil {
		http.Error(w, "stats: "+err.Error(), http.StatusInternalServerError)
		return
	}
	var b strings.Builder
	gauge(&b, "lightcat_devices", "Managed devices (total).", st.Devices)
	gauge(&b, "lightcat_devices_online", "Managed devices currently online.", st.Online)
	gauge(&b, "lightcat_devices_probing", "Online devices being probed (missed their inform period).", st.Probing)
	gauge(&b, "lightcat_params_stored", "Device parameter rows stored.", st.Params)
	gauge(&b, "lightcat_tasks_pending", "Queued tasks waiting for a device session.", st.PendingTask)
	gauge(&b, "lightcat_tasks_running", "Tasks delivered, waiting for the device to answer.", st.RunningTask)
	gauge(&b, "lightcat_tasks_failed", "Stored tasks in failed state.", st.FailedTask)
	gauge(&b, "lightcat_inform_records_stored", "Inform records stored (capped by -inform-history-limit).", st.InformRecords)
	gauge(&b, "lightcat_alert_records_stored", "Alert records stored (threshold/offline alerts).", st.AlertRecords)
	gauge(&b, "lightcat_optical_history_rows", "Optical power history rows stored (trend charts).", st.OpticalRows)

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
}

// gauge 写一个 Prometheus gauge 指标（HELP + TYPE + 值）。
func gauge(b *strings.Builder, name, help string, v int) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s gauge\n%s %d\n", name, help, name, name, v)
}
