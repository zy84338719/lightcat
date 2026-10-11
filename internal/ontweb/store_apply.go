// 采集结果入库：把一次 Snapshot 按标准 TR-098 参数形状写进 store。
//
// 身份设计：OUI 取序列号前 6 位、型号做 ProductClass —— 将来这台设备真接上
// TR-069 时，(OUI, ProductClass, Serial) 三元组一致，会自动合并为同一台设备，
// 届时以真实 Inform 数据为准（非空覆盖 + source 区分来源）。
package ontweb

import (
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zy84338719/lightcat/internal/store"
)

// ApplySnapshot 把一次采集写进库：设备 upsert、设备信息 / 光口原始读数 /
// 无线概况 / FTTR 子光猫与在线终端、光口寄存器换算规则登记，
// 外加一条 6 WEBPOLL 采集记录。返回设备 ID 与本次写入的参数条数。
func ApplySnapshot(st *store.Store, snap *Snapshot, baseURL string) (int64, int, error) {
	if snap.Device.SN == "" {
		return 0, 0, fmt.Errorf("没采到序列号，拒绝入库（页面结构可能变了）")
	}

	// 设备身份：与 TR-069 Inform 同一套三元组，方便将来无缝合并。
	dev := &store.Device{
		OUI:             OUIOf(snap.Device.SN),
		ProductClass:    FirstNonEmpty(snap.Device.Model, "WEB"),
		SerialNumber:    snap.Device.SN,
		Manufacturer:    VendorName(snap.Device.Vendor),
		ModelName:       snap.Device.Model,
		DataModelRoot:   "InternetGatewayDevice.",
		SoftwareVersion: snap.Device.Software,
		HardwareVersion: snap.Device.Hardware,
		UserAgent:       "lightcat-ontweb/1.0",
		SourceIP:        HostOf(baseURL),
		LastEvents:      "6 WEBPOLL",
	}
	id, _, err := st.UpsertDevice(dev)
	if err != nil {
		return 0, 0, fmt.Errorf("写设备失败: %w", err)
	}

	root := dev.DataModelRoot
	params := []store.Param{
		{Name: root + "DeviceInfo.Manufacturer", Value: dev.Manufacturer, Source: "web", UpdatedAt: snap.At},
		{Name: root + "DeviceInfo.ModelName", Value: dev.ModelName, Source: "web", UpdatedAt: snap.At},
		{Name: root + "DeviceInfo.SerialNumber", Value: dev.SerialNumber, Source: "web", UpdatedAt: snap.At},
		{Name: root + "DeviceInfo.SoftwareVersion", Value: dev.SoftwareVersion, Source: "web", UpdatedAt: snap.At},
		{Name: root + "DeviceInfo.HardwareVersion", Value: dev.HardwareVersion, Source: "web", UpdatedAt: snap.At},
		{Name: root + "DeviceInfo.CPUUsage", Value: snap.Device.CPUUsage, Source: "web", UpdatedAt: snap.At},
		{Name: root + "DeviceInfo.MemoryUsage", Value: snap.Device.MemUsage, Source: "web", UpdatedAt: snap.At},
	}
	if o := snap.Optic; o.Iface != "" {
		add := func(leaf, raw string) {
			if raw == "" {
				return
			}
			params = append(params, store.Param{
				Name: o.Iface + leaf, Value: raw, Source: "web", UpdatedAt: snap.At,
			})
		}
		add("TransceiverPower", o.TXRaw)
		add("SupplyVoltage", o.VoltageRaw)
		add("TransceiverTemperature", o.TempRaw)
		add("BiasCurrent", o.BiasRaw)
		add("Status", o.Status)
	}

	// 两个侧的查询返回的是同一批终端（固件只按 RequestFile 区分页面，数据一样），
	// 按 MAC 去重后入库，避免 Hosts 表翻倍。
	seen := map[string]bool{}
	var hosts []Host
	for _, h := range append(append([]Host{}, snap.LanHosts...), snap.WifiHosts...) {
		key := strings.ToLower(h.MAC)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		hosts = append(hosts, h)
	}

	// FTTR 子光猫建模：PortID=ONU{n} 的终端都挂在 X_HW_APDevice.{n} 名下。
	// 端口上那个主机名条目是子光猫自己（isSelfEntry）—— 它做卡片，不做终端。
	// 有线（ETH LANx）与主猫自己 WiFi（WIFI SSIDx）不进这棵树：面板回答的是
	// 「谁连在哪个 WLAN 上」，这两类不是 WLAN 关联，硬造参数就是编数据。
	type subDev struct {
		mac, name, status string
		set               bool
		clients           []Host
	}
	subs := map[int]*subDev{}
	var plain []Host
	for _, h := range hosts {
		if strings.EqualFold(h.PortType, "ONU") && strings.HasPrefix(strings.ToUpper(h.PortID), "ONU") {
			n := 0
			fmt.Sscanf(strings.ToUpper(h.PortID), "ONU%d", &n)
			sd := subs[n]
			if sd == nil {
				sd = &subDev{}
				subs[n] = sd
			}
			if IsSelfEntry(h) && !sd.set {
				sd.mac, sd.name, sd.status, sd.set = h.MAC, h.HostName, h.Status, true
				continue
			}
			sd.clients = append(sd.clients, h)
			continue
		}
		plain = append(plain, h)
	}

	i := 0
	addHost := func(h Host) {
		i++
		p := fmt.Sprintf("%sLANDevice.1.Hosts.Host.%d.", root, i)
		addP := func(leaf, val string) {
			params = append(params, store.Param{Name: p + leaf, Value: val, Source: "web", UpdatedAt: snap.At})
		}
		addP("HostName", h.HostName)
		addP("MACAddress", h.MAC)
		addP("IPAddress", h.IP)
		addP("InterfaceType", strings.TrimSpace(h.PortType+" "+h.PortID))
		addP("Active", boolStr(h.Status == "Online"))
		addP("X_HW_TrafficSend", fmt.Sprint(h.SendBytes))
		addP("X_HW_TrafficRecv", fmt.Sprint(h.RecvBytes))
		addP("X_HW_ConnectTime", h.Time)
	}
	for _, h := range plain {
		addHost(h)
	}

	// 子光猫卡片 + 各自的关联终端表（面板 FTTR 区块与终端树读这组路径）。
	subNs := make([]int, 0, len(subs))
	for n := range subs {
		subNs = append(subNs, n)
	}
	sort.Ints(subNs)
	for _, n := range subNs {
		sd := subs[n]
		pre := fmt.Sprintf("%sX_HW_APDevice.%d.", root, n)
		addp := func(leaf, val string) {
			params = append(params, store.Param{Name: pre + leaf, Value: val, Source: "web", UpdatedAt: snap.At})
		}
		addp("DeviceType", FirstNonEmpty(sd.name, fmt.Sprintf("ONU%d", n)))
		addp("MACAddr", sd.mac)
		addp("DeviceStatus", sd.status)
		addp("OnlineFlag", boolStr(strings.EqualFold(sd.status, "Online")))
		addp("WorkingMode", "ONU")
		addp("AssociatedDeviceNumberOfEntries", fmt.Sprint(len(sd.clients)))
		for k, c := range sd.clients {
			cp := fmt.Sprintf("%sWLANConfiguration.1.AssociatedDevice.%d.", pre, k+1)
			params = append(params,
				store.Param{Name: cp + "MACAddress", Value: c.MAC, Source: "web", UpdatedAt: snap.At},
				store.Param{Name: cp + "X_HW_HostName", Value: c.HostName, Source: "web", UpdatedAt: snap.At},
				store.Param{Name: cp + "InterfaceType", Value: strings.TrimSpace(c.PortType + " " + c.PortID), Source: "web", UpdatedAt: snap.At},
			)
		}
	}

	// 无线概况：面板的 Wi-Fi 表格读这组叶子（SSID / 频段 / 加密 / 开关 / 关联终端数）。
	// 主猫 WiFi 上的终端（PortID=SSID{n}）挂到对应 WLANConfiguration.{n} 的关联表；
	// 有线与 ONU 口的终端不是 WLAN 关联，不进这棵树（FTTR 子光猫在上面单独建模）。
	wifiClients := map[int][]Host{}
	for _, h := range hosts {
		if strings.EqualFold(h.PortType, "WIFI") {
			var n int
			if k, _ := fmt.Sscanf(strings.ToUpper(h.PortID), "SSID%d", &n); k == 1 && n > 0 {
				wifiClients[n] = append(wifiClients[n], h)
			}
		}
	}
	for _, w := range snap.WlanRows {
		pre := fmt.Sprintf("%sLANDevice.1.WLANConfiguration.%d.", root, w.Instance)
		addw := func(leaf, val string) {
			if val == "" {
				return
			}
			params = append(params, store.Param{Name: pre + leaf, Value: val, Source: "web", UpdatedAt: snap.At})
		}
		addw("Enable", w.Enable)
		addw("SSID", w.SSID)
		addw("X_HW_RFBand", w.Band)
		addw("BeaconType", w.BeaconType)
		addw("X_HW_WPAAnd11iEncryptionModes", w.XHWWpaEnc)
		cl := wifiClients[w.Instance]
		addw("AssociatedDeviceNumberOfEntries", fmt.Sprint(len(cl)))
		for k, c := range cl {
			cp := fmt.Sprintf("%sAssociatedDevice.%d.", pre, k+1)
			params = append(params,
				store.Param{Name: cp + "MACAddress", Value: c.MAC, Source: "web", UpdatedAt: snap.At},
				store.Param{Name: cp + "X_HW_HostName", Value: c.HostName, Source: "web", UpdatedAt: snap.At},
			)
		}
	}

	if err := st.UpsertParams(id, params, "web"); err != nil {
		return 0, 0, fmt.Errorf("写参数失败: %w", err)
	}
	if err := ensureWebAliases(st); err != nil {
		return 0, 0, fmt.Errorf("登记光口映射失败: %w", err)
	}
	if err := st.InsertInform(&store.InformRecord{
		DeviceID:    id,
		Events:      "6 WEBPOLL",
		CommandKey:  "ontweb",
		CurrentTime: snap.At.UTC().Format(time.RFC1123Z),
		SourceIP:    HostOf(baseURL),
		ParamCount:  len(params),
	}); err != nil {
		return 0, 0, fmt.Errorf("写采集记录失败: %w", err)
	}

	// 光功率历史：面板趋势图的数据源，换算与面板映射表同一套公式。
	// 收光（rx）传 nil：普通账号的页面不暴露上行收光，宁缺毋编。
	// 保留期裁剪已下沉到 InsertOpticalSample（由 SetOpticalHistoryRetention 配置）。
	tx, temp, volt, bias := opticDecoded(snap.Optic)
	if err := st.InsertOpticalSample(id, snap.At, nil, tx, temp, volt, bias, snap.Optic.Status); err != nil {
		return 0, 0, err
	}
	return id, len(params), nil
}

