package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"modernc.org/sqlite"
)

func TestCachedSQLiteStatementsReuse(t *testing.T) {
	db, err := sql.Open("sqlite-cached", "file::memory:?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `CREATE TABLE sample(id INTEGER PRIMARY KEY, name TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO sample(name) VALUES(?)`, "one"); err != nil {
		t.Fatal(err)
	}
	read := func() string {
		t.Helper()
		var name string
		if err := db.QueryRowContext(ctx, `SELECT name FROM sample WHERE id=?`, 1).Scan(&name); err != nil {
			t.Fatal(err)
		}
		return name
	}
	if read() != "one" || read() != "one" {
		t.Fatal(read())
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sample SET name=? WHERE id=?`, "two", 1); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if read() != "two" {
		t.Fatal(read())
	}
}

func TestCachedStatementsDoNotSerializeReaders(t *testing.T) {
	elapsed := func(driver string) time.Duration {
		t.Helper()
		db, err := sql.Open(driver, filepath.Join(t.TempDir(), "browse.db")+"?_pragma=busy_timeout(5000)")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		db.SetMaxOpenConns(4)
		ctx := context.Background()
		if _, err := db.ExecContext(ctx, `CREATE TABLE sample(id INTEGER PRIMARY KEY, name TEXT)`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO sample(name) VALUES(?)`, "one"); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		var wg sync.WaitGroup
		for n := 0; n < 8; n++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 40; i++ {
					var name string
					if err := db.QueryRowContext(ctx, `SELECT name FROM sample WHERE id=?`, 1).Scan(&name); err != nil {
						t.Errorf("read: %v", err)
						return
					}
				}
			}()
		}
		wg.Wait()
		return time.Since(start)
	}
	raw := elapsed("sqlite")
	cached := elapsed("sqlite-cached")
	if cached > time.Second || (raw > 0 && cached > raw*3 && cached > 150*time.Millisecond) {
		t.Fatalf("cached reads %s, plain sqlite %s", cached, raw)
	}
}

func BenchmarkCachedBrowseRead(b *testing.B) {
	db, err := sql.Open("sqlite-cached", filepath.Join(b.TempDir(), "browse.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(4)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `CREATE TABLE sample(id INTEGER PRIMARY KEY, name TEXT)`); err != nil {
		b.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO sample(name) VALUES(?)`, "one"); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			var name string
			if err := db.QueryRowContext(ctx, `SELECT name FROM sample WHERE id=?`, 1).Scan(&name); err != nil {
				b.Fatal(err)
			}
		}
	})
}

var (
	holdOnce sync.Once
	holdCur  atomic.Int32
	holdPeak atomic.Int32
)

func TestParallelSlowQueriesOverlap(t *testing.T) {
	holdOnce.Do(func() {
		err := sqlite.RegisterScalarFunction("ether_stmt_hold", 0, func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) {
			n := holdCur.Add(1)
			for {
				p := holdPeak.Load()
				if n <= p || holdPeak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(300 * time.Millisecond)
			holdCur.Add(-1)
			return int64(1), nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})
	holdCur.Store(0)
	holdPeak.Store(0)
	db, err := sql.Open("sqlite-cached", filepath.Join(t.TempDir(), "slow.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(4)
	ctx := context.Background()
	start := time.Now()
	var wg sync.WaitGroup
	for n := 0; n < 2; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var n int64
			if err := db.QueryRowContext(ctx, `SELECT ether_stmt_hold()`).Scan(&n); err != nil {
				t.Errorf("slow query: %v", err)
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)
	if holdPeak.Load() < 2 || elapsed > 450*time.Millisecond {
		t.Fatalf("same statement serialized: peak %d elapsed %s", holdPeak.Load(), elapsed)
	}
}
