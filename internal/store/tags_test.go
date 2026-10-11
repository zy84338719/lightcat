package store

import (
	"testing"
	"time"
)

func TestNormalizeTags(t *testing.T) {
	got := NormalizeTags(" 机房A, 机房A ，批次2;  楼层3　")
	// 全角空格（U+3000）会被 TrimSpace 当作空白剥掉（unicode.IsSpace 认它）；
	// 拆分符是中英文逗号/分号/半角空白
	want := []string{"机房A", "批次2", "楼层3"}
	if len(got) != len(want) {
		t.Fatalf("拆分结果 %#v，想要 %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 个：%q != %q", i, got[i], want[i])
		}
	}

	if n := len(NormalizeTags("")); n != 0 {
		t.Errorf("空输入应得空切片，得到 %d 个", n)
	}
	// 超长截断按 rune 数
	long := NormalizeTags("一二三四五六七八九十一二三四五六七八九十一二三四五六七八九十")
	if len(long) != 1 || len([]rune(long[0])) != MaxTagLen {
		t.Errorf("超长应截到 %d 个字符：%q", MaxTagLen, long)
	}
	// 限量
	many := ""
	for i := 0; i < MaxTagsPerDevice+5; i++ {
		many += "t" + string(rune('a'+i%26)) + string(rune('0'+i/26)) + ","
	}
	if n := len(NormalizeTags(many)); n != MaxTagsPerDevice {
		t.Errorf("应限量 %d 个，得到 %d", MaxTagsPerDevice, n)
	}
}

func TestSetDeviceTagsRoundTrip(t *testing.T) {
	st := newTestStore(t)
	id, _, err := st.UpsertDevice(&Device{OUI: "001122", ProductClass: "R", SerialNumber: "TAG1"})
	if err != nil {
		t.Fatal(err)
	}

	if err := st.SetDeviceTags(id, []string{"机房A", "批次2"}); err != nil {
		t.Fatal(err)
	}
	tags, err := st.DeviceTags(id)
	if err != nil {
		t.Fatal(err)
	}
	// DeviceTags 按标签名排序（展示稳定），UTF-8 字节序：批(E6 89) < 机(E6 9C)
	if len(tags) != 2 || tags[0] != "批次2" || tags[1] != "机房A" {
		t.Fatalf("读回 %#v", tags)
	}

	// 整组替换语义
	if err := st.SetDeviceTags(id, []string{"批次3"}); err != nil {
		t.Fatal(err)
	}
	tags, _ = st.DeviceTags(id)
	if len(tags) != 1 || tags[0] != "批次3" {
		t.Fatalf("替换后应为 [批次3]，得到 %#v", tags)
	}

	// 清空
	if err := st.SetDeviceTags(id, nil); err != nil {
		t.Fatal(err)
	}
	tags, _ = st.DeviceTags(id)
	if len(tags) != 0 {
		t.Fatalf("清空后应为空，得到 %#v", tags)
	}
}

func TestAllTagsAndDevicesWithTag(t *testing.T) {
	st := newTestStore(t)
	d1, _, _ := st.UpsertDevice(&Device{OUI: "001122", ProductClass: "R", SerialNumber: "T-A"})
	d2, _, _ := st.UpsertDevice(&Device{OUI: "001122", ProductClass: "R", SerialNumber: "T-B"})
	d3, _, _ := st.UpsertDevice(&Device{OUI: "001122", ProductClass: "R", SerialNumber: "T-C"})
	_ = st.SetDeviceTags(d1, []string{"机房A", "GPON"})
	_ = st.SetDeviceTags(d2, []string{"机房A"})
	_ = st.SetDeviceTags(d3, []string{"机房B"})

	all, err := st.AllTags()
	if err != nil {
		t.Fatal(err)
	}
	// 按使用数降序：机房A(2) 在最前
	if len(all) != 3 || all[0].Tag != "机房A" || all[0].Count != 2 {
		t.Fatalf("AllTags = %#v", all)
	}

	ids, err := st.DevicesWithTag("机房A")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || !ids[d1] || !ids[d2] {
		t.Fatalf("机房A 应命中 d1/d2：%v", ids)
	}

	// 列表接口整页带出标签
	byDev, err := st.TagsByDevice()
	if err != nil {
		t.Fatal(err)
	}
	if len(byDev[d1]) != 2 || len(byDev[d3]) != 1 {
		t.Fatalf("TagsByDevice = %#v", byDev)
	}
}

func TestTagsCascadeOnDelete(t *testing.T) {
	st := newTestStore(t)
	id, _, _ := st.UpsertDevice(&Device{OUI: "001122", ProductClass: "R", SerialNumber: "T-DEL"})
	_ = st.SetDeviceTags(id, []string{"要删的"})
	if err := st.DeleteDevice(id); err != nil {
		t.Fatal(err)
	}
	all, err := st.AllTags()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range all {
		if tc.Tag == "要删的" {
			t.Fatal("设备删除后标签应级联清掉")
		}
	}
}

func TestListAndGetDeviceFillTags(t *testing.T) {
	st := newTestStore(t)
	id, _, _ := st.UpsertDevice(&Device{OUI: "001122", ProductClass: "R", SerialNumber: "T-FILL"})
	_ = st.SetDeviceTags(id, []string{"x", "y"})

	list, err := st.ListDevices()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || len(list[0].Tags) != 2 {
		t.Fatalf("ListDevices 应带出标签：%+v", list)
	}
	d, err := st.GetDevice(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Tags) != 2 || d.Tags[0] != "x" {
		t.Fatalf("GetDevice 应带出标签：%+v", d.Tags)
	}
}

func TestWorstRx(t *testing.T) {
	st := newTestStore(t)
	d1, _, _ := st.UpsertDevice(&Device{OUI: "001122", ProductClass: "R", SerialNumber: "RX-A"})
	d2, _, _ := st.UpsertDevice(&Device{OUI: "001122", ProductClass: "R", SerialNumber: "RX-B"})
	d3, _, _ := st.UpsertDevice(&Device{OUI: "001122", ProductClass: "R", SerialNumber: "RX-NODATA"})

	f := func(v float64) *float64 { return &v }
	now := time.Now()
	// d1：先来一条差的，再来一条好的 —— 榜单应取**最近**那条（-12）
	_ = st.InsertOpticalSample(d1, now.Add(-time.Hour), f(-30), nil, nil, nil, nil, "")
	_ = st.InsertOpticalSample(d1, now, f(-12), nil, nil, nil, nil, "")
	// d2：一条 -25
	_ = st.InsertOpticalSample(d2, now, f(-25), nil, nil, nil, nil, "")
	// d3：只有发光没有收光（Web 通道的常态）—— 不该上榜
	_ = st.InsertOpticalSample(d3, now, nil, f(2.5), nil, nil, nil, "")

	worst, err := st.WorstRx(5)
	if err != nil {
		t.Fatal(err)
	}
	if len(worst) != 2 {
		t.Fatalf("应只有两台有收光数据：%+v", worst)
	}
	if worst[0].DeviceID != d2 || worst[0].RxDbm != -25 {
		t.Errorf("最弱应是 RX-B(-25)：%+v", worst[0])
	}
	if worst[1].DeviceID != d1 || worst[1].RxDbm != -12 {
		t.Errorf("次弱应是 RX-A 的最近读数(-12)：%+v", worst[1])
	}
	if worst[0].Serial != "RX-B" {
		t.Errorf("应联查出序列号：%+v", worst[0])
	}

	// limit 生效
	if w, _ := st.WorstRx(1); len(w) != 1 {
		t.Errorf("limit=1 应只回 1 台：%+v", w)
	}
}
