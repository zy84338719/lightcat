package store

import (
	"fmt"
	"path/filepath"
	"testing"
)

// 参数入库是全场最热的写路径：一台 V271 约 4700 个参数，分批（单批上限 200）
// 灌进来。docs/notes/loadtest.md 的结论 2 记着「吞吐卡在约 1.6–1.8 万参数行/秒」，
// 瓶颈是 MaxOpenConns(1) 的串行化 —— 所以这里量的是「每批里 Prepare 占多少」，
// 决定值不值得把 Prepare 提到连接级复用。

// testingT 兼容 *testing.T 与 *testing.B，让同一套夹具两边都能用。
type testingT interface {
	Helper()
	Fatalf(string, ...any)
	Cleanup(func())
	TempDir() string
}

func benchStoreParams(tb testingT) (*Store, int64) {
	tb.Helper()
	st, err := Open(filepath.Join(tb.TempDir(), "params.db"))
	if err != nil {
		tb.Fatalf("打开库失败: %v", err)
	}
	tb.Cleanup(func() { st.Close() })
	id, _, err := st.UpsertDevice(&Device{OUI: "001122", ProductClass: "R", SerialNumber: "P1"})
	if err != nil {
		tb.Fatalf("建设备失败: %v", err)
	}
	return st, id
}

func benchParams(n int) []Param {
	out := make([]Param, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, Param{
			Name:  fmt.Sprintf("InternetGatewayDevice.LANDevice.1.X_HW_APDevice.3.SomeLeaf.%d", i),
			Value: "1",
		})
	}
	return out
}

// BenchmarkUpsertParamsBatch 一批 200 条（真机单批上限）。
func BenchmarkUpsertParamsBatch(b *testing.B) {
	st, devID := benchStoreParams(b)
	params := benchParams(200)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := st.UpsertParams(devID, params, "getvalues"); err != nil {
			b.Fatalf("写入失败: %v", err)
		}
	}
}

// BenchmarkUpsertParamsBatchReinsert 同一批**重复**写：走 ON CONFLICT 更新分支，
// 更接近真实（设备每轮都报同一批参数）。
func BenchmarkUpsertParamsBatchReinsert(b *testing.B) {
	st, devID := benchStoreParams(b)
	params := benchParams(200)
	if err := st.UpsertParams(devID, params, "getvalues"); err != nil {
		b.Fatalf("预热失败: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := st.UpsertParams(devID, params, "getvalues"); err != nil {
			b.Fatalf("写入失败: %v", err)
		}
	}
}

// BenchmarkUpsertParamsPrepareCost 单独量 Prepare 本身的成本：
// 若是整批耗时的可观比例，那「连接级复用 prepared statement」才值得做；
// 若只占零头，就别动它（连接级 statement 复用会引入跨事务状态，值不回票价）。
func BenchmarkUpsertParamsPrepareCost(b *testing.B) {
	st, _ := benchStoreParams(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tx, err := st.db.Begin()
		if err != nil {
			b.Fatal(err)
		}
		stmt, err := tx.Prepare(upsertParamSQL)
		if err != nil {
			b.Fatal(err)
		}
		_ = stmt.Close()
		_ = tx.Rollback()
	}
}
