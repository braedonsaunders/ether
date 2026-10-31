package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
)

func TestQueryStatsCountsStatementsRowsAndTransactions(t *testing.T) {
	ctx := context.Background()
	db, err := OpenPath(ctx, filepath.Join(t.TempDir(), "stats.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `CREATE TABLE qs_items (id INTEGER PRIMARY KEY, name TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO qs_items(name) VALUES ('a'),('b'),('c')`); err != nil {
		t.Fatal(err)
	}

	reqCtx, stats := WithQueryStats(ctx)
	rows, err := db.QueryContext(reqCtx, `SELECT id, name FROM qs_items ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for rows.Next() {
		n++
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	var name string
	if err := db.QueryRowContext(reqCtx, `SELECT name FROM qs_items WHERE id=?`, 2).Scan(&name); err != nil || name != "b" {
		t.Fatalf("scan = %q, %v", name, err)
	}
	tx, err := db.BeginTx(reqCtx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(reqCtx, `UPDATE qs_items SET name='z' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	if n != 3 {
		t.Fatalf("read %d rows", n)
	}
	if got := stats.Queries(); got != 3 {
		t.Fatalf("queries = %d, want 3", got)
	}
	if got := stats.Rows(); got != 4 {
		t.Fatalf("rows = %d, want 4", got)
	}
	if stats.DBTime() <= 0 {
		t.Fatal("no driver time recorded")
	}
	if got := stats.Writes(); got != 1 {
		t.Fatalf("writes = %d, want 1", got)
	}
	st := stats.Statements()
	if len(st) != 3 {
		t.Fatalf("statements = %+v", st)
	}

	// Work outside the request context is not attributed to it.
	if _, err := db.ExecContext(ctx, `UPDATE qs_items SET name='y' WHERE id=2`); err != nil {
		t.Fatal(err)
	}
	if got := stats.Queries(); got != 3 {
		t.Fatalf("unattributed work counted: %d", got)
	}
}

func TestNestedQueryStatsRecordIntoBoth(t *testing.T) {
	ctx := context.Background()
	db, err := OpenPath(ctx, filepath.Join(t.TempDir(), "nested.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	outerCtx, outer := WithQueryStats(ctx)
	innerCtx, inner := WithQueryStats(outerCtx)
	if QueryStatsFrom(innerCtx) != inner || QueryStatsFrom(outerCtx) != outer {
		t.Fatal("QueryStatsFrom returned the wrong stats")
	}
	var one int
	if err := db.QueryRowContext(innerCtx, `SELECT 1`).Scan(&one); err != nil {
		t.Fatal(err)
	}
	if inner.Queries() != 1 || outer.Queries() != 1 || inner.Rows() != 1 || outer.Rows() != 1 {
		t.Fatalf("inner=%d/%d outer=%d/%d", inner.Queries(), inner.Rows(), outer.Queries(), outer.Rows())
	}
}

func TestQueryStatsColumnTypesUnchanged(t *testing.T) {
	ctx := context.Background()
	db, err := OpenPath(ctx, filepath.Join(t.TempDir(), "types.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `CREATE TABLE qs_t (id INTEGER, label TEXT)`); err != nil {
		t.Fatal(err)
	}
	columns := func(ctx context.Context) []string {
		rows, err := db.QueryContext(ctx, `SELECT id, label FROM qs_t`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		types, err := rows.ColumnTypes()
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, ct := range types {
			out = append(out, fmt.Sprint(ct.DatabaseTypeName(), "/", ct.ScanType()))
		}
		return out
	}
	plain := columns(ctx)
	statsCtx, _ := WithQueryStats(ctx)
	counted := columns(statsCtx)
	if len(plain) != len(counted) {
		t.Fatalf("plain %v counted %v", plain, counted)
	}
	for i := range plain {
		if plain[i] != counted[i] {
			t.Fatalf("plain %v counted %v", plain, counted)
		}
	}
}
