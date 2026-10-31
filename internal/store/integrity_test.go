package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestQuickCheckHealthyDatabase(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	res, err := QuickCheck(ctx, db)
	if err != nil || !res.OK || len(res.Findings) != 0 || res.Checked.IsZero() {
		t.Fatalf("QuickCheck = %+v, %v; want ok", res, err)
	}
	if err := Maintain(ctx, db); err != nil {
		t.Fatalf("Maintain on a healthy database: %v", err)
	}
}

// corruptDatabase writes a database with enough pages that overwriting the
// middle of the file damages b-tree pages rather than the header.
func corruptDatabase(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE store_damage(id INTEGER PRIMARY KEY, v TEXT); CREATE INDEX store_damage_v ON store_damage(v)`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 400; i++ {
		if _, err := db.ExecContext(ctx, `INSERT INTO store_damage(v) VALUES (hex(randomblob(400)))`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	path := filepath.Join(dir, "ether.db")
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := f.Stat()
	junk := make([]byte, 4096*6)
	for i := range junk {
		junk[i] = 0xA5
	}
	if _, err := f.WriteAt(junk, st.Size()/2); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return path
}

func TestQuickCheckReportsDamageWithoutFailing(t *testing.T) {
	ctx := context.Background()
	path := corruptDatabase(t)
	db, err := OpenPath(ctx, path)
	if err != nil {
		t.Fatalf("open damaged database: %v", err)
	}
	defer db.Close()
	res, err := QuickCheck(ctx, db)
	if err != nil {
		t.Fatalf("QuickCheck errored instead of reporting findings: %v", err)
	}
	if res.OK || len(res.Findings) == 0 || len(res.Findings) > maxIntegrityFindings {
		t.Fatalf("QuickCheck = %+v; want bounded findings", res)
	}
	err = Maintain(ctx, db)
	var ce *CorruptionError
	if !errors.As(err, &ce) && !IsCorrupt(err) {
		t.Fatalf("Maintain = %v; want a corruption error", err)
	}
}

func TestQuickCheckSkippedOnClosedDatabaseIsAnError(t *testing.T) {
	db, err := OpenMemory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := QuickCheck(context.Background(), db); err == nil {
		t.Fatal("QuickCheck on a closed database: want an error, not a finding")
	}
}

func TestIsCorrupt(t *testing.T) {
	for msg, want := range map[string]bool{
		"database disk image is malformed (11)": true,
		"file is not a database (26)":           true,
		"connection refused":                    false,
	} {
		if got := IsCorrupt(errors.New(msg)); got != want {
			t.Errorf("IsCorrupt(%q) = %v", msg, got)
		}
	}
	if IsCorrupt(nil) {
		t.Error("IsCorrupt(nil)")
	}
}

func TestOpenReportsMigrationProgress(t *testing.T) {
	ctx := context.Background()
	var seen []Progress
	db, _, err := OpenWith(ctx, Options{DataDir: t.TempDir(), Progress: func(p Progress) { seen = append(seen, p) }})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if len(seen) == 0 {
		t.Fatal("no progress reported while migrating a new database")
	}
	total := seen[0].Total
	for i, p := range seen {
		if p.Stage != StageMigrate || p.Total != total || p.Done != i {
			t.Fatalf("progress %d = %+v; want migrate %d/%d", i, p, i, total)
		}
	}
	if len(seen) != total {
		t.Fatalf("reported %d steps for %d migrations", len(seen), total)
	}
}
