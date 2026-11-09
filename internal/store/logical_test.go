package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func seedLogical(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO storetest_links(id, label, created) VALUES(?, ?, ?)`, []any{5, "five \"quoted\" ? 'x'", 1700000000}},
		{`INSERT INTO storetest_links(id, label, created) VALUES(?, ?, ?)`, []any{9, "nine", 0}},
		{`INSERT INTO storetest_child(link_id, k) VALUES(?, ?)`, []any{5, 1}},
		{`INSERT INTO storetest_queue(body, score, blob, done) VALUES(?, ?, ?, ?)`, []any{"a", 0.25, []byte{0, 0xff, 'z'}, 1}},
		{`INSERT INTO storetest_queue(body, score, blob, done) VALUES(?, ?, ?, ?)`, []any{"b", -3.5, nil, 0}},
		{`UPDATE storetest_settings SET note=? WHERE id=1`, []any{"changed"}},
	} {
		if _, err := db.ExecContext(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
}

func checkLogical(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	var label string
	var created int64
	if err := db.QueryRowContext(ctx, `SELECT label, created FROM storetest_links WHERE id=5`).Scan(&label, &created); err != nil {
		t.Fatal(err)
	}
	if label != "five \"quoted\" ? 'x'" || created != 1700000000 {
		t.Fatalf("link 5 = %q %d", label, created)
	}
	var blob []byte
	var score float64
	if err := db.QueryRowContext(ctx, `SELECT blob, score FROM storetest_queue WHERE body='a'`).Scan(&blob, &score); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(blob, []byte{0, 0xff, 'z'}) || score != 0.25 {
		t.Fatalf("queue a = %v %v", blob, score)
	}
	var nullBlob []byte
	if err := db.QueryRowContext(ctx, `SELECT blob FROM storetest_queue WHERE body='b'`).Scan(&nullBlob); err != nil || nullBlob != nil {
		t.Fatalf("queue b blob = %v err=%v", nullBlob, err)
	}
	var note string
	if err := db.QueryRowContext(ctx, `SELECT note FROM storetest_settings WHERE id=1`).Scan(&note); err != nil || note != "changed" {
		t.Fatalf("settings note = %q err=%v", note, err)
	}
	// Generated ids continue after the restored ones.
	var id int64
	if err := db.QueryRowContext(ctx, `INSERT INTO storetest_links(label) VALUES('new') RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if id <= 9 {
		t.Fatalf("new id %d collides with restored ids", id)
	}
}

