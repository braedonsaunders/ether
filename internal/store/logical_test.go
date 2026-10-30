package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
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
	db, err := openSQLite(context.Background(), ":memory:", nil)
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
