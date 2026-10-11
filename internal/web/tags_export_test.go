package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zy84338719/lightcat/internal/store"
)

func newTagsTestMux(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "lightcat.db"))
	if err != nil {
		t.Fatalf("打开库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	mux := http.NewServeMux()
	if _, err := Register(mux, st, &stubCtrl{}, Options{}); err != nil {
		t.Fatalf("挂路由失败: %v", err)
	}
	return mux, st
}

func TestHandleDeviceTags(t *testing.T) {
	mux, st := newTagsTestMux(t)
	id, _, err := st.UpsertDevice(&store.Device{OUI: "001122", ProductClass: "R", SerialNumber: "TAG-WEB"})
	if err != nil {
		t.Fatal(err)
	}

	form := url.Values{"tags": {"机房A, 批次2，机房A 超长标签超长标签超长标签超长标签超长标签超长"}}
	req := httptest.NewRequest("POST", "/devices/"+strconv.FormatInt(id, 10)+"/tags", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("保存标签应 303 回详情页，得到 %d：%s", w.Code, w.Body.String())
	}

	tags, err := st.DeviceTags(id)
	if err != nil {
		t.Fatal(err)
	}
	// 去重后 3 个；超长的那个被截到 MaxTagLen
	if len(tags) != 3 {
		t.Fatalf("应存 3 个去重后的标签：%#v", tags)
	}
	for _, tg := range tags {
		if len([]rune(tg)) > store.MaxTagLen {
			t.Errorf("超长标签应被截断：%q", tg)
		}
	}

	// 不存在的设备 → 404
	req = httptest.NewRequest("POST", "/devices/9999/tags", strings.NewReader("tags=x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("不存在的设备应 404，得到 %d", w.Code)
	}
}

func TestIndexTagFilterAndWorstRx(t *testing.T) {
	mux, st := newTagsTestMux(t)
	d1, _, _ := st.UpsertDevice(&store.Device{OUI: "001122", ProductClass: "R", SerialNumber: "IDX-A", ModelName: "M1"})
	d2, _, _ := st.UpsertDevice(&store.Device{OUI: "001122", ProductClass: "R", SerialNumber: "IDX-B", ModelName: "M2"})
	if err := st.SetDeviceTags(d1, []string{"机房A"}); err != nil {
		t.Fatal(err)
	}

	// 首页：标签 chips 出现、行内出现标签徽章
	w := doGet(t, mux, "/")
	body := w.Body.String()
	if !strings.Contains(body, "机房A") {
		t.Errorf("首页应渲染标签（chips 或行内徽章）")
	}

	// ?tag= 筛选：只留带标签的那台
	w = doGet(t, mux, "/?tag="+url.QueryEscape("机房A"))
	body = w.Body.String()
	if !strings.Contains(body, "IDX-A") || strings.Contains(body, "IDX-B") {
		t.Errorf("按标签筛选应只剩 IDX-A")
	}
	// 不存在的标签 → 空态提示
	w = doGet(t, mux, "/?tag="+url.QueryEscape("没有这个标签"))
	if !strings.Contains(w.Body.String(), "当前标签下没有设备") {
		t.Errorf("空标签筛选应显示空态提示")
	}

	// 收光最弱榜：两台各插一条收光历史，低的排前面
	f := func(v float64) *float64 { return &v }
	now := time.Now()
	if err := st.InsertOpticalSample(d1, now, f(-14), nil, nil, nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertOpticalSample(d2, now, f(-26), nil, nil, nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	w = doGet(t, mux, "/")
	body = w.Body.String()
	if !strings.Contains(body, "收光最弱") {
		t.Fatalf("有收光数据后应出现「收光最弱」区块")
	}
	iWorst := strings.Index(body, "收光最弱")
	// 榜单里 -26.00（IDX-B）应排在 -14.00（IDX-A）前面：在榜单区块内先出现 -26.00
	seg := body[iWorst:]
	if strings.Index(seg, "-26.00") < 0 || strings.Index(seg, "-14.00") < 0 {
		t.Errorf("榜单应含两条读数：%s", seg[:min(400, len(seg))])
	}
	if strings.Index(seg, "-26.00") > strings.Index(seg, "-14.00") {
		t.Errorf("最弱的应排在最前")
	}
}

func TestApiDevicesCSV(t *testing.T) {
	mux, st := newTagsTestMux(t)
	id, _, _ := st.UpsertDevice(&store.Device{OUI: "001122", ProductClass: "R",
		SerialNumber: "CSV-1", Manufacturer: "Example", ModelName: "M1", Online: true})
	_, _, _ = st.UpsertDevice(&store.Device{OUI: "001122", ProductClass: "R", SerialNumber: "CSV-2"})
	if err := st.SetDeviceTags(id, []string{"机房A"}); err != nil {
		t.Fatal(err)
	}
	f := func(v float64) *float64 { return &v }
	if err := st.InsertOpticalSample(id, time.Now(), f(-15.95), f(0.5), f(43), nil, nil, ""); err != nil {
		t.Fatal(err)
	}

	w := doGet(t, mux, "/api/devices.csv")
	if w.Code != http.StatusOK {
		t.Fatalf("导出应 200，得到 %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Errorf("Content-Type 应为 text/csv：%q", ct)
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Errorf("应带附件下载头：%q", cd)
	}
	body := w.Body.String()
	if !strings.HasPrefix(body, "\ufeff") {
		t.Errorf("应以 UTF-8 BOM 开头（Excel 兼容）")
	}
	if !strings.Contains(body, "serial_number") || !strings.Contains(body, "rx_dbm") {
		t.Errorf("表头缺列：%s", firstLine(body))
	}
	if !strings.Contains(body, "CSV-1") || !strings.Contains(body, "机房A") {
		t.Errorf("行数据缺失（序列号/标签）")
	}
	if !strings.Contains(body, "-15.95") {
		t.Errorf("应含最近收光读数")
	}

	// ?tag= 过滤
	w = doGet(t, mux, "/api/devices.csv?tag="+url.QueryEscape("机房A"))
	body = w.Body.String()
	if !strings.Contains(body, "CSV-1") || strings.Contains(body, "CSV-2") {
		t.Errorf("CSV 按标签过滤失效")
	}
}

func TestApiDeviceOpticalCSV(t *testing.T) {
	mux, st := newTagsTestMux(t)
	id, _, _ := st.UpsertDevice(&store.Device{OUI: "001122", ProductClass: "R", SerialNumber: "OPT-CSV"})
	f := func(v float64) *float64 { return &v }
	base := time.Now().Add(-time.Hour)
	for i := 0; i < 3; i++ {
		if err := st.InsertOpticalSample(id, base.Add(time.Duration(i)*time.Minute),
			f(-15+float64(i)), nil, nil, nil, nil, ""); err != nil {
			t.Fatal(err)
		}
	}

	w := doGet(t, mux, "/api/devices/"+strconv.FormatInt(id, 10)+"/optical.csv")
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，得到 %d", w.Code)
	}
	lines := strings.Split(strings.TrimSpace(strings.TrimPrefix(w.Body.String(), "\ufeff")), "\n")
	if len(lines) != 4 { // 表头 + 3 行
		t.Fatalf("应 4 行，得到 %d：%v", len(lines), lines)
	}
	if lines[0] != "ts,rx_dbm,tx_dbm,temp_c,volt_v,bias_ma,status" {
		t.Errorf("表头不对：%q", lines[0])
	}

	// limit 生效
	w = doGet(t, mux, "/api/devices/"+strconv.FormatInt(id, 10)+"/optical.csv?limit=2")
	lines = strings.Split(strings.TrimSpace(strings.TrimPrefix(w.Body.String(), "\ufeff")), "\n")
	if len(lines) != 3 {
		t.Errorf("limit=2 应得 3 行，得到 %d", len(lines))
	}

	// 不存在的设备 → 404
	w = doGet(t, mux, "/api/devices/9999/optical.csv")
	if w.Code != http.StatusNotFound {
		t.Errorf("应 404，得到 %d", w.Code)
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
