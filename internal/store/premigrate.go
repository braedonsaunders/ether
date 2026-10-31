package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
)

// PreMigrateSuffix names the snapshot taken next to a SQLite database before
// pending migrations change its schema (ether.db.pre-migrate.bak). The
// PreMigrateKeep newest are kept: older ones are renamed .pre-migrate.bak.1,
// .2 by each upgrade that migrates. Files the caller supplies (the settings
// and credential key that match the database) are stored with each one in a
// directory named after it plus PreMigrateFilesSuffix.
//
// PostgreSQL databases get no pre-migrate snapshot; scheduled and manual
// backups (internal/backup) are their only copies.
const (
	PreMigrateSuffix      = ".pre-migrate.bak"
	PreMigrateFilesSuffix = ".files"
	PreMigrateKeep        = 3
)

// SnapshotFile is a file stored alongside a pre-migrate snapshot. Name is a
// plain file name; Data is captured by the caller before anything could
// rewrite it.
type SnapshotFile struct {
	Name string
	Data []byte
}

// preMigrateGen names generation n (0 = newest) of the snapshot at dst.
func preMigrateGen(dst string, n int) string {
	if n == 0 {
		return dst
	}
	return fmt.Sprintf("%s.%d", dst, n)
}

// pendingMigrations counts the migrations in ms a SQLite database has not
// recorded yet. A database without schema_migrations is new (or empty) and
// reports none: there is nothing in it to protect.
func pendingMigrations(ctx context.Context, db *sql.DB, ms []migration) (int, error) {
	var tables int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='schema_migrations'`).Scan(&tables); err != nil {
		return 0, err
	}
	if tables == 0 {
		return 0, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT component, version FROM schema_migrations`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	applied := map[string]bool{}
	for rows.Next() {
		var c string
		var v int
		if err := rows.Scan(&c, &v); err != nil {
			return 0, err
		}
		applied[fmt.Sprintf("%s/%d", c, v)] = true
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(applied) == 0 {
		return 0, nil
	}
	n := 0
	for _, m := range ms {
		if !applied[fmt.Sprintf("%s/%d", m.component, m.version)] {
			n++
		}
	}
	return n, nil
}

// preMigrateIncomplete marks a snapshot whose migration run has not finished
// yet. While it exists the snapshot is not retaken: after a failed migration
// the database is partly migrated (each migration commits on its own), and
// the copy from before the upgrade is the one worth keeping.
const preMigrateIncomplete = ".incomplete"

// snapshotBeforeMigrate writes a consistent copy of the SQLite database at
// path (VACUUM INTO, which reads through the WAL rather than copying the live
// file) to path+PreMigrateSuffix when an existing database has migrations
// pending, so an upgrade whose migration goes wrong can be rolled back by
// hand. files are stored next to it. Everything is written to temporary
// names first and renamed into place after older generations rotate; all of
// it is owner-only like the database. It reports whether a snapshot was
// written. Callers treat errors as non-fatal and call finishPreMigrate once
// Migrate succeeds.
func snapshotBeforeMigrate(ctx context.Context, db *sql.DB, path string, ms []migration, files []SnapshotFile, progress func(Progress)) (bool, error) {
	pending, err := pendingMigrations(ctx, db, ms)
	if err != nil || pending == 0 {
		return false, err
	}
	dst := path + PreMigrateSuffix
	if _, err := os.Stat(dst + preMigrateIncomplete); err == nil {
		if _, err := os.Stat(dst); err == nil {
			return false, nil // keep the snapshot from before the failed run
		}
	}
	for _, f := range files {
		if f.Name == "" || filepath.Base(f.Name) != f.Name || f.Name == "." || f.Name == ".." {
			return false, fmt.Errorf("snapshot file name %q", f.Name)
		}
	}
	tmp := dst + ".tmp"
	tmpFiles := dst + PreMigrateFilesSuffix + ".tmp"
	cleanup := func() { os.Remove(tmp); os.RemoveAll(tmpFiles) }
	// VACUUM INTO needs an absent or empty target; creating it empty first
	// fixes its mode (the copy holds the same credentials as ether.db).
	if err := os.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	if err := os.RemoveAll(tmpFiles); err != nil {
		return false, err
	}
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return false, err
	}
	f.Close()
	report(progress, Progress{Stage: StageSnapshot, Total: pending})
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, tmp); err != nil {
		cleanup()
		return false, fmt.Errorf("vacuum into %s: %w", tmp, err)
	}
	if len(files) > 0 {
		if err := os.Mkdir(tmpFiles, 0o700); err != nil {
			cleanup()
			return false, err
		}
		for _, sf := range files {
			if err := os.WriteFile(filepath.Join(tmpFiles, sf.Name), sf.Data, 0o600); err != nil {
				cleanup()
				return false, err
			}
		}
	}
	// Rotate: drop the oldest generation, shift the rest down by one.
	oldest := preMigrateGen(dst, PreMigrateKeep-1)
	if err := os.Remove(oldest); err != nil && !errors.Is(err, fs.ErrNotExist) {
		cleanup()
		return false, err
	}
	if err := os.RemoveAll(oldest + PreMigrateFilesSuffix); err != nil {
		cleanup()
		return false, err
	}
	for n := PreMigrateKeep - 2; n >= 0; n-- {
		from, to := preMigrateGen(dst, n), preMigrateGen(dst, n+1)
		if err := os.Rename(from, to); err != nil && !errors.Is(err, fs.ErrNotExist) {
			cleanup()
			return false, err
		}
		if err := os.Rename(from+PreMigrateFilesSuffix, to+PreMigrateFilesSuffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
			cleanup()
			return false, err
		}
	}
	if err := os.Rename(tmp, dst); err != nil {
		cleanup()
		return false, err
	}
	if len(files) > 0 {
		if err := os.Rename(tmpFiles, dst+PreMigrateFilesSuffix); err != nil {
			os.RemoveAll(tmpFiles)
			return true, err
		}
	}
	// Nothing has migrated yet, so a crash before this marker exists only
	// means the next start snapshots the same, still unmigrated database.
	if err := os.WriteFile(dst+preMigrateIncomplete, nil, 0o600); err != nil {
		return true, err
	}
	return true, nil
}

