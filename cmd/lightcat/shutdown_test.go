package main

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	oekitversion "github.com/oekit/core/version"

	"github.com/zy84338719/lightcat/internal/store"
	"github.com/zy84338719/lightcat/internal/web"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestShutdownBackground_DrainsInFlightTasks 盯住 OEK-2 的关停契约：
// shutdownBackground 返回时，池里的任务必须已经跑完。
//
// 为什么重要：后台任务（巡检、探测、webhook）都要写库。不等它们收工就关库，
// 这些写在关停窗口里撞上 "database is closed"，探测结果与 Inform 流水白丢。
func TestShutdownBackground_DrainsInFlightTasks(t *testing.T) {
	bg := newBackgroundPool()
	var done atomic.Int32

	// 任务比 shutdown 超时短，正常应当全部跑完
	for range 32 {
		bg.Go(func() {
			time.Sleep(30 * time.Millisecond)
			done.Add(1)
		})
	}

	shutdownBackground(bg, quietLogger())

	if got := done.Load(); got != 32 {
		t.Errorf("排空后完成任务数 = %d，期望 32（后台任务还在写库时就关了）", got)
	}
	if !bg.Closed() {
		t.Error("排空后池应处于 Closed 状态")
	}
}

// TestShutdownBackground_Idempotent 确认重复调用无害 —— 正常路径显式调一次，
// defer 里还要兜底再调一次，两条路径撞上不能出问题。
func TestShutdownBackground_Idempotent(t *testing.T) {
	bg := newBackgroundPool()
	bg.Go(func() {})

	shutdownBackground(bg, quietLogger())
	shutdownBackground(bg, quietLogger())
	shutdownBackground(bg, quietLogger())
}

// TestShutdownBackground_ToleratesTimeout 关停不能被一个卡住的任务无限拖住：
// ctx 到点就返回，宁可丢最后几条结果也不能吊着进程不退。
func TestShutdownBackground_ToleratesTimeout(t *testing.T) {
	bg := newBackgroundPool()
	release := make(chan struct{})
	defer close(release)

	// 卡住的任务：池排空会一直等到 ctx 超时
	bg.Go(func() { <-release })

	done := make(chan struct{})
	go func() {
		defer close(done)
		shutdownBackground(bg, quietLogger())
	}()

	select {
	case <-done:
	case <-time.After(backgroundDrainTimeout + 3*time.Second):
		t.Fatal("排空超过上限仍未返回 —— 关停窗口没有上界")
	}
}

// TestShutdownBeforeStoreClose 端到端钉住顺序：后台任务写库时触发关库，
// 不该出现 "database is closed" 类的写失败。
//
// 这正是 §13 里 OEK-2 描述的验收口径（压测打流量 + SIGTERM），
// 这里用最小可复现的形式把它固化下来。
func TestShutdownBeforeStoreClose(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/order.db")
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	bg := newBackgroundPool()
	var writeFailed atomic.Int32

	// 模拟离线探测回写。跟真实后台循环一样：ctx 一取消就退出，
	// 收尾阶段仍可能再写一次库 —— 那正是排空要等的部分。
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	for range 8 {
		bg.Go(func() {
			for {
				if ctx.Err() != nil {
					// 收尾再写一次；此刻库还开着才说明顺序对了
					if _, _, err := st.GetSetting("no_such_key"); err != nil {
						writeFailed.Add(1)
					}
					return
				}
				// 库已关还在写 = 顺序反了
				if st.Closed() {
					writeFailed.Add(1)
					return
				}
				_, _, _ = st.GetSetting("no_such_key")
				time.Sleep(time.Millisecond)
			}
		})
	}

	time.Sleep(50 * time.Millisecond)
	// 顺序：ctx 取消 → 排空 → 最后才关库
	cancel()
	shutdownBackground(bg, quietLogger())
	if err := st.Close(); err != nil {
		t.Fatalf("关库失败: %v", err)
	}

	if got := writeFailed.Load(); got != 0 {
		t.Errorf("排空后仍有 %d 次后台写撞上已关闭的库（关停顺序反了）", got)
	}
}

// TestSyncOekitVersion 版本号必须只有一个出处。
//
// /buildVersion 端点（oekit/hertz/versionh）读的是 oekit/core/version 的包级变量，
// 面板展示读的是 web.Version。两者不同步的话，用户报障贴出来的是 "dev"，
// 运维无从判断版本 —— 这条断言就是防这个。
func TestSyncOekitVersion(t *testing.T) {
	syncOekitVersion()
	if oekitversion.Version != web.Version {
		t.Errorf("/buildVersion 会报 %q，面板显示 %q —— 两者必须一致",
			oekitversion.Version, web.Version)
	}
}
