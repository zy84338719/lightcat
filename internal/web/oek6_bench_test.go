package web

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/zy84338719/lightcat/internal/store"
)

// OEK-6 的**压测基线**。issue 写死了前置条件：「先压测拿基线再决定，不为优化而优化」。
//
// 结论（Apple M4 / macOS 27 / Go 1.27.1，benchtime=2s）：
//
//	GetSetting（每请求那条主键 SELECT）   3,942 ns/op   19 allocs
//	ListInforms（面板本来就在查）        19,765 ns/op   31 allocs
//	ListAlerts （面板本来就在查）        26,075 ns/op   30 allocs
//	GetDevice （面板本来就在查）         32,338 ns/op  109 allocs
//
// GetSetting 是这几项里**最便宜**的，占一个 GetDevice 的 12%。对照 NFR-4 的
// P95 < 50 ms，它只占 0.008%；即便与写操作争抢单连接被拖到 27 µs（见下面那条用例）
// 也只有 0.05%。所以**没有做 settings 缓存** —— 加缓存换来的��是那 19 次分配，
// 代价是「改完立即生效」这条语义与一层需要单独测的失效逻辑。
//
// 这份基准留着不是为了将来引入缓存，而是为了让「要不要缓存」这个问题
// 以后再被提起时有现成的数可查，而不是重新争论一遍。
//
// 复跑：go test ./internal/web/ -run='^$' -bench='OEK6' -benchmem

func oek6Store(b *testing.B) *store.Store {
	b.Helper()
	st, err := store.Open(filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatalf("打开库失败: %v", err)
	}
	b.Cleanup(func() { st.Close() })
	return st
}

func oek6Seed(b *testing.B, st *store.Store, n int) []int64 {
	b.Helper()
	if err := st.SetSetting(settingAPIToken, "tok-abcdef"); err != nil {
		b.Fatalf("写设置失败: %v", err)
	}
	ids := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		id, _, err := st.UpsertDevice(&store.Device{
			OUI: "001122", ProductClass: "R",
			SerialNumber: fmt.Sprintf("OEK6%04d", i), ModelName: "Model",
		})
		if err != nil {
			b.Fatalf("造设备失败: %v", err)
		}
		ids = append(ids, id)
	}
	return ids
}

// BenchmarkOEK6Baseline 对照组：GetSetting 与面板每个请求本来就要做的那些查询。
func BenchmarkOEK6Baseline(b *testing.B) {
	st := oek6Store(b)
	ids := oek6Seed(b, st, 20)
	devID := ids[0]

	b.Run("GetSetting", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, ok, err := st.GetSetting(settingAPIToken); err != nil || !ok {
				b.Fatalf("读取失败 ok=%v err=%v", ok, err)
			}
		}
	})
	b.Run("GetDevice", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := st.GetDevice(devID); err != nil {
				b.Fatalf("读取失败: %v", err)
			}
		}
	})
	b.Run("ListAlerts", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := st.ListAlerts(20); err != nil {
				b.Fatalf("读取失败: %v", err)
			}
		}
	})
	b.Run("ListInforms", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := st.ListInforms(devID, 20); err != nil {
				b.Fatalf("读取失败: %v", err)
			}
		}
	})
}

// BenchmarkOEK6CurrentAPIToken 连回落逻辑一起量 —— 这是 /api 与 /metrics
// 每请求真正走的那条（面板凭据不在此列：Creds.Guard 用的是内存里的 live 对象，
// 本来就不读库）。
func BenchmarkOEK6CurrentAPIToken(b *testing.B) {
	st := oek6Store(b)
	oek6Seed(b, st, 1)
	opt := Options{}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if tok := currentAPIToken(st, opt); tok != "tok-abcdef" {
			b.Fatalf("token = %q", tok)
		}
	}
}

// BenchmarkOEK6UnderWrites 量 issue 真正担心的那件事：这条 SELECT 与写操作
// **争同一个连接**（MaxOpenConns=1 是规避 SQLITE_BUSY 的既定决策），所以
// 单看裸成本不够 —— 要看并发写下它被拖慢多少。
func BenchmarkOEK6UnderWrites(b *testing.B) {
	st := oek6Store(b)
	oek6Seed(b, st, 20)

	stop := make(chan struct{})
	b.Cleanup(func() { close(stop) })
	go func() {
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			_, _, _ = st.UpsertDevice(&store.Device{
				OUI: "001122", ProductClass: "R",
				SerialNumber: fmt.Sprintf("WR%06d", i), ModelName: "M",
			})
			i++
		}
	}()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok, err := st.GetSetting(settingAPIToken); err != nil || !ok {
			b.Fatalf("读取失败 ok=%v err=%v", ok, err)
		}
	}
}