// opticDecoded 把光口寄存器原始值换算成读数（与面板映射表同一套公式）：
// 功率 10·log10(v/10000) dBm、温度 v/256 ℃、电压 v/10000 V、偏流 v/500 mA。
// 读不到就返回 nil —— 历史里宁可空着也不编数。
func opticDecoded(o Optic) (tx, temp, volt, bias *float64) {
	if v, err := strconv.ParseFloat(strings.TrimSpace(o.TXRaw), 64); err == nil && v > 0 {
		d := 10 * math.Log10(v/10000)
		tx = &d
	}
	if v, err := strconv.ParseFloat(strings.TrimSpace(o.TempRaw), 64); err == nil {
		t := v / 256
		temp = &t
	}
	if v, err := strconv.ParseFloat(strings.TrimSpace(o.VoltageRaw), 64); err == nil {
		x := v / 10000
		volt = &x
	}
	if v, err := strconv.ParseFloat(strings.TrimSpace(o.BiasRaw), 64); err == nil {
		b := v / 500
		bias = &b
	}
	return
}

// ensureWebAliases 登记华为 FTTR 光口寄存器的换算规则（幂等：按唯一键 upsert）。
func ensureWebAliases(st *store.Store) error {
	rows := []store.ParamAlias{
		{Field: "tx_power", Vendor: "华为 FTTR（Web 适配）", MatchContains: "X_HW_LANOpticInterfaceConfig",
			MatchSuffix: ".transceiverpower", Decode: "dbm_01uw", Priority: 10, Enabled: true,
			Note: "寄存器原始值，固件公式 10·log10(v/10000)"},
		{Field: "voltage", Vendor: "华为 FTTR（Web 适配）", MatchContains: "X_HW_LANOpticInterfaceConfig",
			MatchSuffix: ".supplyvoltage", Decode: "mv01", Priority: 10, Enabled: true,
			Note: "原始值 0.1mV"},
		{Field: "temperature", Vendor: "华为 FTTR（Web 适配）", MatchContains: "X_HW_LANOpticInterfaceConfig",
			MatchSuffix: ".transceivertemperature", Decode: "div256", Priority: 10, Enabled: true,
			Note: "原始值 1/256 ℃"},
		{Field: "bias_current", Vendor: "华为 FTTR（Web 适配）", MatchContains: "X_HW_LANOpticInterfaceConfig",
			MatchSuffix: ".biascurrent", Decode: "ua2", Priority: 10, Enabled: true,
			Note: "原始值 2μA"},
	}
	for i := range rows {
		if err := st.UpsertParamAlias(&rows[i]); err != nil {
			return err
		}
	}
	return nil
}

