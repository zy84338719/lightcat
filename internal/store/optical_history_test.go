package store

import (
	"testing"
	"time"
)

func f(v float64) *float64 { return &v }

// 基本读写：rx 可以为空（Web 通道读不到收光），其余读数原样往返。
func TestOpticalHistoryRoundTrip(t *testing.T) {
	st := newTestStore(t)
	at := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)

	// TR-069 通道：五个读数都有
	if err := st.InsertOpticalSample(1, at, f(-21.5), f(2.8), f(43.0), f(3.226), f(29.0), "Up"); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	// Web 通道：收光为 nil
	if err := st.InsertOpticalSample(1, at.Add(time.Minute), nil, f(2.9), nil, nil, nil, "Up"); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	hist, err := st.OpticalHistory(1, 10)
	if err != nil {
		t.Fatalf("读历史失败: %v", err)
	}
	if len(hist) != 2 {
		t.Fatalf("应有 2 条，得到 %d", len(hist))
	}
	if !hist[0].RxDbm.Valid || hist[0].RxDbm.Float64 != -21.5 {
		t.Errorf("第一条 rx = %+v，期望 -21.5", hist[0].RxDbm)
	}
	if hist[1].RxDbm.Valid {
		t.Errorf("第二条 rx 应为空（宁缺毋编），得到 %+v", hist[1].RxDbm)
	}
	if !hist[1].TxDbm.Valid || hist[1].TxDbm.Float64 != 2.9 {
		t.Errorf("第二条 tx = %+v，期望 2.9", hist[1].TxDbm)
	}
	if !hist[0].At.Equal(at) {
		t.Errorf("时间没原样往返：%v vs %v", hist[0].At, at)
	}

	// 别的设备看不到这台的历史
	if other, _ := st.OpticalHistory(2, 10); len(other) != 0 {
		t.Errorf("设备 2 不该有历史，得到 %d 条", len(other))
	}
}

// RecordOpticalSample 的合并窗口：同一次采集的多批 GPV 回执并成一个点，
// 后到的读数只补空、不覆盖已有值。
func TestRecordOpticalSampleCoalesces(t *testing.T) {
	st := newTestStore(t)
	t0 := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)

	// 第一批：只认出收光
	if err := st.RecordOpticalSample(1, t0, f(-21.5), nil, nil, nil, nil, "", 5*time.Minute); err != nil {
		t.Fatalf("第一批失败: %v", err)
	}
	// 第二批（30 秒后）：认出发光和温度 —— 应并进同一条
	if err := st.RecordOpticalSample(1, t0.Add(30*time.Second), nil, f(2.8), f(43), nil, nil, "", 5*time.Minute); err != nil {
		t.Fatalf("第二批失败: %v", err)
	}
	hist, _ := st.OpticalHistory(1, 10)
	if len(hist) != 1 {
		t.Fatalf("窗口内应合并成 1 条，得到 %d", len(hist))
	}
	if hist[0].RxDbm.Float64 != -21.5 || hist[0].TxDbm.Float64 != 2.8 || hist[0].TempC.Float64 != 43 {
		t.Errorf("合并结果不对: %+v", hist[0])
	}
	// 第三批带了个新收光值：已有值不被覆盖（第一条为准）
	if err := st.RecordOpticalSample(1, t0.Add(time.Minute), f(-30), nil, nil, nil, nil, "", 5*time.Minute); err != nil {
		t.Fatalf("第三批失败: %v", err)
	}
	hist, _ = st.OpticalHistory(1, 10)
	if len(hist) != 1 || hist[0].RxDbm.Float64 != -21.5 {
		t.Errorf("已有读数不该被覆盖: %+v", hist)
	}

	// 窗口外：新的一轮采集，插新行
	if err := st.RecordOpticalSample(1, t0.Add(10*time.Minute), f(-21.6), nil, nil, nil, nil, "", 5*time.Minute); err != nil {
		t.Fatalf("窗口外写入失败: %v", err)
	}
	hist, _ = st.OpticalHistory(1, 10)
	if len(hist) != 2 {
		t.Fatalf("窗口外应插新行，得到 %d 条", len(hist))
	}

	// window<=0：总是插新行
	if err := st.RecordOpticalSample(2, t0, f(-20), nil, nil, nil, nil, "", 0); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordOpticalSample(2, t0.Add(time.Second), f(-20), nil, nil, nil, nil, "", 0); err != nil {
		t.Fatal(err)
	}
	if hist2, _ := st.OpticalHistory(2, 10); len(hist2) != 2 {
		t.Errorf("window=0 应不合并，得到 %d 条", len(hist2))
	}
}

// 保留期：设了天数就在写入时顺带裁剪；0 = 不限。
func TestOpticalHistoryRetention(t *testing.T) {
	st := newTestStore(t)
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)

	st.SetOpticalHistoryRetention(30)
	old := now.AddDate(0, 0, -40) // 40 天前，超出保留期
	if err := st.InsertOpticalSample(1, old, f(-21), nil, nil, nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	if hist, _ := st.OpticalHistory(1, 10); len(hist) != 1 {
		t.Fatalf("旧样本应先写进去，得到 %d 条", len(hist))
	}
	// 新样本写入触发裁剪
	if err := st.InsertOpticalSample(1, now, f(-22), nil, nil, nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	hist, _ := st.OpticalHistory(1, 10)
	if len(hist) != 1 || !hist[0].At.Equal(now) {
		t.Fatalf("40 天前的样本应被裁掉，得到 %+v", hist)
	}

	// 不限保留期：多老的都留着
	st.SetOpticalHistoryRetention(0)
	if err := st.InsertOpticalSample(2, old, f(-21), nil, nil, nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertOpticalSample(2, now, f(-22), nil, nil, nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	if hist2, _ := st.OpticalHistory(2, 10); len(hist2) != 2 {
		t.Errorf("保留期 0 应不裁剪，得到 %d 条", len(hist2))
	}
}

func TestLastOpticalSample(t *testing.T) {
	st := newTestStore(t)
	if _, ok, err := st.LastOpticalSample(1); err != nil || ok {
		t.Fatalf("空库应返回 ok=false，得到 ok=%v err=%v", ok, err)
	}
	t0 := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	_ = st.InsertOpticalSample(1, t0, f(-21), nil, nil, nil, nil, "")
	_ = st.InsertOpticalSample(1, t0.Add(time.Hour), f(-22), nil, nil, nil, nil, "")
	last, ok, err := st.LastOpticalSample(1)
	if err != nil || !ok {
		t.Fatalf("取最近样本失败: ok=%v err=%v", ok, err)
	}
	if last.RxDbm.Float64 != -22 {
		t.Errorf("最近样本 rx = %v，期望 -22", last.RxDbm.Float64)
	}
}
