package web

import (
	"fmt"
	"strings"

	"github.com/zy84338719/lightcat/internal/store"
)

// 趋势图约定画布：viewBox 600x120，四周留白 12。
// 服务端直接算 polyline 的 points，前端零 JS、零依赖。
const (
	chartW, chartH, chartPad = 600.0, 120.0, 12.0
)

// sparkPoints 把光功率历史铺成 SVG polyline 的 points 属性。
//
// pick 返回 (值, 是否有效)：历史里允许有空值（比如某次没读到温度），
// 空值跳过但**保留它的时间位置**（x 用原始下标），这样断点不会把曲线拉变形。
// 有效点少于 2 个返回空串 —— 一个点画不出趋势，不摆空壳。
func sparkPoints(samples []store.OpticalSample, pick func(store.OpticalSample) (float64, bool)) string {
	var vals []float64
	var idx []int
	for i, s := range samples {
		if v, ok := pick(s); ok {
			vals = append(vals, v)
			idx = append(idx, i)
		}
	}
	if len(vals) < 2 {
		return ""
	}
	lo, hi := vals[0], vals[0]
	for _, v := range vals[1:] {
		if v < lo {
			lo = v
		}
		if v > hi {
			hi = v
		}
	}
	if hi-lo < 1e-9 {
		hi = lo + 1 // 一条直线也给它上下留白，别贴着边
	}
	span := float64(len(samples) - 1)
	if span < 1 {
		span = 1
	}
	var sb strings.Builder
	for j, v := range vals {
		x := chartPad + float64(idx[j])*(chartW-2*chartPad)/span
		y := chartH - chartPad - (v-lo)/(hi-lo)*(chartH-2*chartPad)
		fmt.Fprintf(&sb, "%.1f,%.1f ", x, y)
	}
	return strings.TrimSpace(sb.String())
}

// opticalPickers 是趋势图要画的几条线。加一条线就在这里加一个 picker。
// rx 只有 TR-069 通道的设备有（Web 适配通道的普通账号读不到上行收光）——
// 没有数据时 picker 返回 false，对应的图整块不渲染，不摆空壳。
func opticalPickers() map[string]func(store.OpticalSample) (float64, bool) {
	return map[string]func(store.OpticalSample) (float64, bool){
		"rx": func(s store.OpticalSample) (float64, bool) {
			if !s.RxDbm.Valid {
				return 0, false
			}
			return s.RxDbm.Float64, true
		},
		"tx": func(s store.OpticalSample) (float64, bool) {
			if !s.TxDbm.Valid {
				return 0, false
			}
			return s.TxDbm.Float64, true
		},
		"temp": func(s store.OpticalSample) (float64, bool) {
			if !s.TempC.Valid {
				return 0, false
			}
			return s.TempC.Float64, true
		},
	}
}
