package web

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/zy84338719/lightcat/internal/store"
)

func newFirmwareEnv(t *testing.T, acsURL string) (*store.Store, *http.ServeMux, *stubCtrl, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "lightcat.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	filesDir := filepath.Join(dir, "files")
	ctrl := &stubCtrl{}
	mux := http.NewServeMux()
	opt := Options{Runtime: RuntimeSettings{FilesDir: filesDir, ACSURL: acsURL}}
	if _, err := Register(mux, st, ctrl, opt); err != nil {
		t.Fatal(err)
	}
	// 取件端点与 Register 挂的是同一个 handler，这里直接复用 mux 测
	return st, mux, ctrl, filesDir
}

// uploadReq 造一个multipart 上传请求。
func uploadReq(t *testing.T, fileName, content string, fields map[string]string) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", fileName)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/firmware/upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

// 上传：随机磁盘名（不是用户给的文件名）、sha256/大小入库、原始名只做展示。
func TestFirmwareUpload(t *testing.T) {
	st, mux, _, filesDir := newFirmwareEnv(t, "")

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, uploadReq(t, "../../evil.bin", "FIRMWARE-BYTES", map[string]string{
		"file_type": "firmware", "version": "V9", "model": "V271-20",
	}))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("上传应 303，得到 %d: %s", w.Code, w.Body.String())
	}
	if loc := w.Header().Get("Location"); strings.Contains(loc, "err=1") {
		t.Fatalf("上传不该报错: %q", loc)
	}

	files, _ := st.ListFiles()
	if len(files) != 1 {
		t.Fatalf("应有 1 份文件，得到 %d", len(files))
	}
	f := files[0]
	// 展示名也过了 filepath.Base：用户文件名里的路径成分哪儿都进不去
	if f.Name != "evil.bin" {
		t.Errorf("展示名应只留 basename: %q", f.Name)
	}
	if f.Size != int64(len("FIRMWARE-BYTES")) || f.Version != "V9" || f.Model != "V271-20" {
		t.Errorf("元数据不对: %+v", f)
	}
	if f.SHA256 == "" || len(f.SHA256) != 64 {
		t.Errorf("sha256 不对: %q", f.SHA256)
	}
	// 磁盘名必须是服务端生成的随机十六进制，绝不含用户输入
	disk := strings.TrimPrefix(f.URLPath, "/files/")
	if len(disk) != 32 || strings.Contains(f.URLPath, "evil") {
		t.Errorf("磁盘名不对: %q", f.URLPath)
	}
	// 目录里只有这一个随机名文件
	entries, _ := os.ReadDir(filesDir)
	if len(entries) != 1 || entries[0].Name() != disk {
		t.Errorf("磁盘上应是随机名文件: %v", entries)
	}
	// 内容原样落盘
	got, err := os.ReadFile(filepath.Join(filesDir, disk))
	if err != nil || string(got) != "FIRMWARE-BYTES" {
		t.Errorf("落盘内容不对: %q err=%v", got, err)
	}
}

// 取件端点：登记过的能取、没登记的 404、穿越形状 404。
func TestFirmwareFileServing(t *testing.T) {
	st, mux, _, _ := newFirmwareEnv(t, "")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, uploadReq(t, "fw.bin", "PAYLOAD-123", nil))
	f, _ := st.ListFiles()
	name := strings.TrimPrefix(f[0].URLPath, "/files/")

	// 登记过：200 + 原样内容 + 明确 Content-Length（CPE 的 HTTP 栈不吃 chunked）
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/files/"+name, nil))
	if w.Code != http.StatusOK || w.Body.String() != "PAYLOAD-123" {
		t.Fatalf("取件不对: %d %q", w.Code, w.Body.String())
	}
	if w.Header().Get("Content-Length") != "11" {
		t.Errorf("应带 Content-Length: %q", w.Header().Get("Content-Length"))
	}

	// 形状对但没登记：404
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/files/ffffffffffffffffffffffffffffffff", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("未登记的名字应 404，得到 %d", w.Code)
	}
	// 穿越/怪形状：404（../ 这种会先被 ServeMux 的路径清洗拦成 307，同样到不了文件）
	for _, bad := range []string{"../lightcat.db", "..%2flightcat.db", "abc", name + "x"} {
		w = httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", "/files/"+bad, nil))
		if w.Code != http.StatusNotFound && w.Code != http.StatusTemporaryRedirect {
			t.Errorf("%q 应 404/307，得到 %d", bad, w.Code)
		}
		if strings.Contains(w.Body.String(), "SQLite") {
			t.Errorf("%q 不该吐出任何文件内容", bad)
		}
	}

	// 删除后：记录没了、磁盘文件也没了、再取 404
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("POST", "/firmware/1/delete", nil))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("删除应 303，得到 %d", w.Code)
	}
	if list, _ := st.ListFiles(); len(list) != 0 {
		t.Error("记录应删掉")
	}
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/files/"+name, nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("删除后应 404，得到 %d", w.Code)
	}
}