func roundTrip(t *testing.T, src, dst *sql.DB) {
	t.Helper()
	ctx := context.Background()
	seedLogical(t, src)
	var buf bytes.Buffer
	if err := ExportLogical(ctx, src, &buf); err != nil {
		t.Fatal(err)
	}
	h, err := ValidateLogical(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if h.Dialect != DialectOf(src) || h.Migrations["storetest"] != 2 {
		t.Fatalf("header = %+v", h)
	}
	if err := ImportLogical(ctx, dst, bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatal(err)
	}
	checkLogical(t, dst)
	// A second import is refused: the target now holds data.
	if err := ImportLogical(ctx, dst, bytes.NewReader(buf.Bytes())); !errors.Is(err, ErrNotEmpty) {
		t.Fatalf("second import err = %v", err)
	}
}

func openSQLiteMemory(t *testing.T) *sql.DB {
	t.Helper()
	db, err := openSQLite(context.Background(), ":memory:", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestLogicalRoundTripSQLite(t *testing.T) {
	roundTrip(t, openSQLiteMemory(t), openSQLiteMemory(t))
}

func TestLogicalRoundTripTestBackend(t *testing.T) {
	if os.Getenv(TestPostgresEnv) == "" {
		t.Skip("set " + TestPostgresEnv + " to run PostgreSQL tests")
	}
	roundTrip(t, OpenTest(t), OpenTest(t))
}

func TestLogicalSQLiteToPostgres(t *testing.T) {
	pgURL(t)
	roundTrip(t, openSQLiteMemory(t), OpenTest(t))
}

func TestLogicalPostgresToSQLite(t *testing.T) {
	pgURL(t)
	roundTrip(t, OpenTest(t), openSQLiteMemory(t))
}

func TestLogicalRejectsDamage(t *testing.T) {
	ctx := context.Background()
	src := openSQLiteMemory(t)
	seedLogical(t, src)
	var buf bytes.Buffer
	if err := ExportLogical(ctx, src, &buf); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	truncated := strings.Join(lines[:len(lines)-1], "\n")
	if _, err := ValidateLogical(strings.NewReader(truncated)); err == nil {
		t.Fatal("truncated dump validated")
	}
	if err := ImportLogical(ctx, openSQLiteMemory(t), strings.NewReader(truncated)); err == nil {
		t.Fatal("truncated dump imported")
	}
	dropped := strings.Join(append(append([]string{}, lines[:2]...), lines[3:]...), "\n")
	if _, err := ValidateLogical(strings.NewReader(dropped)); err == nil {
		t.Fatal("dump with a missing row validated")
	}
	if _, err := ValidateLogical(strings.NewReader(`{"hello":1}`)); err == nil {
		t.Fatal("foreign JSON validated")
	}
	future := strings.Replace(buf.String(), `"storetest":2`, `"storetest":99`, 1)
	if err := ImportLogical(ctx, openSQLiteMemory(t), strings.NewReader(future)); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("future schema import err = %v", err)
	}
	// Nothing was written by the failed imports.
	dst := openSQLiteMemory(t)
	_ = ImportLogical(ctx, dst, strings.NewReader(truncated))
	var n int
	if err := dst.QueryRowContext(ctx, `SELECT COUNT(*) FROM storetest_links`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("failed import left %d rows (err=%v)", n, err)
	}
}

// A full-text index is derived data: its virtual table and shadow tables are
// left out of a dump and rebuilt by their triggers on restore.
func TestLogicalSkipsVirtualTables(t *testing.T) {
	ctx := context.Background()
	db, err := OpenPath(ctx, filepath.Join(t.TempDir(), "fts.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `CREATE TABLE lg_docs (id INTEGER PRIMARY KEY, body TEXT);
CREATE TABLE catalog_fts_bulk (id INTEGER PRIMARY KEY);
CREATE VIRTUAL TABLE lg_docs_fts USING fts5(body, content='lg_docs', content_rowid='id', tokenize='trigram');`); err != nil {
		t.Fatal(err)
	}
	tables, err := listTables(ctx, db, SQLite)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range tables {
		if strings.HasPrefix(name, "lg_docs_fts") || name == "catalog_fts_bulk" {
			t.Fatalf("dump lists derived table %s (all: %v)", name, tables)
		}
	}
	found := false
	for _, name := range tables {
		found = found || name == "lg_docs"
	}
	if !found {
		t.Fatalf("dump misses lg_docs: %v", tables)
	}
}

func init() {
	// A schema history for TestLogicalRestoresAcrossDropsAndRenames: a
	// renamed column, a dropped column and a dropped table.
	Register("storetest_history", 1, `
CREATE TABLE storetest_hist(id INTEGER PRIMARY KEY, nick TEXT NOT NULL DEFAULT '', dropped INTEGER);
ALTER TABLE storetest_hist RENAME COLUMN nick TO alias;
ALTER TABLE storetest_hist DROP COLUMN dropped;
CREATE TABLE storetest_gone(id INTEGER);
DROP TABLE storetest_gone;`)
}

// A backup taken before migrations dropped a table, dropped a column and
// renamed one still restores: dropped data is skipped, the renamed column
// loads under its new name. A column the server never had is refused.
func TestLogicalRestoresAcrossDropsAndRenames(t *testing.T) {
	ctx := context.Background()
	old := func(extra string) []byte {
		t.Helper()
		src := OpenTest(t)
		seedLogical(t, src)
		for _, q := range []string{
			`ALTER TABLE storetest_hist RENAME COLUMN alias TO nick`,
			`ALTER TABLE storetest_hist ADD COLUMN dropped INTEGER`,
			`CREATE TABLE storetest_gone(id INTEGER)`,
			`INSERT INTO storetest_gone(id) VALUES(1)`,
			`INSERT INTO storetest_hist(id, nick, dropped) VALUES(7, 'seven', 3)`,
			extra,
		} {
			if q == "" {
				continue
			}
			if _, err := src.ExecContext(ctx, q); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
		var buf bytes.Buffer
		if err := ExportLogical(ctx, src, &buf); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	dst := OpenTest(t)
	if err := ImportLogical(ctx, dst, bytes.NewReader(old(""))); err != nil {
		t.Fatal(err)
	}
	checkLogical(t, dst)
	var alias string
	if err := dst.QueryRowContext(ctx, `SELECT alias FROM storetest_hist WHERE id=7`).Scan(&alias); err != nil || alias != "seven" {
		t.Fatalf("renamed column = %q err=%v", alias, err)
	}
	if err := ImportLogical(ctx, OpenTest(t), bytes.NewReader(old(`ALTER TABLE storetest_hist ADD COLUMN never INTEGER`))); err == nil {
		t.Fatal("restored a column this server never had")
	}

	RegisterRetiredTable("storetest_retired_table")
	if err := ImportLogical(ctx, OpenTest(t), bytes.NewReader(old(`CREATE TABLE storetest_retired_table(id INTEGER); INSERT INTO storetest_retired_table(id) VALUES(42);`))); err != nil {
		t.Fatalf("failed restoring with retired table: %v", err)
	}
}
