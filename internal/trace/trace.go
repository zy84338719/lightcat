// Package trace 给「一次请求触发的全部日志」串上同一个 trace_id。
//
// 要解决的问题：一次设备 Inform 会打出「收到 Inform」「已入队：取设备基本信息」
// 「入队采集光功率」等十几条日志，散在 cwmp、web 两层。用户报障贴一段日志出来，
// 我们没法回答「这十几条是不是同一次上报」。
//
// 三块拼图：
//   - 注入：Hertz 中间件给每个请求生成 trace_id 并塞进 ctx；
//   - 取值：用 oekit/tracing/traceid 的 ctx 键，这样 logx / hertz 那些 oekit
//     组件（init 里已经向 logx 注册过同一个提取器）也能拿到同一个值；
//   - 落地：SlogHandler 包一层 slog.Handler，从 ctx 里取出 trace_id 补成字段，
//     **不动用户可见的日志格式**（只是在末尾多一个 trace_id=…）。
//
// 刻意不把全仓 *slog.Logger 换成 logx 门面：本仓是单二进制自部署的家用设备，
// 不是多租户微服务，logx 的价值（多服务统一 sink、跨服务 trace 串联）在这里
// 不存在，而全量替换要动每个日志调用点，可观测性却没有实质增加（见
// docs/requirements.md §14.5）。
package trace

import (
	"context"
	"log/slog"

	"github.com/google/uuid"
	"github.com/oekit/tracing/traceid"
)

// Header 是 trace_id 的 HTTP 头名。上游（WAF、反代、日志采集）带了它就沿用，
// 没带才自己生成 —— 这样一次请求穿过多层代理时，trace_id 不会在每一层都变。
const Header = "X-Trace-Id"

// Key 是日志字段名。
const Key = "trace_id"

// New 生成一个新的 trace_id。
//
// 用 UUID v4 而不是短随机串：trace_id 会出现在面板上、会被用户复制去搜日志，
// 撞了就查错。UUID 的碰撞概率在单进程的设备量级下可以忽略。
func New() string { return uuid.NewString() }

// With 把 trace_id 注入 ctx。
func With(ctx context.Context, id string) context.Context { return traceid.ContextWith(ctx, id) }

// From 从 ctx 取 trace_id；没有则返回空串。
func From(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	return traceid.FromContext(ctx)
}

// Ensure 返回带 trace_id 的 ctx：已有就沿用，没有就现生成一个。
func Ensure(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if From(ctx) != "" {
		return ctx
	}
	return With(ctx, New())
}

// SlogHandler 给 slog.Handler 包一层：每条记录都补上 ctx 里的 trace_id。
//
// 为什么必须包一层而不是靠调用点自己加属性：trace_id 挂在 ctx 上，而
// slog.Logger.Info（无 Context 变体）根本拿不到 ctx —— 靠调用点加就等于
// 每个日志点都要记得传，既容易漏又会污染调用签名。
func SlogHandler(inner slog.Handler) slog.Handler {
	if inner == nil {
		return nil
	}
	return &handler{inner: inner}
}

type handler struct {
	inner slog.Handler
}

func (h *handler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h *handler) Handle(ctx context.Context, r slog.Record) error {
	// 已经有 trace_id（调用点用 With 显式带上的，或外层已经包过一层）就不重复补。
	if id := From(ctx); id != "" && !recordHasKey(r, Key) {
		r.AddAttrs(slog.String(Key, id))
	}
	return h.inner.Handle(ctx, r)
}

func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	return &handler{inner: h.inner.WithAttrs(attrs)}
}

func (h *handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return &handler{inner: h.inner.WithGroup(name)}
}

// recordHasKey 判记录里是否已经带了某个 key（slog 没有提供这个查询）。
func recordHasKey(r slog.Record, key string) bool {
	found := false
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			found = true
			return false
		}
		return true
	})
	return found
}
