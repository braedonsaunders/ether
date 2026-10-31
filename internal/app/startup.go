package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/backup"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/startup"
	"github.com/braedonsaunders/ether/internal/store"
)

// startError tags a failed start with what failed, for the status page.
type startError struct {
	code string
	err  error
}

func (e *startError) Error() string { return e.err.Error() }
func (e *startError) Unwrap() error { return e.err }

func startFail(code string, err error) error { return &startError{code: code, err: err} }

// failureOf describes a failed start for the status page. standalone says
// whether the page may restore a backup: only on a standalone server,
// whose database is a file in its data directory, and not while a restore
// that could not be undone holds the data directory.
func failureOf(err error, standalone bool) startup.Failure {
	f := startup.Failure{Code: startup.FailUnknown, Detail: err.Error()}
	var se *startError
	if errors.As(err, &se) {
		f.Code = se.code
	}
	f.Recoverable = standalone && f.Code != startup.FailRestore
	return f
}

// openFailure classifies a database that would not open.
func openFailure(err error, postgres bool) string {
	switch {
	case postgres && store.IsUnavailable(err):
		return startup.FailDatabaseUnreachable
	case postgres && !isMigrationError(err):
		return startup.FailDatabaseRejected
	case store.IsCorrupt(err):
		return startup.FailDatabaseCorrupt
	case isMigrationError(err):
		return startup.FailMigration
	default:
		return startup.FailUnknown
	}
}

// isMigrationError matches store's "migration component/version: ..." error.
func isMigrationError(err error) bool {
	for e := err; e != nil; e = errors.Unwrap(e) {
		if strings.HasPrefix(e.Error(), "migration ") {
			return true
		}
	}
	return false
}

// trackedDatabase names the database for the status page: the host and
// port of a PostgreSQL server, never its credentials.
func trackedDatabase(db config.Database) startup.Database {
	if !db.IsPostgres() {
		return startup.Database{Kind: "sqlite"}
	}
	out := startup.Database{Kind: "postgres"}
	if u, err := url.Parse(db.URL); err == nil && u.Hostname() != "" {
		out.Host = u.Host
		if u.Port() == "" {
			out.Host = net.JoinHostPort(u.Hostname(), "5432")
		}
	}
	return out
}

// errRestoreRolledBack marks a failed start after which the original data
// was put back: Serve starts again at once, on the original data.
var errRestoreRolledBack = errors.New("the restored backup could not start, so the original data was put back")

// openFrontDoor binds Serve's listener the first time a start gets here,
// from the settings as they are on disk (opening them may rewrite them,
// and a pending restore is about to replace them). Unreadable settings
// bind loopback only.
func (a *App) openFrontDoor() error {
	f := a.opts.front
	if f.bound() {
		return nil
	}
	server := readNodeFacts(a.opts.DataDir, os.Getenv).Server
	port, err := envPort()
	if err != nil {
		return err
	}
	if port != 0 {
		server.Port = port
	}
	if err := f.bind(server, port != 0); err != nil {
		return err
	}
	a.boundNow = true
	a.log.Info("Ether is starting; the console shows its progress", "ui", fmt.Sprintf("http://localhost:%d/ether/", f.port))
	return nil
}

// keepListeningPort saves the port the front door listens on when the
// settings name another, but only where the settings are behind the
// listener: it was just bound (and moved off a busy default port), or a
// restore brought settings from before a port change. A listener kept from
// an earlier start (a cluster transition) leaves a port the owner has just
// saved for the next restart alone.
func (a *App) keepListeningPort() error {
	f := a.opts.front
	if f == nil || !f.bound() || a.cfg.Get().Server.Port == f.port {
		return nil
	}
	if !a.boundNow && a.restored == nil {
		return nil
	}
	if f.moved && a.boundNow {
		return a.saveMovedPort(f.port)
	}
	return a.cfg.Update(func(c *config.Config) error { c.Server.Port = f.port; return nil })
}

// applyPendingRestore swaps in a restore requested before the restart, or
// puts the original files back after one that never got its server ready.
// A node that shares a PostgreSQL database never applies one: the backup's
// settings would point it at the shared database.
func (a *App) applyPendingRestore() error {
	dir := a.opts.DataDir
	p, err := backup.ReadPending(dir)
	if err == nil && p == nil {
		backup.CleanStaged(dir, 24*time.Hour)
		return nil
	}
	if p != nil && p.Rollback == "" && !readNodeFacts(dir, os.Getenv).Standalone() {
		_ = backup.CancelPending(dir)
		a.log.Error("restore not applied: a restore replaces the database of a standalone server only")
		return nil
	}
	if p != nil && p.Rollback == "" {
		a.tracker.Restoring(p.Source)
		a.log.Info("restoring a backup", "source", p.Source, "kind", p.Kind)
	}
	rep, err := backup.ApplyPending(dir, time.Now)
	if err != nil {
		return startFail(startup.FailRestore, err)
	}
	a.restored = rep
	switch {
	case rep == nil:
	case rep.Status == "applied":
	case rep.RolledBack:
		a.log.Error("a restored backup could not start; the original data was put back", "err", rep.Error)
	default:
		a.log.Error("restore failed; the original data was kept", "err", rep.Error)
	}
	backup.CleanStaged(dir, 24*time.Hour)
	return nil
}

