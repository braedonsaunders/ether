package store

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSnapshotBeforeMigrate(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "ether.db")
	snap := path + PreMigrateSuffix

	// A brand-new database has nothing to protect.
	fresh, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := snapshotBeforeMigrate(ctx, fresh, path, registered(), nil, nil); ok || err != nil {
		t.Fatalf("fresh database: ok=%v err=%v", ok, err)
	}
	fresh.Close()

	db, err := OpenPath(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := os.Stat(snap); !os.IsNotExist(err) {
		t.Fatalf("snapshot written for a fresh database: %v", err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE premig_t (v TEXT); INSERT INTO premig_t VALUES ('before')`); err != nil {
		t.Fatal(err)
	}

	// Fully migrated: no snapshot.
	if ok, err := snapshotBeforeMigrate(ctx, db, path, registered(), nil, nil); ok || err != nil {
		t.Fatalf("nothing pending: ok=%v err=%v", ok, err)
	}

	// A newer binary with one more migration snapshots the old state.
	next := append(registered(), migration{component: "premig", version: 1, sql: `SELECT 1`})
	if ok, err := snapshotBeforeMigrate(ctx, db, path, next, nil, nil); !ok || err != nil {
		t.Fatalf("pending migration: ok=%v err=%v", ok, err)
	}
	if v := snapshotValue(t, snap); v != "before" {
		t.Fatalf("snapshot holds %q", v)
	}
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(snap); err != nil || fi.Mode().Perm()&0o077 != 0 {
			t.Fatalf("snapshot mode %v err=%v, want owner-only", fi.Mode(), err)
		}
	}

	// The migration run failed (marker still present): a retry must keep
	// the snapshot from before the upgrade.
	if _, err := db.ExecContext(ctx, `UPDATE premig_t SET v='partly migrated'`); err != nil {
		t.Fatal(err)
	}
	if ok, err := snapshotBeforeMigrate(ctx, db, path, next, nil, nil); ok || err != nil {
		t.Fatalf("retry after failure: ok=%v err=%v", ok, err)
	}
	if v := snapshotValue(t, snap); v != "before" {
		t.Fatalf("snapshot replaced after a failed run: %q", v)
	}

	// Once a run completes, the next upgrade replaces the snapshot; only
	// one is kept.
	finishPreMigrate(path)
	if ok, err := snapshotBeforeMigrate(ctx, db, path, next, nil, nil); !ok || err != nil {
		t.Fatalf("next upgrade: ok=%v err=%v", ok, err)
	}
	if v := snapshotValue(t, snap); v != "partly migrated" {
		t.Fatalf("snapshot not refreshed: %q", v)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Fatalf("temporary file left behind: %s", e.Name())
		}
	}
}

func TestOpenRemovesIncompleteMarkerAfterMigrating(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ether.db")
	marker := path + PreMigrateSuffix + preMigrateIncomplete
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := OpenPath(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("marker survived a successful migration: %v", err)
	}
}

func snapshotValue(t *testing.T, path string) string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var v string
	if err := db.QueryRow(`SELECT v FROM premig_t`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// Each upgrade that migrates takes a new snapshot set (database plus the
// files the caller captured); the PreMigrateKeep newest are kept.
func TestPreMigrateSnapshotsRotateWithTheirFiles(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "ether.db")
	db, err := OpenPath(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `CREATE TABLE premig_t (v TEXT); INSERT INTO premig_t VALUES ('v0')`); err != nil {
		t.Fatal(err)
	}
	ms := registered()
	for i := 1; i <= PreMigrateKeep+1; i++ {
		ms = append(ms, migration{component: "premig", version: i, sql: `SELECT 1`})
		files := []SnapshotFile{{Name: "config.json", Data: []byte(fmt.Sprintf(`{"gen":%d}`, i))}, {Name: ".credentials-key", Data: []byte("key")}}
		if ok, err := snapshotBeforeMigrate(ctx, db, path, ms, files, nil); !ok || err != nil {
			t.Fatalf("upgrade %d: ok=%v err=%v", i, ok, err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO schema_migrations(component,version) VALUES('premig',?)`, i); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `UPDATE premig_t SET v=?`, fmt.Sprintf("v%d", i)); err != nil {
			t.Fatal(err)
		}
		finishPreMigrate(path)
	}
	dst := path + PreMigrateSuffix
	for n := 0; n < PreMigrateKeep; n++ {
		gen := preMigrateGen(dst, n)
		upgrade := PreMigrateKeep + 1 - n // newest first
		if v := snapshotValue(t, gen); v != fmt.Sprintf("v%d", upgrade-1) {
			t.Fatalf("generation %d holds %q", n, v)
		}
		cfg, err := os.ReadFile(filepath.Join(gen+PreMigrateFilesSuffix, "config.json"))
		if err != nil || string(cfg) != fmt.Sprintf(`{"gen":%d}`, upgrade) {
			t.Fatalf("generation %d config %q %v", n, cfg, err)
		}
		if runtime.GOOS != "windows" {
			for _, p := range []string{gen, filepath.Join(gen+PreMigrateFilesSuffix, ".credentials-key")} {
				if fi, err := os.Stat(p); err != nil || fi.Mode().Perm()&0o077 != 0 {
					t.Fatalf("%s mode %v err=%v, want owner-only", p, fi.Mode(), err)
				}
			}
			if fi, err := os.Stat(gen + PreMigrateFilesSuffix); err != nil || fi.Mode().Perm() != 0o700 {
				t.Fatalf("files dir mode %v err=%v", fi.Mode(), err)
			}
		}
	}
	if _, err := os.Stat(preMigrateGen(dst, PreMigrateKeep)); !os.IsNotExist(err) {
		t.Fatalf("kept more than %d snapshots: %v", PreMigrateKeep, err)
	}
	if _, err := os.Stat(preMigrateGen(dst, PreMigrateKeep) + PreMigrateFilesSuffix); !os.IsNotExist(err) {
		t.Fatalf("kept more than %d file sets: %v", PreMigrateKeep, err)
	}
	if _, err := snapshotBeforeMigrate(ctx, db, path, append(ms, migration{component: "premig", version: 99, sql: `SELECT 1`}), []SnapshotFile{{Name: "../escape"}}, nil); err == nil {
		t.Fatal("snapshot file name with a path accepted")
	}
}

// A database migrated by a newer binary is reported, never refused, and
// components this binary does not know are ignored.
func TestSchemaAheadWarnsWithoutRefusing(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ether.db")
	db, err := OpenPath(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	known := KnownMigrations()
	var comp string
	for c := range known {
		comp = c
		break
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO schema_migrations(component,version) VALUES(?,?),('removed_feature',7)`, comp, known[comp]+2); err != nil {
		t.Fatal(err)
	}
	ahead, err := SchemaAhead(ctx, db)
	if err != nil || len(ahead) != 1 || ahead[0] != fmt.Sprintf("%s %d > %d", comp, known[comp]+2, known[comp]) {
		t.Fatalf("ahead %v %v", ahead, err)
	}
	db.Close()

	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(prev)
	again, err := OpenPath(ctx, path)
	if err != nil {
		t.Fatalf("refused a newer schema: %v", err)
	}
	again.Close()
	if out := logs.String(); !strings.Contains(out, "migrated by a newer Ether") || !strings.Contains(out, comp) || strings.Contains(out, "removed_feature") {
		t.Fatalf("log:\n%s", out)
	}
}
