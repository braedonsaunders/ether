package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestRegisterSQLiteApplyRefusesUnregisteredVersion(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("RegisterSQLiteApply created a version")
		}
	}()
	RegisterSQLiteApply("sqlite_apply_missing", 1, func(context.Context, *sql.Tx) error { return nil })
}

func TestSQLiteApplyIsSQLiteOnlyAndKeepsVersionBookkeeping(t *testing.T) {
	const component = "applyprobe"
	regMu.Lock()
	orig := append([]migration(nil), migrations...)
	regMu.Unlock()
	t.Cleanup(func() {
		regMu.Lock()
		migrations = orig
		regMu.Unlock()
	})

	var calls int
	Register(component, 1, `this sqlite text must not run`)
	RegisterDialect(component, 1, Postgres, `CREATE TABLE applyprobe_pg(id BIGINT);`)
	RegisterSQLiteApply(component, 1, func(ctx context.Context, tx *sql.Tx) error {
		calls++
		_, err := tx.ExecContext(ctx, `CREATE TABLE applyprobe(id INTEGER PRIMARY KEY); INSERT INTO applyprobe(id) VALUES (1)`)
		return err
	})

	regMu.Lock()
	var probe *migration
	var storetestNil = true
	for i := range migrations {
		m := &migrations[i]
		if m.component == component && m.version == 1 {
			probe = m
		}
		if m.component == "storetest" && m.sqliteApply != nil {
			storetestNil = false
		}
	}
	regMu.Unlock()
	if probe == nil {
		t.Fatal("probe migration missing")
	}
	if probe.sqlFor(Postgres) != `CREATE TABLE applyprobe_pg(id BIGINT);` {
		t.Fatalf("postgres sql %q", probe.sqlFor(Postgres))
	}
	if probe.sqlFor(SQLite) != `this sqlite text must not run` {
		t.Fatalf("sqlite sql changed to %q", probe.sqlFor(SQLite))
	}
	if !storetestNil {
		t.Fatal("sqliteApply attached to a migration that did not request it")
	}

	ctx := context.Background()
	dir := t.TempDir()
	db, err := openSQLite(ctx, filepath.Join(dir, "ether.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("sqlite apply calls %d", calls)
	}
	var n, version int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM applyprobe`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("probe rows %d (%v)", n, err)
	}
	if err := db.QueryRowContext(ctx, `SELECT version FROM schema_migrations WHERE component=?`, component).Scan(&version); err != nil || version != 1 {
		t.Fatalf("recorded version %d (%v)", version, err)
	}
	db.Close()
	db, err = openSQLite(ctx, filepath.Join(dir, "ether.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	if calls != 1 {
		t.Fatalf("applied version called apply again (%d)", calls)
	}

	Register(component, 2, `CREATE TABLE applyprobe_skipped(id INTEGER);`)
	RegisterSQLiteApply(component, 2, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `CREATE TABLE applyprobe_v2(id INTEGER)`); err != nil {
			return err
		}
		return errors.New("probe rollback")
	})
	failed := filepath.Join(t.TempDir(), "ether.db")
	db, err = openSQLite(ctx, failed, nil)
	if err == nil {
		db.Close()
		t.Fatal("failing apply was recorded")
	}
	if !strings.Contains(err.Error(), component+"/2") || !strings.Contains(err.Error(), "probe rollback") {
		t.Fatalf("error %v", err)
	}
	raw, err := sql.Open("sqlite", "file:"+failed+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if err := raw.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM schema_migrations WHERE component=?`, component).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 1 {
		t.Fatalf("failed version recorded as %d", version)
	}
	var tables int
	if err := raw.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE name='applyprobe_v2'`).Scan(&tables); err != nil || tables != 0 {
		t.Fatalf("rolled-back table count %d (%v)", tables, err)
	}
	if err := raw.QueryRowContext(ctx, `SELECT COUNT(*) FROM applyprobe`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("committed probe rows %d (%v)", n, err)
	}
}
