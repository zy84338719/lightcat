package store

import (
	"testing"
)

func TestFileCRUD(t *testing.T) {
	st := newTestStore(t)

	f := &File{
		Name: "V271_firmware.bin", FileType: FileTypeFirmware,
		URLPath: "/files/0123456789abcdef0123456789abcdef",
		Size:    1234, SHA256: "abc", Version: "V5R023", Model: "V271-20",
	}
	id, err := st.CreateFile(f)
	if err != nil || id == 0 {
		t.Fatalf("登记失败: id=%d err=%v", id, err)
	}
	if f.CreatedAt.IsZero() {
		t.Error("应默认填时间")
	}

	// 用户可控的 URLPath 形状必须被拒绝（路径穿越挡在入库前）
	for _, bad := range []string{"../etc/passwd", "/etc/passwd", "/files/../x", ""} {
		if _, err := st.CreateFile(&File{Name: "x", URLPath: bad}); err == nil {
			t.Errorf("url_path=%q 应被拒绝", bad)
		}
	}
	if _, err := st.CreateFile(&File{Name: "", URLPath: "/files/aa"}); err == nil {
		t.Error("空名应被拒绝")
	}

	list, err := st.ListFiles()
	if err != nil || len(list) != 1 {
		t.Fatalf("列出失败: %d err=%v", len(list), err)
	}
	if list[0].Version != "V5R023" || list[0].Size != 1234 {
		t.Errorf("字段没往返: %+v", list[0])
	}

	got, err := st.GetFile(id)
	if err != nil || got.URLPath != f.URLPath {
		t.Fatalf("GetFile 不对: %+v err=%v", got, err)
	}
	byPath, err := st.GetFileByURLPath(f.URLPath)
	if err != nil || byPath.ID != id {
		t.Fatalf("GetFileByURLPath 不对: %+v err=%v", byPath, err)
	}
	if _, err := st.GetFileByURLPath("/files/不存在"); err == nil {
		t.Error("没登记的路径应查不到")
	}

	if err := st.DeleteFile(id); err != nil {
		t.Fatal(err)
	}
	if list, _ := st.ListFiles(); len(list) != 0 {
		t.Error("删除后应列不出")
	}
}

func TestFindTaskByCommandKey(t *testing.T) {
	st := newTestStore(t)
	devID, _, _ := st.UpsertDevice(&Device{OUI: "001122", ProductClass: "R", SerialNumber: "DL1"})
	id, err := st.EnqueueTask(&Task{DeviceID: devID, Kind: "Download", CommandKey: "ck-123"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.FindTaskByCommandKey("ck-123")
	if err != nil || got == nil || got.ID != id {
		t.Fatalf("按 CommandKey 找任务失败: %+v err=%v", got, err)
	}
	// 空 key / 未知 key：nil 而不是错
	if got, err := st.FindTaskByCommandKey(""); err != nil || got != nil {
		t.Errorf("空 key 应返回 nil: %+v %v", got, err)
	}
	if got, err := st.FindTaskByCommandKey("没这个"); err != nil || got != nil {
		t.Errorf("未知 key 应返回 nil: %+v %v", got, err)
	}
}
