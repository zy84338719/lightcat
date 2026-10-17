package web

import (
	"context"
	"time"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/oekit/hertz/health"

	"github.com/zy84338719/lightcat/internal/store"
)

// 探针路径。集中定义，deploy/k8s 与 docker-compose 的探针配置都按这里对齐。
const (
	// PingPath 是**存活**探针：只看进程还在不在，不碰任何依赖。
	// k8s liveness 用它 —— 数据库忙的时候绝不该把容器重启掉。
	PingPath = "/pingz"
	// HealthPath 是**就绪**探针：会真去 ping 一次数据库。
	// k8s readiness 用它 —— 库还写不进去时就不该再往这个实例导流量。
	HealthPath = "/healthz"
	// VersionPath 是版本信息端点（出问题时让用户把这行贴过来最省事）。
	VersionPath = "/buildVersion"
)

// healthCheckTimeout 是就绪探针里单项检查的上限。
const healthCheckTimeout = 2 * time.Second

// RegisterHealthRoutes 在 Hertz 引擎上挂存活/就绪/版本三个端点。
//
// 为什么要有这一组（而不是继续拿面板首页当探针）：
// 原来 deploy/k8s 的 startup/readiness/liveness 三条探针全部打 `GET /`，
// 而 `/` 是**面板首页**——要渲染模板、查设备表、算统计。一次探活把整条重链路
// 走一遍：库忙时探针就超时，k8s 会误判并把好端端的容器重启掉；
// 面板开了鉴权时它还会 401，让「服务是不是活着」这件事本身依赖了登录态。
//
// 分工（也符合 k8s 探针的通行约定）：
//   - liveness → /pingz：只答 pong，不碰依赖，故障时不重启容器；
//   - readiness → /healthz：ping 一次库，通不过就临时摘流量；
//   - 版本 → /buildVersion。
//
// 路由优先级：这三个是 Hertz 静态路径注册，优先于 NewHertzEngine 里的
// "/*any" 兜底（radix 树里静态段优先于通配），所以不会被面板 mux 截走。
func RegisterHealthRoutes(e *server.Hertz, st *store.Store) {
	health.RegisterHealthRoutes(e,
		health.WithPingPath(PingPath),
		health.WithHealthPath(HealthPath),
		health.WithVersionPath(VersionPath),
		// 就绪探针唯一要验的依赖就是 SQLite。SQLite 不吃 TCP，
		// 所以检查方式是 ping 而不是 dial —— 真去开连接反而可能撞上文件锁。
		health.WithCheckers(health.NamedChecker("sqlite", func(ctx context.Context) error {
			ctx, cancel := context.WithTimeout(ctx, healthCheckTimeout)
			defer cancel()
			return st.Ping(ctx)
		})),
		health.WithCheckTimeout(healthCheckTimeout),
	)
}
