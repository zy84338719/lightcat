package web

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/zy84338719/lightcat/internal/store"
)

// 上传与下发（FR-11 固件/文件管理）：
//
//   - 上传：面板登录后才能用；文件落到 -files-dir，磁盘名用服务端生成的
//     随机十六进制（**绝不用用户给的文件名拼路径**），原始名只当元数据展示。
//   - 下发：给设备入队一条 Download 任务；CPE 自己来拉 /files/<随机名>。
//   - 取件端点 /files/{name} 不做面板鉴权 —— 设备没有面板登录态（TR-069 的
//     Download 本来就假设文件服务器可匿名或用专属凭据）。安全性靠三点：
//     名字是 128 位随机的、只有登记在库里的才放行、且只按精确名取（无目录列表）。

// maxFirmwareSize 是单份上传文件的上限（512 MiB，家用光猫固件通常 <100 MiB）。
const maxFirmwareSize = 512 << 20

// diskNameRe 是我们自己生成的磁盘文件名的形状（32 位小写十六进制）。
// 取件请求先过这道闸，任何别的形状直接 404 —— 路径穿越在这一层就死掉。
var diskNameRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

// randomDiskName 生成 16 字节随机十六进制的磁盘文件名。
func randomDiskName() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// handleFirmware 渲染「固件」页：文件库列表 + 上传表单 + 下发表单。
func (s *Server) handleFirmware(w http.ResponseWriter, r *http.Request) {
	files, err := s.store.ListFiles()
	if err != nil {
		http.Error(w, "读取文件库失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	devices, err := s.store.ListDevices()
	if err != nil {
		http.Error(w, "读取设备列表失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	rows := make([]map[string]any, 0, len(files))
	for _, f := range files {
		rows = append(rows, map[string]any{
			"ID":       f.ID,
			"Name":     f.Name,
			"Type":     firmwareTypeLabel(f.FileType),
			"Version":  f.Version,
			"Model":    f.Model,
			"Size":     humanSize(f.Size),
			"SHA256":   f.SHA256,
			"SHAShort": shortSHA(f.SHA256),
			"At":       f.CreatedAt,
		})
	}
	devRows := make([]map[string]any, 0, len(devices))
	for _, d := range devices {
		devRows = append(devRows, map[string]any{
			"ID":     d.ID,
			"Name":   d.DisplayName,
			"Online": d.Online,
		})
	}
	s.renderLang(w, r, "firmware.html", map[string]any{
		"Section":    "firmware",
		"Files":      rows,
		"Devices":    devRows,
		"Types":      store.FileTypes,
		"TypeLabels": map[string]string{"firmware": "固件", "web": "Web 内容", "config": "配置文件"},
		"HasACSURL":  strings.TrimSpace(s.opt.Runtime.ACSURL) != "",
		"Notice":     strings.TrimSpace(r.URL.Query().Get("msg")),
		"NoticeErr":  r.URL.Query().Get("err") == "1",
	})
}

func firmwareTypeLabel(t string) string {
	switch t {
	case store.FileTypeWeb:
		return "Web 内容"
	case store.FileTypeConfig:
		return "配置文件"
	default:
		return "固件"
	}
}

func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// humanSize 把字节数变成人能读的（面板不引第三方库，自己写十行）。
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// handleFirmwareUpload 处理上传：落盘（随机磁盘名）+ 登记元数据。
func (s *Server) handleFirmwareUpload(w http.ResponseWriter, r *http.Request) {
	dir := s.opt.Runtime.FilesDir
	if dir == "" {
		s.firmwareBack(w, r, "未配置文件目录（-files-dir）", true)
		return
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		s.firmwareBack(w, r, "创建文件目录失败: "+err.Error(), true)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFirmwareSize)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		s.firmwareBack(w, r, "读取上传失败（文件超过 512 MiB 或表单不合法）: "+err.Error(), true)
		return
	}
	src, header, err := r.FormFile("file")
	if err != nil {
		s.firmwareBack(w, r, "没有收到文件: "+err.Error(), true)
		return
	}
	defer src.Close()

	fileType := r.FormValue("file_type")
	if !containsStr(store.FileTypes, fileType) {
		fileType = store.FileTypeFirmware
	}
	diskName, err := randomDiskName()
	if err != nil {
		s.firmwareBack(w, r, "生成存储名失败: "+err.Error(), true)
		return
	}
	// 目标路径 = 配置目录 + 服务端生成的随机名。用户给的文件名只进元数据，
	// 永远不参与拼路径（filepath.Base 再兜一层底）。
	dstPath := filepath.Join(filepath.Clean(dir), diskName)
	dst, err := os.OpenFile(dstPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		s.firmwareBack(w, r, "写入文件失败: "+err.Error(), true)
		return
	}
	hasher := sha256.New()
	n, err := io.Copy(io.MultiWriter(dst, hasher), src)
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(dstPath)
		s.firmwareBack(w, r, "保存文件失败: "+err.Error(), true)
		return
	}

	f := &store.File{
		Name:     filepath.Base(header.Filename),
		FileType: fileType,
		URLPath:  "/files/" + diskName,
		Size:     n,
		SHA256:   hex.EncodeToString(hasher.Sum(nil)),
		Version:  strings.TrimSpace(r.FormValue("version")),
		Model:    strings.TrimSpace(r.FormValue("model")),
	}
	if _, err := s.store.CreateFile(f); err != nil {
		_ = os.Remove(dstPath) // 元数据都登不上，磁盘上也别留孤儿
		s.firmwareBack(w, r, "登记文件失败: "+err.Error(), true)
		return
	}
	s.firmwareBack(w, r, fmt.Sprintf("已上传 %s（%s，SHA256 %s）", f.Name, humanSize(f.Size), shortSHA(f.SHA256)), false)
}

// handleFirmwareDelete 删除一份文件（记录 + 磁盘）。
func (s *Server) handleFirmwareDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.firmwareBack(w, r, "文件 ID 非法", true)
		return
	}
	f, err := s.store.GetFile(id)
	if err != nil {
		s.firmwareBack(w, r, "文件不存在", true)
		return
	}
	if err := s.store.DeleteFile(id); err != nil {
		s.firmwareBack(w, r, "删除记录失败: "+err.Error(), true)
		return
	}
	// 磁盘文件按登记的 url_path 反推（同样是服务端生成的名字，无用户输入）
	if dir := s.opt.Runtime.FilesDir; dir != "" {
		name := strings.TrimPrefix(f.URLPath, "/files/")
		if diskNameRe.MatchString(name) {
			_ = os.Remove(filepath.Join(filepath.Clean(dir), name))
		}
	}
	s.firmwareBack(w, r, "已删除「"+f.Name+"」", false)
}