// 下发：设备/文件/延迟传给控制接口；基地址优先 -acs-url，没配才退回面板 Host。
func TestFirmwareDeploy(t *testing.T) {
	st, mux, ctrl, _ := newFirmwareEnv(t, "http://10.0.0.1:7547")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, uploadReq(t, "fw.bin", "X", nil))
	f, _ := st.ListFiles()
	devID, _, _ := st.UpsertDevice(&store.Device{OUI: "001122", ProductClass: "R", SerialNumber: "DL-DEV"})

	post := func(form string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/firmware/deploy", strings.NewReader(form))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		mux.ServeHTTP(w, req)
		return w
	}

	w = post("device_id=999&file_id=x")
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "err=1") {
		t.Errorf("坏 ID 应报错: %q", loc)
	}
	w = post("device_id=" + strconv.FormatInt(devID, 10) +
		"&file_id=" + strconv.FormatInt(f[0].ID, 10) + "&delay=120")
	if w.Code != http.StatusSeeOther {
		t.Fatalf("下发应 303，得到 %d", w.Code)
	}
	if loc := w.Header().Get("Location"); strings.Contains(loc, "err=1") {
		t.Fatalf("下发不该报错: %q", loc)
	}
	if ctrl.dlDevice != devID || ctrl.dlFile != f[0].ID || ctrl.dlDelay != 120 {
		t.Errorf("传给控制接口的参数不对: %+v", ctrl)
	}
	if ctrl.dlBase != "http://10.0.0.1:7547" {
		t.Errorf("应优先用 -acs-url: %q", ctrl.dlBase)
	}

	// 没配 -acs-url：退回面板 Host；延迟超上限要拦
	st2, mux2, ctrl2, _ := newFirmwareEnv(t, "")
	w2 := httptest.NewRecorder()
	mux2.ServeHTTP(w2, uploadReq(t, "fw.bin", "X", nil))
	files2, _ := st2.ListFiles()
	dev2, _, _ := st2.UpsertDevice(&store.Device{OUI: "001122", ProductClass: "R", SerialNumber: "DL-DEV2"})
	deploy := func(delay string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/firmware/deploy",
			strings.NewReader("device_id="+strconv.FormatInt(dev2, 10)+
				"&file_id="+strconv.FormatInt(files2[0].ID, 10)+"&delay="+delay))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Host = "192.168.1.50:8080"
		w := httptest.NewRecorder()
		mux2.ServeHTTP(w, req)
		return w
	}
	if loc := deploy("999999").Header().Get("Location"); !strings.Contains(loc, "err=1") {
		t.Errorf("延迟超上限应报错: %q", loc)
	}
	deploy("0")
	if ctrl2.dlBase != "http://192.168.1.50:8080" {
		t.Errorf("没配 -acs-url 应退回面板 Host: %q", ctrl2.dlBase)
	}

	// 面板走 HTTPS 时，兜底基地址也要拼 https（设备拉文件不该再多跳一次）
	reqTLS := httptest.NewRequest("POST", "/firmware/deploy",
		strings.NewReader("device_id="+strconv.FormatInt(dev2, 10)+
			"&file_id="+strconv.FormatInt(files2[0].ID, 10)+"&delay=0"))
	reqTLS.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reqTLS.Host = "192.168.1.50:8443"
	reqTLS.TLS = &tls.ConnectionState{} // 模拟 TLS 请求
	wTLS := httptest.NewRecorder()
	mux2.ServeHTTP(wTLS, reqTLS)
	if ctrl2.dlBase != "https://192.168.1.50:8443" {
		t.Errorf("TLS 请求应拼 https 基地址: %q", ctrl2.dlBase)
	}
}

// JSON 接口：上传后脚本从 /api/files 拿文件 ID 再下发。
func TestFilesAPI(t *testing.T) {
	st, mux, _, _ := newFirmwareEnv(t, "")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, uploadReq(t, "fw.bin", "ABC", nil))
	f, _ := st.ListFiles()

	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/files", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("状态 = %d", w.Code)
	}
	var resp struct {
		Count int `json:"count"`
		Data  []struct {
			ID      int64  `json:"ID"`
			URLPath string `json:"URLPath"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析 JSON 失败: %v", err)
	}
	if resp.Count != 1 || len(resp.Data) != 1 || resp.Data[0].URLPath != f[0].URLPath {
		t.Errorf("API 内容不对: %+v", resp)
	}
}

// 固件页：文件列表、上传/下发表单、-acs-url 提示的渲染。
func TestFirmwarePage(t *testing.T) {
	st, mux, _, _ := newFirmwareEnv(t, "")
	if _, _, err := st.UpsertDevice(&store.Device{OUI: "001122", ProductClass: "R", SerialNumber: "P1"}); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/firmware", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("状态 = %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "文件库是空的") {
		t.Error("空库应显示空态")
	}
	if !strings.Contains(body, "-acs-url") {
		t.Error("没配 -acs-url 应给出提示")
	}

	w = httptest.NewRecorder()
	mux.ServeHTTP(w, uploadReq(t, "fw.bin", "X", map[string]string{"version": "V9"}))
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/firmware", nil))
	body = w.Body.String()
	for _, want := range []string{"fw.bin", "V9", "固件", "下发到设备"} {
		if !strings.Contains(body, want) {
			t.Errorf("固件页缺少 %q", want)
		}
	}
}
