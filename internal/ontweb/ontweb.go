// Package ontweb 提供光猫 Web 界面（普通账号）的采集客户端。
//
// 为什么存在这个包：TR-069 需要超级管理员才能把 ACS URL 指过来，而普通账号的
// Web 界面其实自带一套 AJAX 参数接口（getajax.cgi，本质是 HTTP 上的 TR-181 查询）。
// 这条通道拿不到写权限，但设备信息、光口读数、在线终端都能读 —— 够面板展示。
//
// 机型适配：页面路径、登录握手、页面内嵌 JS 数据的构造器都收在 profile（配方）里，
// 见 profile.go。当前唯一真机验证过的配方是华为 V271-20（V5R023 固件）：
//
//	登录握手：POST /asp/GetRandCount.asp（响应带 UTF-8 BOM）拿一次性 token，
//	          POST /login.cgi：UserName + base64(密码) + Language + x.X_HW_Token。
//
// 本包不做任何文件读写（LoadSnapshotFromDir 除外），也不依赖 store ——
// 纯客户端 + 解析，方便测试。
package ontweb

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/oekit/pkg/errors"
	"github.com/oekit/pkg/jsonrepair"
)

// Client 是一台光猫的 Web 会话。
type Client struct {
	base string
	user string
	pass string
	http *http.Client

	// prof 是当前使用的机型配方。登录与首轮设备信息用默认配方，
	// 抓到设备信息后按自报型号/固件重选（见 Snapshot）。
	prof *profile
}

// New 建一个客户端。base 形如 http://192.168.1.1。
func New(base, user, pass string) (*Client, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	if !strings.Contains(base, "://") {
		base = "http://" + base
	}
	u, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil {
		return nil, errors.Wrap(err, "光猫地址不合法")
	}
	if u.Host == "" {
		return nil, fmt.Errorf("光猫地址不合法: %s", base)
	}
	return &Client{
		base: strings.TrimRight(base, "/"),
		user: user,
		pass: pass,
		http: &http.Client{Timeout: 20 * time.Second, Jar: jar},
		prof: defaultProfile,
	}, nil
}

// ProfileName 返回当前使用的机型配方名（日志与诊断用）。
func (c *Client) ProfileName() string { return c.prof.Name }

// Host 是终端列表里的一台设备（有线 / WiFi / FTTR 子光猫口）。
type Host struct {
	HostName  string
	IP        string
	MAC       string
	PortType  string // ETH / ONU / WLAN…
	PortID    string // LAN1 / ONU1 / SSID…
	Status    string // Online / Offline
	IpType    string
	Time      string // 接入时长（设备自己格式化的）
	SendBytes int64
	RecvBytes int64
}

// DeviceInfo 是设备基本信息。
type DeviceInfo struct {
	SN          string
	Hardware    string
	Software    string
	Model       string
	Vendor      string
	MAC         string
	Description string
	CPUUsage    string
	MemUsage    string
}

// WlanRow 是一个 WLANConfiguration 实例的无线概况（面板的 Wi-Fi 概况表读这组字段）。
type WlanRow struct {
	Instance   int
	Band       string // 2.4GHz / 5GHz（按固件自报的 SsidNum='n,m' 划分实例段）
	Enable     string // "1" / "0"
	SSID       string
	BeaconType string // WPA2 / WPA2/WPA3 / 11i …
	XHWWpaEnc  string // X_HW_WPAand11iEncryptionModes，如 AESEncryption
}

// Optic 是光口的一组原始读数（寄存器原始值，换算交给面板的映射表）。
// 固件自己的公式：功率 10·log10(v/10000) dBm、电压 v/10 mV、温度 v/256 ℃、偏流 v/500 mA。
type Optic struct {
	Iface      string // 参数域，如 …X_HW_LANOpticInterfaceConfig.1.
	Status     string // Up / Down
	PowerOn    string // ON / OFF
	TXRaw      string // 发光功率原始值
	VoltageRaw string // 供电电压原始值（0.1mV）
	TempRaw    string // 温度原始值（1/256 ℃）
	BiasRaw    string // 偏流原始值（2μA）
}

