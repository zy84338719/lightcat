// `lightcat ontweb …`：华为光猫 Web 适配器（只读通道）。
//
// 为什么要有这个子命令：TR-069 通道需要运营商超级管理员才能把 ACS URL 指到本 ACS，
// 而普通账号（user）的 Web 界面自带 AJAX 参数接口。本命令用普通账号采集一次
// 设备信息 / 光口读数 / 在线终端，按标准 TR-098 参数形状写进与主程序同一个库 ——
// 面板（设备列表 / 详情 / 光功率 / 终端列表）原样复用，一行不改。
// 入库逻辑在 internal/ontweb.ApplySnapshot（服务端的内建轮询共用同一份）。
//
// 用法：
//
//	ONT_USER=user ONT_PASSWORD=… lightcat ontweb -url http://192.168.1.1 -db /var/lib/lightcat/lightcat.db
//	ONT_PASSWORD=… lightcat ontweb -from-dir var   # 解析外层脚本抓好的页面（见 README）
package main

import (
	"flag"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"

	"github.com/zy84338719/lightcat/internal/ontweb"
	"github.com/zy84338719/lightcat/internal/store"
)

func runOntWeb(args []string) error {
	fs := flag.NewFlagSet("ontweb", flag.ExitOnError)
	var (
		base     = fs.String("url", "http://192.168.1.1", "光猫 Web 地址")
		user     = fs.String("user", os.Getenv("ONT_USER"), "光猫账号（默认取 ONT_USER）")
		pass     = fs.String("pass", os.Getenv("ONT_PASSWORD"), "光猫密码（默认取 ONT_PASSWORD）")
		dbPath   = fs.String("db", "lightcat.db", "SQLite 文件（与主程序共用同一个库）")
		histDays = fs.Int("history-days", 30, "光功率历史保留天数（0 = 不限；与主程序 -optical-history-days 同一口径）")
		quiet    = fs.Bool("quiet", false, "只输出错误")
		fromDir  = fs.String("from-dir", "", "跳过登录与抓取，直接解析该目录下的页面文件（device.html/optic.html/wlan.html/hosts_lan.txt/hosts_wifi.txt）")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}

	var snap *ontweb.Snapshot
	var err error
	if *fromDir != "" {
		// 离线模式：页面由外层脚本（如系统 curl/python3）抓好落盘 —— 见 README「本地网络授权」
		snap, err = ontweb.LoadSnapshotFromDir(*fromDir)
	} else {
		if *user == "" || *pass == "" {
			return fmt.Errorf("需要光猫账号密码：设 ONT_USER / ONT_PASSWORD 或用 -user -pass")
		}
		var cl *ontweb.Client
		cl, err = ontweb.New(*base, *user, *pass)
		if err != nil {
			return err
		}
		snap, err = cl.Snapshot()
	}
	if err != nil {
		return err
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	st.SetOpticalHistoryRetention(*histDays)

	id, nParams, err := ontweb.ApplySnapshot(st, snap, *base)
	if err != nil {
		return err
	}
	if *quiet {
		return nil
	}
	// 软诊断如实打印（某页解析不出、配方对不上…）—— 不静默失败
	for _, w := range snap.Warnings {
		fmt.Fprintln(os.Stderr, "提示:", w)
	}
	// 与 ApplySnapshot 相同口径的去重计数（两侧查询返回的是同一批终端）
	uniq := map[string]bool{}
	for _, h := range append(append([]ontweb.Host{}, snap.LanHosts...), snap.WifiHosts...) {
		if strings.ToLower(h.MAC) != "" {
			uniq[strings.ToLower(h.MAC)] = true
		}
	}
	fmt.Printf("已采集 %s（%s / %s，配方 %s）：参数 %d 条，终端 %d 台（FTTR 子光猫 %d 台、其下终端 %d 台），发光 %s dBm（原始值入库，面板按映射表换算）\n",
		devShowName(snap), snap.Device.SN, snap.Device.Software, snap.Profile,
		nParams, len(uniq), countSubs(snap), countSubClients(snap), webTxDbm(snap.Optic.TXRaw))
	_ = id
	return nil
}

func devShowName(snap *ontweb.Snapshot) string {
	vendor := ontweb.VendorName(snap.Device.Vendor)
	if vendor != "" && snap.Device.Model != "" {
		return vendor + " " + snap.Device.Model
	}
	return firstNonEmptyStr(snap.Device.Model, snap.Device.SN)
}

func countSubs(snap *ontweb.Snapshot) int {
	seen := map[int]bool{}
	n := 0
	for _, h := range append(append([]ontweb.Host{}, snap.LanHosts...), snap.WifiHosts...) {
		if !strings.EqualFold(h.PortType, "ONU") {
			continue
		}
		var inst int
		fmt.Sscanf(strings.ToUpper(h.PortID), "ONU%d", &inst)
		if inst > 0 && !seen[inst] {
			seen[inst] = true
			n++
		}
	}
	return n
}

func countSubClients(snap *ontweb.Snapshot) int {
	seen := map[string]bool{}
	n := 0
	for _, h := range append(append([]ontweb.Host{}, snap.LanHosts...), snap.WifiHosts...) {
		if !strings.EqualFold(h.PortType, "ONU") || seen[strings.ToLower(h.MAC)] {
			continue
		}
		seen[strings.ToLower(h.MAC)] = true
		if ontweb.IsSelfEntry(h) {
			continue
		}
		n++
	}
	return n
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// webTxDbm 给命令行输出一个直观读数（面板侧仍以映射表换算为准）。
func webTxDbm(raw string) string {
	v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || v <= 0 {
		return "?"
	}
	return fmt.Sprintf("%.1f", 10*math.Log10(v/10000))
}