// finishPreMigrate records that migrations for the database at path
// completed, so the next upgrade takes a fresh snapshot.
func finishPreMigrate(path string) {
	_ = os.Remove(path + PreMigrateSuffix + preMigrateIncomplete)
}

// SchemaAhead lists the components this binary registers whose applied
// migrations in db go beyond the newest it knows ("auth 9 > 7"): the
// database was migrated by a newer Ether. Components this binary does not
// register (removed features, or added by a newer release) are ignored.
//
// It only informs. Migrations are additive, so an older binary keeps
// working on a newer schema; that is what lets the updater's crash-loop
// rollback restart the previous version and a cluster run mixed versions
// during a rolling update, and why Ether never refuses to start over it.
func SchemaAhead(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT component, MAX(version) FROM schema_migrations GROUP BY component ORDER BY component`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	known := KnownMigrations()
	var out []string
	for rows.Next() {
		var c string
		var v int
		if err := rows.Scan(&c, &v); err != nil {
			return nil, err
		}
		if k, ok := known[c]; ok && v > k {
			out = append(out, fmt.Sprintf("%s %d > %d", c, v, k))
		}
	}
	return out, rows.Err()
}

// warnSchemaAhead logs SchemaAhead's findings with the newest pre-migrate
// snapshot, if there is one.
func warnSchemaAhead(ctx context.Context, db *sql.DB, log *slog.Logger, snapshot string) {
	ahead, err := SchemaAhead(ctx, db)
	if err != nil || len(ahead) == 0 {
		return
	}
	attrs := []any{"components", ahead}
	if snapshot != "" {
		if _, err := os.Stat(snapshot); err == nil {
			attrs = append(attrs, "snapshot", snapshot)
		}
	}
	log.Warn("store: this database was migrated by a newer Ether; this version keeps running on it because migrations only add, "+
		"but if something misbehaves, upgrade again or restore a backup or the pre-migrate snapshot taken before that upgrade", attrs...)
}
