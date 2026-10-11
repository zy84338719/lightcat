package web

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/zy84338719/lightcat/internal/store"
)

// handleInventory 渲染「设备清单」：全部设备资产一张表。
//
// 与概览页的分工：概览页是运营看板（榜单 / 告警 / WiFi 概览，按筛选裁剪），
// 清单页是资产盘点 —— 不筛选、不裁列，存货的、离线的、历史上纳管过的都在，
// 连 WAN 口 IP、首次纳管、上报周期这些低频字段也一并列出，直到手动删除为止。
func (s *Server) handleInventory(w http.ResponseWriter, r *http.Request) {
	all, err := s.store.ListDevices()
	if err != nil {
		http.Error(w, "读取设备列表失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	stats, err := s.store.Stats()
	if err != nil {
		http.Error(w, "读取统计失败: "+err.Error(), http.StatusInternalServerError)
		return
	}

	wifiParams, opticalParams, err := s.store.SummaryParams()
	if err != nil {
		wifiParams, opticalParams = map[int64][]store.Param{}, map[int64][]store.Param{}
	}
	aliases, err := s.store.EnabledAliases()
	if err != nil {
		aliases = nil
	}
	opticalByDevice := map[int64]HostOptical{}
	hasOptical := false
	for id, ps := range opticalParams {
		o := hostOpticalFrom(aliases, ps)
		if !o.Has() {
			continue
		}
		opticalByDevice[id] = o
		hasOptical = true
	}
	// ontweb 通道的设备拿不到 PON 参数域，光读数从最近一次历史补
	//（与概览页同一套兜底口径；rx 没有就空着，宁缺毋编）。
	for _, d := range all {
		if _, ok := opticalByDevice[d.ID]; ok {
			continue
		}
		if smp, ok, err := s.store.LastOpticalSample(d.ID); err == nil && ok {
			o := HostOptical{}
			if smp.RxDbm.Valid {
				o.Rx = fmt.Sprintf("%.2f dBm", smp.RxDbm.Float64)
			}
			if smp.TxDbm.Valid {
				o.Tx = fmt.Sprintf("%.2f dBm", smp.TxDbm.Float64)
			}
			if o.Has() {
				opticalByDevice[d.ID] = o
				hasOptical = true
			}
		}
	}

	clientCounts := map[int64]int{}
	fttrCounts := map[int64]int{}
	for _, d := range all {
		tree := BuildClientTree(wifiParams[d.ID], nil)
		n := 0
		for _, g := range tree.Groups() {
			n += len(g.Clients)
		}
		clientCounts[d.ID] = n

		seen := map[string]bool{}
		for _, p := range wifiParams[d.ID] {
			if i := strings.Index(p.Name, "X_HW_APDevice."); i >= 0 {
				rest := p.Name[i+len("X_HW_APDevice."):]
				if j := strings.Index(rest, "."); j > 0 {
					seen[rest[:j]] = true
				}
			}
		}
		if len(seen) > 0 {
			fttrCounts[d.ID] = len(seen)
		}
	}

	s.renderLang(w, r, "inventory.html", map[string]any{
		"Devices":      all,
		"Stats":        stats,
		"OfflineCount": stats.Devices - stats.Online,
		"ClientCounts": clientCounts,
		"Optical":      opticalByDevice,
		"HasOptical":   hasOptical,
		"FTTR":         fttrCounts,
		"Section":      "inventory",
		"CanRefresh":   true,
	})
}