// handleFirmwareDeploy 给一台设备入队 Download 任务。
func (s *Server) handleFirmwareDeploy(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.firmwareBack(w, r, "表单解析失败", true)
		return
	}
	deviceID, err1 := strconv.ParseInt(r.FormValue("device_id"), 10, 64)
	fileID, err2 := strconv.ParseInt(r.FormValue("file_id"), 10, 64)
	delay, _ := strconv.Atoi(r.FormValue("delay"))
	if err1 != nil || err2 != nil {
		s.firmwareBack(w, r, "设备或文件 ID 非法", true)
		return
	}
	if delay < 0 || delay > 86400 {
		s.firmwareBack(w, r, "延迟秒数要在 0 ~ 86400 之间", true)
		return
	}
	// 文件 URL 的基地址：优先 -acs-url（设备视角可达的对外地址）；
	// 没配就退回「面板当前访问的 Host」—— 同网段部署时通常就是它，但要提醒核实。
	// 协议跟着面板请求走：开了 TLS 的面板拼 https，设备来拉文件不用再多跳一次。
	base := strings.TrimSpace(s.opt.Runtime.ACSURL)
	fallback := false
	if base == "" {
		scheme := "http://"
		if r.TLS != nil {
			scheme = "https://"
		}
		base = scheme + r.Host
		fallback = true
	}
	if s.ctrl == nil {
		s.firmwareBack(w, r, "当前部署没有下发能力", true)
		return
	}
	taskID, err := s.ctrl.EnqueueDownload(deviceID, fileID, delay, base)
	if err != nil {
		s.firmwareBack(w, r, "下发失败: "+err.Error(), true)
		return
	}
	msg := fmt.Sprintf("已入队下载任务 #%d；设备下次会话收到指令后自己来拉文件", taskID)
	if fallback {
		msg += "（注意：未配置 -acs-url，URL 按面板访问地址拼的 " + base + "，请确认设备可达）"
	}
	s.firmwareBack(w, r, msg, false)
}

func (s *Server) firmwareBack(w http.ResponseWriter, r *http.Request, msg string, isErr bool) {
	q := "?msg=" + url.QueryEscape(msg)
	if isErr {
		q += "&err=1"
	}
	http.Redirect(w, r, "/firmware"+q, http.StatusSeeOther)
}

// apiFiles 返回文件库 JSON（自动化/脚本用：上传后从这里拿文件 ID 再下发）。
func (s *Server) apiFiles(w http.ResponseWriter, r *http.Request) {
	payload, err := s.APIFiles()
	writeJSONOrError(w, payload, err)
}

// FirmwareFileHandler 是内置文件服务的取件端点（GET /files/<随机名>）。
//
// 独立导出是因为**双端口部署时它必须挂在 CWMP 那侧的 mux 上**（设备只连
// ACS 端口）；共用端口时 Register 也会把它挂进面板 mux（见 web.go）。
// 鉴权说明见文件头注释：不做面板登录校验，靠随机名 + 库内登记 + 精确匹配。
func FirmwareFileHandler(st *store.Store, dir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if !diskNameRe.MatchString(name) || dir == "" {
			http.NotFound(w, r)
			return
		}
		// 只放行登记在库里的文件（磁盘上的孤儿/残留不服务）
		if _, err := st.GetFileByURLPath("/files/" + name); err != nil {
			http.NotFound(w, r)
			return
		}
		f, err := os.Open(filepath.Join(filepath.Clean(dir), name))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer f.Close()
		w.Header().Set("Content-Type", "application/octet-stream")
		// CPE 的 HTTP 栈五花八门：明确 Content-Length，不用 chunked
		if fi, err := f.Stat(); err == nil {
			w.Header().Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
		}
		_, _ = io.Copy(w, f)
	}
}