// empty 表示一个字段都没解析出来（页面抓到了但配方对不上时的诊断依据）。
func (o Optic) empty() bool {
	return o.Iface == "" && o.Status == "" && o.PowerOn == "" &&
		o.TXRaw == "" && o.VoltageRaw == "" && o.TempRaw == "" && o.BiasRaw == ""
}

// Snapshot 是一次完整采集。
type Snapshot struct {
	Device    DeviceInfo
	Optic     Optic
	LanHosts  []Host
	WifiHosts []Host
	WlanRows  []WlanRow
	At        time.Time

	// Warnings 是「采到了但不完整」的软诊断（某页解析不出、配方对不上…）。
	// 入库照常进行，调用方（CLI / 内建轮询）负责把它展示出来 —— 不静默失败。
	Warnings []string
	// Profile 记录本次采集实际使用的配方名（入库日志里留痕）。
	Profile string
}

var (
	quotedRe    = regexp.MustCompile(`"((?:[^"\\]|\\x[0-9a-fA-F]{2})*)"`)
	hexEscRe    = regexp.MustCompile(`\\x([0-9a-fA-F]{2})`)
	quotedAllRe = regexp.MustCompile(`"([^"]*)"`)
)

// hexDecode 把固件 AJAX 响应里的 \xNN 逐字节还原成 UTF-8 文本。
func hexDecode(s string) string {
	if !strings.Contains(s, `\x`) {
		return s
	}
	b := hexEscRe.ReplaceAllFunc([]byte(s), func(m []byte) []byte {
		v, err := strconv.ParseUint(string(m[2:4]), 16, 8)
		if err != nil {
			return m
		}
		return []byte{byte(v)}
	})
	return string(b)
}

