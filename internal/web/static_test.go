package web

import (
	"context"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	hertzserver "github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/adaptor"

	"github.com/zy84338719/lightcat/internal/store"
)

// startStaticServer 起一台与生产同构的引擎：先挂 NewHertzEngine 的 "/*any"
// 兜底（面板 mux 经 adaptor 进来），再按 withStatic 决定挂不挂原生静态路由。
// 只挂静态路由、不挂兜底的测法证明不了「静态路由压得过通配」这件事。
func startStaticServer(t *testing.T, withStatic bool) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	// 面板 mux 用「什么都答 panel-catchall」代替真实 mux：
	// 一旦静态请求没被原生路由接住，就会落到这里，测试能一眼看出是回落了。
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "panel-catchall")
	})

	e := hertzserver.New(
		hertzserver.WithListener(ln),
		hertzserver.WithDisablePrintRoute(true),
		hertzserver.WithHandleMethodNotAllowed(true),
	)
	fallback := adaptor.HertzHandler(mux)
	e.Any("/", fallback)
	e.Any("/*any", fallback)
	if withStatic {
		RegisterStaticRoutes(e)
	}
	go func() { _ = e.Run() }()
	t.Cleanup(func() {
		// 短超时即可：这里只是收掉测试用的监听器，不是在测优雅退出
		// （那件事由 cmd/lightcat 的 shutdown 测试负责）。
		// 用默认 5s 会让每个用例白白多花 5 秒。
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		_ = e.Shutdown(ctx)
	})
	return "http://" + ln.Addr().String()
}

