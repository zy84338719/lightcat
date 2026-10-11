package ontweb

import "regexp"

// profile 是**一个光猫固件家族**的 Web 适配配方：登录握手、页面路径、
// 页面内嵌 JS 数据的构造器正则、AJAX 参数域，全部集中在这里。
//
// 设计口径与 param_aliases 一致：支持一台新机型是**加一份配方（数据）**，
// 采集流程（Snapshot → ApplySnapshot）与面板（读标准参数形状）都不用动。
//
// 现状：只有 huaweiV271 在真机（联通定制 V271-20，V5R023C10S200 固件）上验证过。
// 华为其它型号大概率同款页面结构（这套 asp 页面十几年没大变）；中兴 / 烽火 /
// 诺基亚等是另一套页面 —— 到那时照 huaweiV271Profile 抄一份改路径与正则即可。
//
// 配方选择：登录用默认配方，抓到设备信息后按自报的型号 / 固件版本重选
// （pickProfile）；对不上任何配方就继续用默认的，但解析失败时会把
// 「检测到的型号 / 固件 + 适配入口」写进错误与告警（不静默失败）。
type profile struct {
	Name string // 给人看的配方名（错误信息与日志里用）

	// ---- 登录握手 ----
	TokenPath string   // POST 拿一次性 token 的路径（"" = 该固件不需要 token）
	LoginPath string   // POST 登录的路径
	LoginOK   []string // 登录成功响应里出现的标记（任一命中即成功）

	// ---- 页面路径 ----
	DevicePage    string // 设备信息页
	OpticPage     string // 光口信息页
	WlanPage      string // 无线基本配置页
	HostsLanPage  string // 有线 / ONU 口终端页
	HostsWifiPage string // WiFi 终端页

	// ---- 终端 AJAX ----
	HostsParamDomain string // getajax.cgi 的 x= 参数域（{i} 保持字面量，固件不解码转义）
	HostsFields      string // POST body 里要的字段列表（& 分隔，尾部接 token）

	// ---- 页面解析（预编译正则）----
	deviceInfoRe *regexp.Regexp // new <ctor>(…) 的实参串
	cpuRe        *regexp.Regexp
	memRe        *regexp.Regexp
	opticRowRe   *regexp.Regexp // 光口寄存器原始值行
	opticStatRe  *regexp.Regexp // opticPower / opticStatus 变量
	wlanCallRe   *regexp.Regexp // 无线实例构造调用
	ssidNumRe    *regexp.Regexp // 2.4G/5G 实例数划分
	tokenRe      *regexp.Regexp // 页面里的 onttoken 输入框

	// ---- 适用性判定（抓到设备信息后选配方用）----
	modelRe *regexp.Regexp // nil = 不参与判定
	swRe    *regexp.Regexp // nil = 不参与判定
}

// huaweiV271Profile 是真机验证过的配方：华为 V271-20（V5R023 固件，联通定制）。
func huaweiV271Profile() *profile {
	return &profile{
		Name: "华为 V271-20（V5R023 固件）",

		TokenPath: "/asp/GetRandCount.asp",
		LoginPath: "/login.cgi",
		LoginOK:   []string{"top.location.replace", "index.asp"},

		DevicePage:    "/html/ssmp/deviceinfo/deviceinfo.asp",
		OpticPage:     "/html/amp/opticinfo/useropticinfo.asp",
		WlanPage:      "/html/amp/wlanbasic/wlan.asp?basic?2G",
		HostsLanPage:  "/html/bbsp/userdevinfo/userdevinfolan.asp",
		HostsWifiPage: "/html/bbsp/userdevinfo/userdevinfowifi.asp",

		HostsParamDomain: "InternetGatewayDevice.LANDevice.1.X_HW_UserDev.{i}",
		HostsFields:      "HostName&IpAddr&MacAddr&PortType&PortID&TrafficSend&TrafficRecv&DevStatus&time&IpType",

		// stDeviceInfo(domain, SN, HW, SW, 型号, 厂商代码, 出厂日期, MAC, 描述, …)
		deviceInfoRe: regexp.MustCompile(`new stDeviceInfo\(([^;]+?\))`),
		cpuRe:        regexp.MustCompile(`var cpuUsed = '([^']*)'`),
		memRe:        regexp.MustCompile(`var memUsed = '([^']*)'`),
		opticRowRe:   regexp.MustCompile(`new UserOpticInfo\("([^"]+)","(\d+)","(\d+)","(\d+)","(\d+)"\)`),
		opticStatRe:  regexp.MustCompile(`var (opticPower|opticStatus) = '([^']*)'`),
		wlanCallRe:   regexp.MustCompile(`new stWlan\(`),
		ssidNumRe:    regexp.MustCompile(`var SsidNum = '(\d+),(\d+)'`),
		tokenRe:      regexp.MustCompile(`name="onttoken"[^>]*value="([0-9a-fA-F]+)"`),

		modelRe: regexp.MustCompile(`(?i)V271|HN8145|F[0-9]{4}|EG\d{3}`), // 华为主力 FTTR/PON 型号族
		swRe:    regexp.MustCompile(`(?i)^V[35]R`),                       // 华为固件版本写法 V5R023…
	}
}

// defaultProfile 是登录与首轮设备信息抓取用的配方（也是离线解析模式的配方）。
var defaultProfile = huaweiV271Profile()

// profiles 是全部已登记配方。新增机型：照 huaweiV271Profile 加一份、登记到这里。
var profiles = []*profile{defaultProfile}

// matches 按设备自报的型号 / 固件版本判断配方是否适用。
// 两个判定正则都为 nil 的配方是「通用兜底」，永远匹配。
func (p *profile) matches(d DeviceInfo) bool {
	modelOK := p.modelRe == nil || p.modelRe.MatchString(d.Model) || p.modelRe.MatchString(d.Description)
	swOK := p.swRe == nil || p.swRe.MatchString(d.Software)
	if p.modelRe == nil && p.swRe == nil {
		return true
	}
	// 有判定条件的：型号与固件至少对上一样才算（都对不上肯定不是这家的页面）
	if p.modelRe != nil && p.swRe != nil {
		return modelOK || swOK
	}
	if p.modelRe != nil {
		return modelOK
	}
	return swOK
}

// pickProfile 按设备自报信息选配方；一个都对不上就返回默认配方
// （页面结构没准还是兼容的，解析失败时错误信息里会带上检测到的型号/固件）。
func pickProfile(d DeviceInfo) *profile {
	for _, p := range profiles {
		if p.matches(d) {
			return p
		}
	}
	return defaultProfile
}
