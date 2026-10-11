package web

import (
	"net/http"
	"strings"
)

// metricsTarget 返回 /metrics 的可达地址（设置页展示与配置下载共用同一口径）：
// 面板监听优先，单端口时与 ACS 同端口；监听形如 ":17550" 时补上本机地址。
func (s *Server) metricsTarget() string {
	target := s.opt.Runtime.WebListen
	if target == "" {
		target = s.opt.Runtime.ACSListen
	}
	if strings.HasPrefix(target, ":") {
		target = "127.0.0.1" + target
	}
	return target
}

// handlePrometheusConfig 下载按当前实例生成的 Prometheus 抓取配置：
// 目标地址、抓取间隔都填好；配了 API Token 就带上 Bearer 段 —— 拿走即用。
// 与设置页同一套鉴权（登录管理员）：文件里含 token，不能匿名下载。
func (s *Server) handlePrometheusConfig(w http.ResponseWriter, r *http.Request) {
	var b strings.Builder
	b.WriteString("# LightCat 光猫管家 —— Prometheus 抓取配置\n")
	b.WriteString("# 用法：并进你的 prometheus.yml（或按需改造为 file_sd / 配置管理分发）。\n")
	b.WriteString("scrape_configs:\n")
	b.WriteString("  - job_name: lightcat\n")
	b.WriteString("    metrics_path: /metrics\n")
	b.WriteString("    scrape_interval: 30s\n")
	b.WriteString("    static_configs:\n")
	b.WriteString("      - targets: [\"" + s.metricsTarget() + "\"]\n")
	if tok := currentAPIToken(s.store, s.opt); tok != "" {
		b.WriteString("    authorization:\n")
		b.WriteString("      type: Bearer\n")
		b.WriteString("      credentials: \"" + tok + "\"\n")
	} else {
		b.WriteString("    # 未配置 API Token：面板开着登录保护时抓取会 401，先到设置页配一个\n")
	}
	w.Header().Set("Content-Type", "text/yaml; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="prometheus-lightcat.yml"`)
	_, _ = w.Write([]byte(b.String()))
}

// grafanaDashboardJSON 可直接导入 Grafana 的看板（Dashboards → Import）：
// 四张存量 stat + 三张时序图，覆盖 /metrics 暴露的全部十个指标。
// 数据源用 ${DS_PROMETHEUS} 输入变量，导入时让用户选自己的 Prometheus。
const grafanaDashboardJSON = `{
  "__inputs": [
    {
      "name": "DS_PROMETHEUS",
      "label": "Prometheus",
      "description": "LightCat 指标所在的数据源",
      "type": "datasource",
      "pluginId": "prometheus",
      "pluginName": "Prometheus"
    }
  ],
  "annotations": { "list": [] },
  "editable": true,
  "graphTooltip": 0,
  "links": [],
  "panels": [
    {
      "id": 1, "type": "stat", "title": "已纳管设备",
      "gridPos": { "x": 0, "y": 0, "w": 6, "h": 5 },
      "datasource": { "type": "prometheus", "uid": "${DS_PROMETHEUS}" },
      "targets": [ { "expr": "lightcat_devices", "refId": "A" } ],
      "fieldConfig": { "defaults": { "unit": "short" }, "overrides": [] },
      "options": { "colorMode": "value", "graphMode": "none", "reduceOptions": { "calcs": ["lastNotNull"] } }
    },
    {
      "id": 2, "type": "stat", "title": "在线设备",
      "gridPos": { "x": 6, "y": 0, "w": 6, "h": 5 },
      "datasource": { "type": "prometheus", "uid": "${DS_PROMETHEUS}" },
      "targets": [ { "expr": "lightcat_devices_online", "refId": "A" } ],
      "fieldConfig": { "defaults": { "unit": "short", "thresholds": { "mode": "absolute", "steps": [ { "color": "red", "value": null }, { "color": "green", "value": 1 } ] } }, "overrides": [] },
      "options": { "colorMode": "value", "graphMode": "none", "reduceOptions": { "calcs": ["lastNotNull"] } }
    },
    {
      "id": 3, "type": "stat", "title": "探测中（超期未上报）",
      "gridPos": { "x": 12, "y": 0, "w": 6, "h": 5 },
      "datasource": { "type": "prometheus", "uid": "${DS_PROMETHEUS}" },
      "targets": [ { "expr": "lightcat_devices_probing", "refId": "A" } ],
      "fieldConfig": { "defaults": { "unit": "short" }, "overrides": [] },
      "options": { "colorMode": "value", "graphMode": "none", "reduceOptions": { "calcs": ["lastNotNull"] } }
    },
    {
      "id": 4, "type": "stat", "title": "告警记录",
      "gridPos": { "x": 18, "y": 0, "w": 6, "h": 5 },
      "datasource": { "type": "prometheus", "uid": "${DS_PROMETHEUS}" },
      "targets": [ { "expr": "lightcat_alert_records_stored", "refId": "A" } ],
      "fieldConfig": { "defaults": { "unit": "short" }, "overrides": [] },
      "options": { "colorMode": "value", "graphMode": "none", "reduceOptions": { "calcs": ["lastNotNull"] } }
    },
    {
      "id": 5, "type": "timeseries", "title": "已入库参数行数",
      "gridPos": { "x": 0, "y": 5, "w": 12, "h": 8 },
      "datasource": { "type": "prometheus", "uid": "${DS_PROMETHEUS}" },
      "targets": [ { "expr": "lightcat_params_stored", "refId": "A" } ],
      "fieldConfig": { "defaults": { "unit": "short" }, "overrides": [] },
      "options": { "legend": { "displayMode": "list", "placement": "bottom" }, "tooltip": { "mode": "single" } }
    },
    {
      "id": 6, "type": "timeseries", "title": "Inform 记录（TR-069 上报与 Web 轮询）",
      "gridPos": { "x": 12, "y": 5, "w": 12, "h": 8 },
      "datasource": { "type": "prometheus", "uid": "${DS_PROMETHEUS}" },
      "targets": [ { "expr": "lightcat_inform_records_stored", "refId": "A" } ],
      "fieldConfig": { "defaults": { "unit": "short" }, "overrides": [] },
      "options": { "legend": { "displayMode": "list", "placement": "bottom" }, "tooltip": { "mode": "single" } }
    },
    {
      "id": 7, "type": "timeseries", "title": "任务队列",
      "gridPos": { "x": 0, "y": 13, "w": 12, "h": 8 },
      "datasource": { "type": "prometheus", "uid": "${DS_PROMETHEUS}" },
      "targets": [
        { "expr": "lightcat_tasks_pending", "refId": "A", "legendFormat": "待办" },
        { "expr": "lightcat_tasks_running", "refId": "B", "legendFormat": "运行中" },
        { "expr": "lightcat_tasks_failed", "refId": "C", "legendFormat": "失败" }
      ],
      "fieldConfig": { "defaults": { "unit": "short" }, "overrides": [] },
      "options": { "legend": { "displayMode": "list", "placement": "bottom" }, "tooltip": { "mode": "single" } }
    },
    {
      "id": 8, "type": "timeseries", "title": "光功率历史行数",
      "gridPos": { "x": 12, "y": 13, "w": 12, "h": 8 },
      "datasource": { "type": "prometheus", "uid": "${DS_PROMETHEUS}" },
      "targets": [ { "expr": "lightcat_optical_history_rows", "refId": "A" } ],
      "fieldConfig": { "defaults": { "unit": "short" }, "overrides": [] },
      "options": { "legend": { "displayMode": "list", "placement": "bottom" }, "tooltip": { "mode": "single" } }
    }
  ],
  "refresh": "30s",
  "schemaVersion": 39,
  "tags": ["lightcat", "tr069"],
  "templating": { "list": [] },
  "time": { "from": "now-6h", "to": "now" },
  "timepicker": {},
  "timezone": "browser",
  "title": "LightCat 光猫管家",
  "uid": "lightcat-overview",
  "version": 1
}`

// handleGrafanaDashboard 下载 Grafana 看板 JSON（导入即用，覆盖全部指标）。
func (s *Server) handleGrafanaDashboard(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="lightcat-grafana-dashboard.json"`)
	_, _ = w.Write([]byte(grafanaDashboardJSON))
}