// restoreApplied reports whether this start runs on a restore it applied
// and has not confirmed yet.
func (a *App) restoreApplied() bool {
	return a.restored != nil && a.restored.Status == "applied"
}

// confirmRestore records that the server is about to serve on the
// restored backup: the report says "restored" from here on. An error
// means the restore was not committed; the server must not serve on it
// (Serve then rolls it back).
func (a *App) confirmRestore() error {
	if !a.restoreApplied() {
		return nil
	}
	if err := backup.ConfirmRestore(a.opts.DataDir, a.restored); err != nil {
		if p, _ := backup.ReadPending(a.opts.DataDir); p == nil {
			// Committed; only the report could not be written.
			a.restored.Status = "restored"
			a.log.Warn("restore finished, but its report could not be saved", "err", err)
			return nil
		}
		return startFail(startup.FailRestore, fmt.Errorf("could not finish the restore: %w", err))
	}
	a.restored.Status = "restored"
	a.log.Info("restore finished", "source", a.restored.Source, "rollback", a.restored.RollbackPath)
	return nil
}

// rollbackUnconfirmed puts the original files back when a start on a
// restore it applied fails before the server is ready (the settings and
// database must be closed). It reports whether it did.
func (a *App) rollbackUnconfirmed(cause error) bool {
	if !a.restoreApplied() {
		return false
	}
	reason := "the server stopped before it was ready"
	if cause != nil {
		reason = cause.Error()
	}
	rep, err := backup.RollbackApplied(a.opts.DataDir, time.Now, reason)
	if err != nil {
		a.log.Error("a restored backup could not start and the original data could not be put back", "err", err)
		return false
	}
	a.restored = rep
	a.log.Error("the restored backup could not start; the original data was put back", "err", reason)
	return true
}

// storeProgress shows the database opening on the status page.
func (a *App) storeProgress(p store.Progress) {
	switch p.Stage {
	case store.StageSnapshot:
		a.tracker.Set(startup.PhaseBackingUp)
	case store.StageMigrate:
		a.tracker.Set(startup.PhaseMigrating)
		a.tracker.Progress(p.Done+1, p.Total)
	}
}

// checkRestoredIntegrity checks a database restored at this start before
// anything uses it, for the restore report. Other starts check later, off
// the startup path (checkIntegrityLater): a full read of a large database
// takes seconds.
func (a *App) checkRestoredIntegrity(ctx context.Context) error {
	if !a.restoreApplied() || store.DialectOf(a.db) == store.Postgres {
		return nil
	}
	a.tracker.Set(startup.PhaseCheckingIntegrity)
	res, err := store.QuickCheck(ctx, a.db)
	if err != nil {
		a.log.Warn("database integrity check could not run", "err", err)
		return nil
	}
	a.recordIntegrity(res)
	a.restored.Integrity = &res
	if !res.OK {
		return startFail(startup.FailRestore, fmt.Errorf("the restored database is damaged: %s", strings.Join(res.Findings, "; ")))
	}
	return nil
}

// importRestoredDump imports a restored PostgreSQL-style dump.
func (a *App) importRestoredDump(ctx context.Context) error {
	if _, err := os.Stat(filepath.Join(a.opts.DataDir, backup.LogicalDumpFile)); err != nil {
		return nil
	}
	a.tracker.Set(startup.PhaseImporting)
	imported, err := backup.ImportPending(ctx, a.db, a.opts.DataDir)
	if err != nil {
		return startFail(startup.FailRestore, fmt.Errorf("database: %w", err))
	}
	if imported {
		a.log.Info("database: imported restored backup")
		if a.restoreApplied() {
			a.restored.Imported = true
		}
	}
	return nil
}

// integrityDelay is how long after start the routine check waits, so it
// does not compete with startup work.
const integrityDelay = 2 * time.Minute

// checkIntegrityLater runs the start's integrity check once the server has
// settled (SQLite only; a restored database was checked already).
func (a *App) checkIntegrityLater(ctx context.Context) {
	if store.DialectOf(a.db) == store.Postgres || a.integrity.Load() != nil {
		return
	}
	t := time.NewTimer(integrityDelay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return
	case <-t.C:
	}
	res, err := store.QuickCheck(ctx, a.db)
	if err != nil {
		if ctx.Err() == nil {
			a.log.Warn("database integrity check could not run", "err", err)
		}
		return
	}
	a.recordIntegrity(res)
}

// recordIntegrity keeps a check's result for the console and logs damage
// once, with what to do about it.
func (a *App) recordIntegrity(res store.Integrity) {
	a.integrity.Store(&res)
	if res.OK {
		a.log.Info("database integrity check passed", "took", time.Duration(res.Took*float64(time.Second)).Round(time.Millisecond))
		return
	}
	a.log.Error("database integrity check failed: the database file is damaged. Ether keeps running so you can restore a backup from Maintenance → Backups",
		"findings", res.Findings)
}
