package store

import (
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// loadtest.md 的待办 #1：「批量多行 INSERT ... ON CONFLICT 代替逐行 upsert，减少语句数
// 与 prepare 开销」。这里先原型 + 实测，值不值得做由数字决定 —— 不为优化而优化。

// upsertParamsMultiRow 一条语句写 N 行。
//
// ON CONFLICT 里的 excluded 指的是**本行**那组待插入的值，多行与逐行语义一致
// （这是它能等价替换的关键）。
func upsertParamsMultiRow(tx *sql.Tx, deviceID int64, params []Param, source, now string) error {
	if len(params) == 0 {
		return nil
	}
	const cols = 7
	var b strings.Builder
	b.WriteString(`INSERT INTO device_params
		(device_id, name, value, value_type, writable, source, updated_at) VALUES `)
	args := make([]any, 0, cols*len(params))
	for i, p := range params {
		if p.Name == "" {
			continue
		}
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString("(?,?,?,?,?,?,?)")
		t := p.ValueType
		if t == "" {
			t = "string"
		}
		wr := 0
		if p.Writable {
			wr = 1
		}
		args = append(args, deviceID, p.Name, p.Value, t, wr, source, now)
	}
	if len(args) == 0 {
		return nil
	}
	b.WriteString(upsertParamTail)
	_, err := tx.Exec(b.String(), args...)
	return err
}

// multiRowChunk 是每条语句的行数。7 列 × 100 行 = 700 个变量，
// 稳稳落在最老的 SQLITE_MAX_VARIABLE_NUMBER（999）之下，不挑 SQLite 版本。
const multiRowChunk = 100

func upsertParamsMultiRowChunked(tx *sql.Tx, deviceID int64, params []Param, source, now string) error {
	for start := 0; start < len(params); start += multiRowChunk {
		end := start + multiRowChunk
		if end > len(params) {
			end = len(params)
		}
		if err := upsertParamsMultiRow(tx, deviceID, params[start:end], source, now); err != nil {
			return err
		}
	}
	return nil
}

// BenchmarkUpsertParamsStrategies 三种写法同场比：这是「改不改」的依据。
func BenchmarkUpsertParamsStrategies(b *testing.B) {
	const batch = 200

	b.Run("逐行Exec(现状)", func(b *testing.B) {
		st, devID := benchStoreParams(b)
		params := benchParams(batch)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if err := st.UpsertParams(devID, params, "getvalues"); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("多行单语句", func(b *testing.B) {
		st, devID := benchStoreParams(b)
		params := benchParams(batch)
		now := ts(time.Now())
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			tx, err := st.db.Begin()
			if err != nil {
				b.Fatal(err)
			}
			if err := upsertParamsMultiRowChunked(tx, devID, params, "getvalues", now); err != nil {
				b.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				b.Fatal(err)
			}
		}
	})

	// 重复写（走 ON CONFLICT 更新分支）才是真机的常态：设备每轮都报同一批参数。
	b.Run("逐行Exec-重复写", func(b *testing.B) {
		st, devID := benchStoreParams(b)
		params := benchParams(batch)
		if err := st.UpsertParams(devID, params, "getvalues"); err != nil {
			b.Fatal(err)
		}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if err := st.UpsertParams(devID, params, "getvalues"); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("多行单语句-重复写", func(b *testing.B) {
		st, devID := benchStoreParams(b)
		params := benchParams(batch)
		now := ts(time.Now())
		if err := st.UpsertParams(devID, params, "getvalues"); err != nil {
			b.Fatal(err)
		}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			tx, err := st.db.Begin()
			if err != nil {
				b.Fatal(err)
			}
			if err := upsertParamsMultiRowChunked(tx, devID, params, "getvalues", now); err != nil {
				b.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// TestMultiRowEquivalence 多行写法必须与逐行写法**逐行等价** —— 尤其是
// source='getnames' 那条 CASE WHEN 分支（名字枚举不带值，不能把已有值刷空）。
func TestMultiRowEquivalence(t *testing.T) {
	st, devID := benchStoreParams(t)
	params := benchParams(50)
	if err := st.UpsertParams(devID, params, "getvalues"); err != nil {
		t.Fatalf("逐行写入失败: %v", err)
	}

	// 再用多行写法重写一遍，source 换成 getnames
	now := ts(time.Now())
	tx, err := st.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	names := make([]Param, len(params))
	for i, p := range params {
		names[i] = Param{Name: p.Name, Value: "", ValueType: "", Writable: false}
	}
	if err := upsertParamsMultiRowChunked(tx, devID, names, "getnames", now); err != nil {
		t.Fatalf("多行写入失败: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	got, err := st.ListParams(devID)
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if len(got) != len(params) {
		t.Fatalf("读回 %d 条，期望 %d", len(got), len(params))
	}
	nonEmpty := 0
	for _, p := range got {
		if p.Value == "" {
			t.Errorf("参数 %s 的值被 getnames 刷空了 —— CASE WHEN 分支没生效", p.Name)
			continue
		}
		nonEmpty++
	}
	if nonEmpty != len(params) {
		t.Errorf("有值的参数 %d 条，期望 %d", nonEmpty, len(params))
	}
}

// 多行 VALUES 子句在行数固定时**完全相同**（都是重复的 "(?,?,?,?,?,?)"），
// 只有 args 不同 —— 所以 SQL 文本可以按行数缓存，配合 db.Prepare 的
// statement 缓存做到「一次解析、多次执行」。
type cachedMultiRow struct {
	db    *sql.DB
	stmts sync.Map // 行数 → *sql.Stmt
}

func newCachedMultiRow(db *sql.DB) *cachedMultiRow { return &cachedMultiRow{db: db} }

func (c *cachedMultiRow) stmtFor(n int) (*sql.Stmt, error) {
	if v, ok := c.stmts.Load(n); ok {
		return v.(*sql.Stmt), nil
	}
	var b strings.Builder
	b.WriteString(upsertParamHead)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString("(?,?,?,?,?,?,?)")
	}
	b.WriteString(upsertParamTail)
	st, err := c.db.Prepare(b.String())
	if err != nil {
		return nil, err
	}
	c.stmts.Store(n, st)
	return st, nil
}

func (c *cachedMultiRow) upsert(tx *sql.Tx, deviceID int64, params []Param, source, now string) error {
	if len(params) == 0 {
		return nil
	}
	st, err := c.stmtFor(len(params))
	if err != nil {
		return err
	}
	stmt := tx.Stmt(st) // 复用连接级 statement，不在本事务里重新 Prepare
	defer stmt.Close()
	args := make([]any, 0, 7*len(params))
	for _, p := range params {
		if p.Name == "" {
			continue
		}
		t := p.ValueType
		if t == "" {
			t = "string"
		}
		wr := 0
		if p.Writable {
			wr = 1
		}
		args = append(args, deviceID, p.Name, p.Value, t, wr, source, now)
	}
	if len(args) == 0 {
		return nil
	}
	_, err = stmt.Exec(args...)
	return err
}

// BenchmarkUpsertParamsCachedMultiRow 缓存 SQL 文本后的多行写法：既少语句数，
// 又不重复解析。
func BenchmarkUpsertParamsCachedMultiRow(b *testing.B) {
	const batch = 200
	for _, mode := range []string{"首写", "重复写"} {
		b.Run(mode, func(b *testing.B) {
			st, devID := benchStoreParams(b)
			params := benchParams(batch)
			c := newCachedMultiRow(st.db)
			now := ts(time.Now())
			// MaxOpenConns(1) 下事务已占住唯一连接，此时再 db.Prepare 会死锁 ——
			// 所以语句必须在 Begin 之前备好（生产实现要照这一点）。
			if _, err := c.stmtFor(len(params)); err != nil {
				b.Fatalf("预备语句失败: %v", err)
			}
			run := func() {
				tx, err := st.db.Begin()
				if err != nil {
					b.Fatal(err)
				}
				if err := c.upsert(tx, devID, params, "getvalues", now); err != nil {
					b.Fatal(err)
				}
				if err := tx.Commit(); err != nil {
					b.Fatal(err)
				}
			}
			if mode == "重复写" {
				run()
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				run()
			}
		})
	}
}

// BenchmarkUpsertParamsChunkSizes 分块大小是不是选得不公平？逐档扫一遍再下结论。
func BenchmarkUpsertParamsChunkSizes(b *testing.B) {
	params := benchParams(200)
	for _, chunk := range []int{5, 10, 25, 50, 100, 200} {
		b.Run(fmt.Sprintf("chunk=%d", chunk), func(b *testing.B) {
			st, devID := benchStoreParams(b)
			now := ts(time.Now())
			if err := st.UpsertParams(devID, params, "getvalues"); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				tx, err := st.db.Begin()
				if err != nil {
					b.Fatal(err)
				}
				for s := 0; s < len(params); s += chunk {
					e := s + chunk
					if e > len(params) {
						e = len(params)
					}
					if err := upsertParamsMultiRow(tx, devID, params[s:e], "getvalues", now); err != nil {
						b.Fatal(err)
					}
				}
				if err := tx.Commit(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