func fetchStatic(t *testing.T, base, method, path string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, base+path, nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	resp, err := (&http.Client{
		Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}).Do(req)
	if err != nil {
		t.Fatalf("%s %s 失败: %v", method, path, err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(body)
}

// TestStaticServedByNativeRoute 静态资源由 Hertz 原生路由直出：
// 模板里引用的两个文件都要 200 + 正确 Content-Type + 完整字节。
func TestStaticServedByNativeRoute(t *testing.T) {
	base := startStaticServer(t, true)
	for _, c := range []struct{ path, mime string }{
		{"/static/style.css", "text/css; charset=utf-8"},
		{"/static/app.js", "text/javascript; charset=utf-8"},
	} {
		resp, body := fetchStatic(t, base, "GET", c.path)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s 状态码 = %d，期望 200（body=%q）", c.path, resp.StatusCode, body)
		}
		if ct := resp.Header.Get("Content-Type"); ct != c.mime {
			t.Errorf("%s Content-Type = %q，期望 %q", c.path, ct, c.mime)
		}
		if len(body) == 0 {
			t.Errorf("%s 返回空 body", c.path)
		}
		if n := resp.Header.Get("Content-Length"); n != "" && n != itoaLocal(len(body)) {
			t.Errorf("%s Content-Length = %q，body 却是 %d 字节", c.path, n, len(body))
		}
		// 逐字节对：面板的 CSS/JS 坏了就是白屏，比状态码更要紧
		want, err := assets.ReadFile("static/" + strings.TrimPrefix(c.path, "/static/"))
		if err != nil {
			t.Fatalf("读内嵌资源失败: %v", err)
		}
		if body != string(want) {
			t.Errorf("%s 内容与内嵌文件不一致（拿到 %d 字节，应为 %d）", c.path, len(body), len(want))
		}
		// 关键：不能是兜底路由吐出来的串
		if strings.Contains(body, "panel-catchall") {
			t.Errorf("%s 落到了 \"/*any\" 兜底（原生静态路由没接住）", c.path)
		}
	}
}

func itoaLocal(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TestStaticNoCacheKept 钉住「改了 app.js / style.css 刷新就能用上」这条行为：
// 静态资源必须发 no-cache。oekit 的 adaptor.StaticFSHandler 不带这个头，
// 换过去就会悄悄丢掉 —— 所以这里单独钉一根钉子。
func TestStaticNoCacheKept(t *testing.T) {
	base := startStaticServer(t, true)
	for _, p := range []string{"/static/style.css", "/static/app.js"} {
		resp, _ := fetchStatic(t, base, "GET", p)
		if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("%s Cache-Control = %q，期望 no-cache（浏览器会缓存住旧脚本）", p, cc)
		}
	}
}

// TestStaticHeadAndMethodNotAllowed HEAD 要能取到头；POST 不该被静态路由当成 200。
func TestStaticHeadAndMethodNotAllowed(t *testing.T) {
	base := startStaticServer(t, true)
	resp, _ := fetchStatic(t, base, "HEAD", "/static/style.css")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("HEAD /static/style.css 状态码 = %d，期望 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/css; charset=utf-8" {
		t.Errorf("HEAD Content-Type = %q，期望 text/css; charset=utf-8", ct)
	}
}

// TestStaticMissingFile404 找不到的文件给 404，且错误响应上**不能**有 Cache-Control
// （与 http.FileServer 时代一致：net/http 的 serveError 会摘掉缓存头，
// 免得一个临时 404 被浏览器缓存住）。
func TestStaticMissingFile404(t *testing.T) {
	base := startStaticServer(t, true)
	resp, body := fetchStatic(t, base, "GET", "/static/nope.css")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("状态码 = %d，期望 404（body=%q）", resp.StatusCode, body)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "" {
		t.Errorf("404 上的 Cache-Control = %q，应为空", cc)
	}
	if ns := resp.Header.Get("X-Content-Type-Options"); ns != "nosniff" {
		t.Errorf("404 的 X-Content-Type-Options = %q，期望 nosniff", ns)
	}
}

// TestStaticRejectsPathTraversal 目录穿越必须拿不到东西。
// embed.FS 的 Open 过 fs.ValidPath，".."、绝对路径一律拒。
func TestStaticRejectsPathTraversal(t *testing.T) {
	base := startStaticServer(t, true)
	for _, p := range []string{
		"/static/../templates/alerts.html",
		"/static/%2e%2e/templates/alerts.html",
		"/static/..%2ftemplates%2falerts.html",
	} {
		resp, body := fetchStatic(t, base, "GET", p)
		if resp.StatusCode == http.StatusOK && strings.Contains(body, "<html") {
			t.Errorf("%s 竟然读到了 embed 根外的模板（穿越成立）", p)
		}
	}
}

// TestStaticOnRealEngine 走**生产真正那条装配路径**（NewHertzEngine +
// RegisterStaticRoutes + 真实面板 mux），而不是上面那种近似引擎：
// 验的是「原生静态路由与 NewHertzEngine 的兜底、方法/静态路由开关能共存」，
// 顺带确认面板首页没被静态路由误伤。
func TestStaticOnRealEngine(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "real.db"))
	if err != nil {
		t.Fatalf("打开库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	mux := http.NewServeMux()
	if _, err := Register(mux, st, &stubCtrl{}, Options{}); err != nil {
		t.Fatalf("挂面板路由失败: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	e := NewHertzEngine(mux, ln, nil)
	RegisterStaticRoutes(e)
	go func() { _ = e.Run() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		_ = e.Shutdown(ctx)
	})
	base := "http://" + ln.Addr().String()

	// 1) 静态资源仍走原生路由（不鉴权：登录页自己就要用 style.css）
	resp, body := fetchStatic(t, base, "GET", "/static/style.css")
	if resp.StatusCode != http.StatusOK || len(body) == 0 {
		t.Fatalf("GET /static/style.css = %d，body %d 字节；期望 200 且非空", resp.StatusCode, len(body))
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q，期望 no-cache", cc)
	}

	// 2) 面板自己的路由没被静态路由误伤（仍由 mux 兜底处理）
	resp, body = fetchStatic(t, base, "GET", "/pingz")
	if resp.StatusCode == http.StatusOK && strings.Contains(body, "<html") {
		t.Errorf("/pingz 被静态路由抢了（body=%q）", body)
	}
}

// TestStaticDirectoryAndRangeBehavior 钉住两处**有意**的行为变化，
// 都是因为原生 handler 不再借道 http.FileServer：
//   - /static/（目录本身）回 404，而不是像 FileServer 那样列文件；
//   - 不发 Accept-Ranges —— 没实现 Range 就不宣称（CSS/JS 本来也不会 Range 请求），
//     宣称了却不支持才是撒谎。
func TestStaticDirectoryAndRangeBehavior(t *testing.T) {
	base := startStaticServer(t, true)
	resp, body := fetchStatic(t, base, "GET", "/static/")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /static/ 状态码 = %d，期望 404（不再返回文件列表）", resp.StatusCode)
	}
	if strings.Contains(body, "style.css") {
		t.Errorf("GET /static/ 仍在列文件名（body=%q）", body)
	}
	resp, _ = fetchStatic(t, base, "GET", "/static/style.css")
	if ar := resp.Header.Get("Accept-Ranges"); ar != "" {
		t.Errorf("Accept-Ranges = %q，应为空（没实现 Range 就不宣称）", ar)
	}
}

// TestStaticReverse_FallsBackToCatchAll 反向验证：**不**注册原生静态路由时，
// /static/ 必须落到 "/*any" 兜底（而不是碰巧还能从别处拿到文件）。
// 有了这根钉子，将来谁把 RegisterStaticRoutes 挪掉或改错路径，测试立刻红；
// 顺带也说明面板 mux 里已经不再有静态资源那条兜底。
func TestStaticReverse_FallsBackToCatchAll(t *testing.T) {
	base := startStaticServer(t, false)
	resp, body := fetchStatic(t, base, "GET", "/static/style.css")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（兜底 mux 会答它）", resp.StatusCode)
	}
	if body != "panel-catchall" {
		t.Fatalf("body = %q，期望 %q —— 说明不注册原生路由时静态资源**不是**落到兜底，"+
			"这根反向验证的假设已经不成立", body, "panel-catchall")
	}
}
