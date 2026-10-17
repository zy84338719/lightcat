package cwmp

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oekit/concurrency/loop"
)

// TestLoopSurvivesPanicInTick 钉住 loop.Run 换掉手写 ticker 换来的东西。
//
// 改之前三处周期循环（离线巡检 / 会话清理 / ontweb 采集）都是「整个 for-select
// 塞进一个 goBg 任务」。一轮 panic 虽然被 GoSafe recover、**进程不崩**，但那会把
// 循环协程一起带走：离线巡检静默停摆（设备离线永远不判）、会话清理停摆（超时会话
// 永不释放）、ontweb 采集停摆 —— 而且**没有任何告警**，因为 recover 把它吃干净了。
//
// loop.Run 的单轮 panic 收编（async.RunSafe）让循环存活，这正是这次换掉的理由。
func TestLoopSurvivesPanicInTick(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var ticks atomic.Int32
	l := loop.Run(ctx, loop.Config{Interval: time.Millisecond, Name: "panic-probe"},
		func(context.Context) {
			if ticks.Add(1) == 3 {
				panic("某一轮炸了")
			}
		})
	time.Sleep(150 * time.Millisecond)
	cancel()
	l.Wait()

	if got := ticks.Load(); got < 10 {
		t.Errorf("第 3 轮 panic 后循环应该继续跑，实际只跑了 %d 轮（panic 打断了循环？）", got)
	}
}

// TestLoopDrainsOnShutdown 停机排水：ctx 取消只拦「下一轮」，在飞轮次用独立 ctx
// 跑完 —— 否则关库时正在写库的巡检会被拦腰砍断（OEK-2 那条关停契约）。
func TestLoopDrainsOnShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	var finished atomic.Bool
	var started sync.WaitGroup
	started.Add(1)
	l := loop.Run(ctx, loop.Config{Interval: time.Millisecond, Name: "drain-probe"},
		func(context.Context) {
			started.Done()
			time.Sleep(60 * time.Millisecond) // 在飞轮次
			finished.Store(true)
		})
	started.Wait() // 确保有一轮已经在飞
	cancel()       // 此刻它还在跑
	l.Wait()       // 排水等它跑完

	if !finished.Load() {
		t.Error("在飞轮次被 ctx 取消打断了 —— 关库时这会砍断正在写库的巡检")
	}
}
