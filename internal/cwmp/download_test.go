package cwmp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zy84338719/lightcat/internal/store"
)

func seedFile(t *testing.T, st *store.Store) store.File {
	t.Helper()
	f := &store.File{
		Name: "fw.bin", FileType: store.FileTypeFirmware,
		URLPath: "/files/0123456789abcdef0123456789abcdef",
		Size:    4096, SHA256: "aa", Version: "V9",
	}
	if _, err := st.CreateFile(f); err != nil {
		t.Fatal(err)
	}
	return *f
}

// Download 报文构造：TR-069 的要素一个不能少，FileType 用标准值。
func TestBuildTaskBodyDownload(t *testing.T) {
	payload, _ := json.Marshal(downloadPayload{
		FileID: 1, FileType: FileTypeOf(store.FileTypeFirmware),
		URL: "http://10.0.0.1:7547/files/abc", FileSize: 4096,
		DelaySeconds: 30, CommandKey: "ck-1",
	})
	body, err := buildTaskBody(&store.Task{Kind: TaskDownload, Payload: string(payload), CommandKey: "ck-1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"<cwmp:Download>",
		"<CommandKey>ck-1</CommandKey>",
		"<FileType>1 Firmware Upgrade Image</FileType>",
		"<URL>http://10.0.0.1:7547/files/abc</URL>",
		"<FileSize>4096</FileSize>",
		"<DelaySeconds>30</DelaySeconds>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("报文缺 %q：%s", want, body)
		}
	}

	// 类型映射
	if got := FileTypeOf(store.FileTypeWeb); got != "2 Web Content" {
		t.Errorf("web 类型映射不对: %q", got)
	}
	if got := FileTypeOf(store.FileTypeConfig); got != "3 Vendor Configuration File" {
		t.Errorf("config 类型映射不对: %q", got)
	}

	// 缺 URL 的坏载荷：构造要报错（任务会被跳过），不能发出空 URL 的 Download
	bad, _ := json.Marshal(downloadPayload{FileID: 1})
	if _, err := buildTaskBody(&store.Task{Kind: TaskDownload, Payload: string(bad)}); err == nil {
		t.Error("缺 URL 应报错")
	}
}

// 入队：URL 用「设备可达基地址 + 登记的 url_path」拼；重复下发要拒绝。
func TestEnqueueDownload(t *testing.T) {
	srv, st, devID := newTestServer(t, Config{})
	f := seedFile(t, st)

	// 没有 -acs-url（base 为空）：明确报错，不猜地址
	if _, err := srv.EnqueueDownload(devID, f.ID, 0, "  "); err == nil {
		t.Error("缺基地址应报错")
	}
	// 不存在的文件
	if _, err := srv.EnqueueDownload(devID, 999, 0, "http://10.0.0.1:7547"); err == nil {
		t.Error("文件不存在应报错")
	}

	id, err := srv.EnqueueDownload(devID, f.ID, 60, "http://10.0.0.1:7547/")
	if err != nil {
		t.Fatal(err)
	}
	tk, err := st.GetTask(id)
	if err != nil || tk.Kind != TaskDownload || tk.Status != store.TaskPending {
		t.Fatalf("任务不对: %+v err=%v", tk, err)
	}
	var p downloadPayload
	if err := json.Unmarshal([]byte(tk.Payload), &p); err != nil {
		t.Fatal(err)
	}
	if p.URL != "http://10.0.0.1:7547"+f.URLPath {
		t.Errorf("URL 拼接不对: %q", p.URL)
	}
	if p.DelaySeconds != 60 || p.CommandKey == "" || p.CommandKey != tk.CommandKey {
		t.Errorf("载荷不对: %+v", p)
	}

	// 同一设备已有一条未结的 Download：拒绝重复下发
	if _, err := srv.EnqueueDownload(devID, f.ID, 0, "http://10.0.0.1:7547"); err == nil {
		t.Error("重复下发应被拒绝")
	}
}

// TransferComplete 关联：成功结案 / 失败结案 / 未知 key 不动 / 重复回执不改已结任务。
func TestSettleDownloadTask(t *testing.T) {
	srv, st, devID := newTestServer(t, Config{})
	f := seedFile(t, st)

	// 成功路径
	id, err := srv.EnqueueDownload(devID, f.ID, 0, "http://10.0.0.1:7547")
	if err != nil {
		t.Fatal(err)
	}
	tk, _ := st.GetTask(id)
	// 模拟「已下发」：claim 使其 running
	claimed, _ := st.ClaimNextTask(devID, nil)
	if claimed == nil || claimed.ID != id {
		t.Fatalf("claim 到的不是下载任务: %+v", claimed)
	}
	srv.settleDownloadTask(tk.CommandKey, "0", "")
	got, _ := st.GetTask(id)
	if got.Status != store.TaskDone || !strings.Contains(got.Result, "传输完成") {
		t.Errorf("成功后应结案: %+v", got)
	}
	// 重复回执：已 done 的任务不再被改
	srv.settleDownloadTask(tk.CommandKey, "9010", "又来一次失败回执")
	got2, _ := st.GetTask(id)
	if got2.Status != store.TaskDone {
		t.Errorf("重复回执不该改已结任务: %+v", got2)
	}

	// 失败路径
	id2, err := srv.EnqueueDownload(devID, f.ID, 0, "http://10.0.0.1:7547")
	if err != nil {
		t.Fatalf("第一条已结案，应允许新的下发: %v", err)
	}
	tk2, _ := st.GetTask(id2)
	_, _ = st.ClaimNextTask(devID, nil)
	srv.settleDownloadTask(tk2.CommandKey, "9010", "Transfer failed: file not found")
	got3, _ := st.GetTask(id2)
	if got3.Status != store.TaskFailed || !strings.Contains(got3.Result, "9010") {
		t.Errorf("失败回执应判失败并留错误码: %+v", got3)
	}

	// 未知 CommandKey（设备自发/历史被裁）：只记日志，不炸
	srv.settleDownloadTask("没这个key", "", "")
	srv.settleDownloadTask("", "", "")
}