// IsSelfEntry 判断这条终端记录是不是「子光猫自己」。
//
// 华为 FTTR（V271-20 + K251 系子光猫）实测：子光猫会在自己的 ONU 端口上
// 以主机名的形式出现（如 K251e）。按 K251 前缀认；没有这样的条目时
// 卡片少 MAC/型号但仍会渲染，不影响其它实例。
func IsSelfEntry(h Host) bool {
	return strings.HasPrefix(strings.ToUpper(strings.TrimSpace(h.HostName)), "K251")
}

// OUIOf 取序列号前 6 位做 OUI；太短就给一个明确的占位。
func OUIOf(sn string) string {
	sn = strings.TrimSpace(sn)
	if len(sn) >= 6 {
		return strings.ToUpper(sn[:6])
	}
	return "WEB000"
}

// VendorName 把厂商代码翻译成显示名（HWTC → Huawei）。
func VendorName(code string) string {
	switch strings.ToUpper(strings.TrimSpace(code)) {
	case "HWTC":
		return "Huawei"
	case "":
		return ""
	default:
		return code
	}
}

// FirstNonEmpty 返回第一个非空（去首尾空白）的值。
func FirstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// HostOf 从 base URL 里抠 host:port（设备列表的「来源 IP」列用）。
func HostOf(base string) string {
	if u, err := url.Parse(base); err == nil {
		return u.Host
	}
	return base
}

func boolStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}