func (c *Client) doGet(path string) (string, error) {
	res, err := c.http.Get(c.base + path)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func (c *Client) doPost(path, contentType, body string) (string, error) {
	res, err := c.http.Post(c.base+path, contentType, bytes.NewReader([]byte(body)))
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// Login 走普通账号的登录握手（按当前配方）。之后会话 Cookie 由 jar 维护。
func (c *Client) Login() error {
	token := ""
	if c.prof.TokenPath != "" {
		// GetRandCount 的响应带 UTF-8 BOM，必须剥掉，否则 token 对不上。
		raw, err := c.doPost(c.prof.TokenPath, "application/x-www-form-urlencoded", "")
		if err != nil {
			return errors.Wrap(err, "拿登录 token 失败")
		}
		token = strings.TrimSpace(strings.TrimPrefix(raw, "\ufeff"))
		if token == "" {
			return fmt.Errorf("登录 token 为空：设备不在线，或该固件的登录握手与内置配方不同（当前配方：%s，适配入口见 internal/ontweb/profile.go）", c.prof.Name)
		}
	}
	form := url.Values{}
	form.Set("UserName", c.user)
	form.Set("PassWord", base64.StdEncoding.EncodeToString([]byte(c.pass)))
	form.Set("Language", "chinese")
	if token != "" {
		form.Set("x.X_HW_Token", token)
	}
	res, err := c.doPost(c.prof.LoginPath, "application/x-www-form-urlencoded", form.Encode())
	if err != nil {
		return errors.Wrap(err, "登录请求失败")
	}
	// 成功标记按配方判（华为 V5R023：top.location.replace 跳转页；失败回登录页 + 错误文案）
	for _, ok := range c.prof.LoginOK {
		if strings.Contains(res, ok) {
			return nil
		}
	}
	return fmt.Errorf("登录被拒绝（用户名或密码错误、已被锁定，或该固件的成功标记与内置配方不同）")
}

// Page 抓一个相对路径页面（诊断用）。
func (c *Client) Page(path string) (string, error) { return c.doGet(path) }

// parseDevice 从设备信息页 HTML 里抠出基本信息（配方方法，纯函数便于测试）。
func (p *profile) parseDevice(page string) DeviceInfo {
	var out DeviceInfo
	if m := p.deviceInfoRe.FindStringSubmatch(page); m != nil {
		args := quotedRe.FindAllStringSubmatch(m[1], -1)
		vals := make([]string, 0, len(args))
		for _, a := range args {
			vals = append(vals, hexDecode(a[1]))
		}
		// stDeviceInfo(domain, SN, HW, SW, 型号, 厂商代码, 出厂日期, MAC, 描述, …)
		pick := func(i int) string {
			if i < len(vals) {
				return vals[i]
			}
			return ""
		}
		out.SN = pick(1)
		out.Hardware = pick(2)
		out.Software = pick(3)
		out.Model = pick(4)
		out.Vendor = pick(5)
		out.MAC = pick(7)
		out.Description = pick(8)
	}
	if m := p.cpuRe.FindStringSubmatch(page); m != nil {
		out.CPUUsage = m[1]
	}
	if m := p.memRe.FindStringSubmatch(page); m != nil {
		out.MemUsage = m[1]
	}
	return out
}

// parseDevicePage 用默认配方解析（测试与离线模式的入口，行为与重构前一致）。
func parseDevicePage(page string) DeviceInfo { return defaultProfile.parseDevice(page) }

// DeviceInfo 采集设备基本信息。
func (c *Client) DeviceInfo() (DeviceInfo, error) {
	page, err := c.doGet(c.prof.DevicePage)
	if err != nil {
		return DeviceInfo{}, errors.Wrap(err, "抓设备信息页失败")
	}
	d := c.prof.parseDevice(page)
	if d.SN == "" && strings.TrimSpace(page) != "" {
		return d, fmt.Errorf("设备信息页解析不出序列号：页面结构与内置配方不符（配方：%s，页面：%s）—— "+
			"换固件或换机型时按 internal/ontweb/profile.go 加一份配方即可", c.prof.Name, c.prof.DevicePage)
	}
	return d, nil
}

// parseOptic 从光口信息页 HTML 里抠出原始读数（配方方法，纯函数便于测试）。
func (p *profile) parseOptic(page string) Optic {
	var out Optic
	for _, m := range p.opticStatRe.FindAllStringSubmatch(page, -1) {
		switch m[1] {
		case "opticPower":
			out.PowerOn = m[2]
		case "opticStatus":
			out.Status = m[2]
		}
	}
	if m := p.opticRowRe.FindStringSubmatch(page); m != nil {
		out.Iface, out.TXRaw, out.VoltageRaw, out.TempRaw, out.BiasRaw =
			m[1], m[2], m[3], m[4], m[5]
	}
	return out
}

// parseOpticPage 用默认配方解析（测试与离线模式的入口）。
func parseOpticPage(page string) Optic { return defaultProfile.parseOptic(page) }

// Optic 采集光口读数（寄存器原始值）。
func (c *Client) Optic() (Optic, error) {
	page, err := c.doGet(c.prof.OpticPage)
	if err != nil {
		return Optic{}, errors.Wrap(err, "抓光口信息页失败")
	}
	o := c.prof.parseOptic(page)
	if o.empty() && strings.TrimSpace(page) != "" {
		return o, fmt.Errorf("光口信息页认不出任何读数：页面结构与内置配方不符（配方：%s，页面：%s）",
			c.prof.Name, c.prof.OpticPage)
	}
	return o, nil
}

// stWlanArgs 抠出页面上每个 stWlan(…) 调用的实参（字符串感知的括号配平）。
// 实参全是带引号的字符串字面量（值里的非 ASCII 是 \xNN 转义），先配平再解码再切分。
func (p *profile) stWlanArgs(page string) [][]string {
	var rows [][]string
	for _, loc := range p.wlanCallRe.FindAllStringIndex(page, -1) {
		i, j, depth, inStr := loc[1], loc[1], 1, false
		for j < len(page) && depth > 0 {
			ch := page[j]
			if inStr {
				if ch == '\\' {
					j += 2
					continue
				}
				if ch == '"' {
					inStr = false
				}
			} else {
				switch {
				case ch == '"':
					inStr = true
				case ch == '(':
					depth++
				case ch == ')':
					depth--
				}
			}
			j++
		}
		seg := hexDecode(page[i : j-1])
		args := quotedAllRe.FindAllStringSubmatch(seg, -1)
		row := make([]string, 0, len(args))
		for _, a := range args {
			row = append(row, a[1])
		}
		if len(row) >= 8 { // 短于 stWlan 的 28 参的是 stWpsPin 之类的其它构造
			rows = append(rows, row)
		}
	}
	return rows
}

// parseWlan 从无线基本配置页解析 WLANConfiguration 概况（配方方法，纯函数便于测试）。
//
// 2G/5G 两页内容相同（都含全部实例），取一页即可；实例号 → 频段的划分依据是
// 固件自报的 SsidNum='n,m'（2.4GHz n 个在前，5GHz m 个在后），不靠 SSID 名字猜。
func (p *profile) parseWlan(page string) ([]WlanRow, error) {
	var n2g int
	if m := p.ssidNumRe.FindStringSubmatch(page); m != nil {
		n2g, _ = strconv.Atoi(m[1])
	}
	out := make([]WlanRow, 0, 8)
	seen := map[string]bool{}
	for _, r := range p.stWlanArgs(page) {
		domain := r[0]
		inst := 0
		if idx := strings.LastIndex(domain, "."); idx >= 0 {
			inst, _ = strconv.Atoi(domain[idx+1:])
		}
		if inst == 0 || seen[domain] {
			continue
		}
		seen[domain] = true
		row := WlanRow{Instance: inst, Enable: r[2], SSID: r[3], BeaconType: r[7]}
		if n2g > 0 {
			row.Band = "5GHz"
			if row.Instance <= n2g {
				row.Band = "2.4GHz"
			}
		}
		if len(r) > 16 {
			row.XHWWpaEnc = r[16]
		}
		out = append(out, row)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Instance < out[b].Instance })
	return out, nil
}

// parseWlanPage 用默认配方解析（测试与离线模式的入口）。
func parseWlanPage(page string) ([]WlanRow, error) { return defaultProfile.parseWlan(page) }

// WlanConfig 采集无线概况。
func (c *Client) WlanConfig() ([]WlanRow, error) {
	page, err := c.doGet(c.prof.WlanPage)
	if err != nil {
		return nil, errors.Wrap(err, "抓无线配置页失败")
	}
	rows, err := c.prof.parseWlan(page)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 && strings.TrimSpace(page) != "" {
		return nil, fmt.Errorf("无线配置页认不出任何实例：页面结构与内置配方不符（配方：%s，页面：%s）",
			c.prof.Name, c.prof.WlanPage)
	}
	return rows, nil
}

// Hosts 采集在线终端。which 取 "lan"（有线/ONU 口）或 "wifi"。
// 固件按 RequestFile 区分两侧，参数域见配方 HostsParamDomain。
func (c *Client) Hosts(which string) ([]Host, error) {
	var pagePath string
	switch which {
	case "lan":
		pagePath = c.prof.HostsLanPage
	case "wifi":
		pagePath = c.prof.HostsWifiPage
	default:
		return nil, fmt.Errorf("Hosts: 未知侧 %q", which)
	}
	page, err := c.doGet(pagePath)
	if err != nil {
		return nil, errors.Wrap(err, "抓终端页失败")
	}
	m := c.prof.tokenRe.FindStringSubmatch(page)
	if m == nil {
		return nil, fmt.Errorf("终端页里没有 onttoken：页面结构与内置配方不符（配方：%s，页面：%s）",
			c.prof.Name, pagePath)
	}
	// 查询串必须原样拼：固件的 httpd 不解码 %7B 这类转义，{i} 得保持字面量。
	requestFile := strings.TrimPrefix(pagePath, "/")
	q := "x=" + c.prof.HostsParamDomain + "&RequestFile=" + requestFile
	body := c.prof.HostsFields + "&x.X_HW_Token=" + m[1]
	raw, err := c.doPost("/getajax.cgi?"+q, "application/x-www-form-urlencoded", body)
	if err != nil {
		return nil, errors.Wrap(err, "终端查询失败")
	}
	return parseHostsJSON(raw), nil
}

// parseHostsJSON 解析终端 AJAX 响应（\xNN 转义的 JSON 数组，纯函数，便于测试）。
//
// 用 jsonrepair.Unmarshal 而不是 json.Unmarshal：这是**真机抓取**，厂商固件
// 返回什么我们控制不了。它能救回来的典型坏法是「包了一层」——markdown 代码围栏、
// 前后带说明文字、数组尾部多一个逗号；以前整份直接丢弃（界面上就是「一个终端都没有」），
// 现在拿到的终端照常显示。
//
// 边界要说清楚（实测过的，别当万能钥匙用）：
//   - 合法的 JSON 走 stdlib 快路径，解析成本不变；
//   - 截断的数组（只回来半截）**救不回来**，仍然返回 nil —— 宁可少显示，
//     也不能把没解析全的东西当成完整结果报上去；
//   - 纯垃圾（登录过期提示、HTML 错误页）照样报错，不会凭空造出空终端。
func parseHostsJSON(raw string) []Host {
	var rows []map[string]any
	if err := jsonrepair.Unmarshal(hexDecode(raw), &rows); err != nil {
		return nil
	}
	out := make([]Host, 0, len(rows))
	for _, r := range rows {
		mac := asString(r["MacAddr"])
		if mac == "" {
			continue // 固件会在末尾附一条汇总行，没有 MAC
		}
		out = append(out, Host{
			HostName:  asString(r["HostName"]),
			IP:        asString(r["IpAddr"]),
			MAC:       mac,
			PortType:  asString(r["PortType"]),
			PortID:    asString(r["PortID"]),
			Status:    asString(r["DevStatus"]),
			IpType:    asString(r["IpType"]),
			Time:      asString(r["time"]),
			SendBytes: asInt(r["TrafficSend"]),
			RecvBytes: asInt(r["TrafficRecv"]),
		})
	}
	return out
}

// Snapshot 登录并做一次完整采集。
//
// 配方选择：登录与设备信息页用默认配方；抓到设备信息后按自报的型号/固件
// 重选配方（pickProfile），后面的页面按新配方抓。软失败（某页解析不出）
// 进 Warnings 而不是让整次采集报废 —— 采到多少入库多少，缺的说清楚。
func (c *Client) Snapshot() (*Snapshot, error) {
	if err := c.Login(); err != nil {
		return nil, err
	}
	snap := &Snapshot{At: time.Now()}
	warn := func(err error) {
		if err != nil {
			snap.Warnings = append(snap.Warnings, err.Error())
		}
	}
	d, err := c.DeviceInfo()
	if err != nil {
		warn(err)
	}
	snap.Device = d
	if d.SN != "" {
		// 按设备自报信息重选配方（对不上就继续用默认的，解析失败自会进 Warnings）
		if p := pickProfile(d); p != c.prof {
			c.prof = p
			snap.Warnings = append(snap.Warnings,
				fmt.Sprintf("按型号/固件切换到配方：%s", p.Name))
		}
	}
	if o, err := c.Optic(); err != nil {
		warn(err)
	} else {
		snap.Optic = o
	}
	if h, err := c.Hosts("lan"); err != nil {
		warn(err)
	} else {
		snap.LanHosts = h
	}
	if h, err := c.Hosts("wifi"); err != nil {
		warn(err)
	} else {
		snap.WifiHosts = h
	}
	if w, err := c.WlanConfig(); err != nil {
		warn(err)
	} else {
		snap.WlanRows = w
	}
	snap.Profile = c.prof.Name
	if snap.Device.SN == "" {
		return nil, fmt.Errorf("没采到设备信息（序列号为空）：%s；检测到的型号=%q 固件=%q —— "+
			"若是新机型/新固件，按 internal/ontweb/profile.go 加一份配方（页面路径与构造器正则），"+
			"采集与入库流程不用动",
			strings.Join(snap.Warnings, "; "), snap.Device.Model, snap.Device.Software)
	}
	return snap, nil
}

// 页面文件名：LoadSnapshotFromDir 的约定输入（目录里放这几份抓取结果）。
const (
	fileDevice    = "device.html"
	fileOptic     = "optic.html"
	fileWlan      = "wlan.html"
	fileHostsLan  = "hosts_lan.txt"
	fileHostsWifi = "hosts_wifi.txt"
)

// LoadSnapshotFromDir 从一个目录里读取已抓取的页面/AJAX 响应，组装成 Snapshot。
//
// 为什么需要这个入口：macOS 新版对 launchd 后台进程按二进制做「本地网络」授权，
// 未签名的自编译二进制会被静默拒绝（EHOSTUNREACH），而系统自带的 curl/python 放行。
// 所以定时采集可以由外层脚本用 curl 抓页面落盘，这里只负责解析与入库 ——
// 网络层与逻辑层分离后，采集在任何调度环境都能跑。
//
// 目录里应包含：device.html、optic.html、wlan.html、hosts_lan.txt、hosts_wifi.txt。
// hosts_*.txt 是 getajax 的原始响应（含 \xNN 转义，未解码）。
//
// 离线模式没法登录探测，一律用默认配方解析；解析不出的部分进 Warnings。
func LoadSnapshotFromDir(dir string) (*Snapshot, error) {
	if strings.TrimSpace(dir) == "" || strings.Contains(dir, "..") {
		return nil, fmt.Errorf("非法目录: %q", dir)
	}
	read := func(name string) (string, error) {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	p := defaultProfile
	snap := &Snapshot{At: time.Now(), Profile: p.Name}
	var missing []string
	if page, err := read(fileDevice); err != nil {
		missing = append(missing, err.Error())
	} else {
		snap.Device = p.parseDevice(page)
		if snap.Device.SN == "" {
			snap.Warnings = append(snap.Warnings, "device.html 解析不出序列号（页面结构与默认配方不符）")
		}
	}
	if page, err := read(fileOptic); err != nil {
		missing = append(missing, err.Error())
	} else if o := p.parseOptic(page); o.empty() {
		snap.Warnings = append(snap.Warnings, "optic.html 认不出任何光口读数（页面结构与默认配方不符）")
	} else {
		snap.Optic = o
	}
	if page, err := read(fileWlan); err != nil {
		missing = append(missing, err.Error())
	} else if rows, _ := p.parseWlan(page); len(rows) == 0 {
		snap.Warnings = append(snap.Warnings, "wlan.html 认不出任何无线实例（页面结构与默认配方不符）")
	} else {
		snap.WlanRows = rows
	}
	if raw, err := read(fileHostsLan); err != nil {
		missing = append(missing, err.Error())
	} else {
		snap.LanHosts = parseHostsJSON(raw)
	}
	if raw, err := read(fileHostsWifi); err != nil {
		missing = append(missing, err.Error())
	} else {
		snap.WifiHosts = parseHostsJSON(raw)
	}
	if snap.Device.SN == "" {
		return nil, fmt.Errorf("device.html 缺失或解析不出序列号（%s）—— 若是新机型/新固件，"+
			"按 internal/ontweb/profile.go 加一份配方", strings.Join(missing, "; "))
	}
	return snap, nil
}

func asString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return ""
	}
}

func asInt(v any) int64 {
	switch t := v.(type) {
	case string:
		n, _ := strconv.ParseInt(strings.TrimSpace(t), 10, 64)
		return n
	case float64:
		return int64(t)
	default:
		return 0
	}
}
