package web

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/zy84338719/lightcat/internal/store"
)

// failingCtrl 让三个「入队型」动作一律失败，用来验证面板是否如实反馈。
type failingCtrl struct {
	stubCtrl
	err error
}

func (c *failingCtrl) RequestRefresh(int64) error { return c.err }
func (c *failingCtrl) FetchWiFi(int64) error      { return c.err }
func (c *failingCtrl) FetchSubtree(int64, string, []string, int) error {
	return c.err
}

// newFailingServer 起一个所有入队动作都失败的面板，返回 mux 与一台存在的设备 ID。
func newFailingServer(t *testing.T, cause error) (*http.ServeMux, int64) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "enqueue.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	id, _, err := st.UpsertDevice(&store.Device{
		OUI: "001122", ProductClass: "SimRouter", SerialNumber: "ENQ-1",
	})
	if err != nil {
		t.Fatalf("建设备失败: %v", err)
	}

	mux := http.NewServeMux()
	if _, err := Register(mux, st, &failingCtrl{err: cause}, Options{}); err != nil {
		t.Fatalf("挂路由失败: %v", err)
	}
	return mux, id
}

// assertEnqueueFailureNotice 断言重定向里带了 err=1 与原因文案。
func assertEnqueueFailureNotice(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusSeeOther {
		t.Fatalf("状态码 = %d，期望 303", w.Code)
	}
	loc := w.Header().Get("Location")
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("Location 解析失败: %v", err)
	}
	q := u.Query()
	if q.Get("err") != "1" {
		t.Errorf("Location %q 缺少 err=1 —— 入队失败被当成了成功反馈给用户", loc)
	}
	if msg := q.Get("msg"); !strings.Contains(msg, "没能入队") {
		t.Errorf("提示语 = %q，没有说明是入队失败（原因会被藏起来）", msg)
	}
}

// TestEnqueueFailure_FeedsBackToUser 是 OEK-4 的验收断言：
// 三个表单处理器以前用 `_ =` 吞掉入队错误照样跳「成功」，
// 现在必须带 err=1 反馈。
//
// 反向验证：把 err 置 nil 后断言会红（下面 ok 分支已覆盖）。
func TestEnqueueFailure_FeedsBackToUser(t *testing.T) {
	cause := errors.New("设备不在线")
	mux, id := newFailingServer(t, cause)
	ds := strconv.FormatInt(id, 10)

	cases := []struct {
		name string
		path string
		form string // 非空则作为 application/x-www-form-urlencoded 提交
	}{
		{name: "刷新基本信息", path: "/devices/" + ds + "/refresh"},
		{name: "重新采集无线概况", path: "/devices/" + ds + "/wifi"},
		// handleFetch 在 path 为空时按设计不入队，所以这里必须带上要读的子树路径
		{name: "读取参数子树", path: "/devices/" + ds + "/fetch", form: "path=InternetGatewayDevice."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var req *http.Request
			if c.form != "" {
				req = httptest.NewRequest("POST", c.path, strings.NewReader(c.form))
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			} else {
				req = httptest.NewRequest("POST", c.path, nil)
			}
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			assertEnqueueFailureNotice(t, w)
		})
	}
}

// TestEnqueueSuccess_NoErrorFlag 反向验证：入队成功时不该带 err=1。
// 没有这一条的话，上面的断言可能只是「永远带 err=1」也能通过。
func TestEnqueueSuccess_NoErrorFlag(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "enqueue_ok.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	id, _, err := st.UpsertDevice(&store.Device{
		OUI: "001122", ProductClass: "SimRouter", SerialNumber: "ENQ-OK",
	})
	if err != nil {
		t.Fatalf("建设备失败: %v", err)
	}

	mux := http.NewServeMux()
	if _, err := Register(mux, st, &stubCtrl{}, Options{}); err != nil {
		t.Fatalf("挂路由失败: %v", err)
	}

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("POST", "/devices/"+strconv.FormatInt(id, 10)+"/refresh", nil))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("状态码 = %d，期望 303", w.Code)
	}
	if loc := w.Header().Get("Location"); strings.Contains(loc, "err=1") {
		t.Errorf("入队成功却带了 err=1：%q", loc)
	}
}
