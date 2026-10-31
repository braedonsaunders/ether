package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenSetsJournalSizeLimitAndMaintainTruncatesWAL(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var limit int64
	if err := db.QueryRowContext(ctx, `PRAGMA journal_size_limit`).Scan(&limit); err != nil {
		t.Fatal(err)
	}
	if limit != journalSizeLimit {
		t.Fatalf("journal_size_limit = %d, want %d", limit, journalSizeLimit)
	}
	var synchronous int
	if err := db.QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&synchronous); err != nil {
		t.Fatal(err)
	}
	if synchronous != 1 {
		t.Fatalf("synchronous = %d, want NORMAL (1)", synchronous)
	}
	var auto int
	if err := db.QueryRowContext(ctx, `PRAGMA wal_autocheckpoint`).Scan(&auto); err != nil {
		t.Fatal(err)
	}
	if auto != 0 {
		t.Fatalf("wal_autocheckpoint = %d, want 0", auto)
	}

	if _, err := db.ExecContext(ctx, `CREATE TABLE store_test_t(v TEXT)`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		if _, err := db.ExecContext(ctx, `INSERT INTO store_test_t(v) VALUES (hex(randomblob(512)))`); err != nil {
			t.Fatal(err)
		}
	}
	wal := filepath.Join(dir, "ether.db-wal")
	if st, err := os.Stat(wal); err != nil || st.Size() == 0 {
		t.Fatalf("expected a non-empty WAL before maintenance: %v", err)
	}
	if err := Maintain(ctx, db); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(wal); err == nil && st.Size() != 0 {
		t.Fatalf("WAL size after Maintain = %d, want 0", st.Size())
	}
}

func TestMaintainInMemory(t *testing.T) {
	db, err := OpenMemory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := Maintain(context.Background(), db); err != nil {
		t.Fatal(err)
	}
}
