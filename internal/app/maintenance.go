package app

import (
	"context"
	"errors"
	"time"

	"github.com/braedonsaunders/ether/internal/schedule"
	"github.com/braedonsaunders/ether/internal/store"
)

// dbMaintenanceInterval is how often derived data is pruned, the SQLite WAL
// truncated, the planner statistics refreshed and the database checked for
// damage. The first pass waits
// dbMaintenanceDelay so it never competes with startup work.
const (
	dbMaintenanceInterval = 24 * time.Hour
	dbMaintenanceDelay    = 15 * time.Minute
)

// pruner deletes expired or out-of-retention derived rows for one component
// and reports how many rows it removed.
type pruner struct {
	name  string
	prune func(context.Context) (int64, error)
}

// pruners lists every component with a retention policy. User data (plays,
// watch state, history, playlists, catalogue titles) is never pruned.
func (a *App) pruners() []pruner {
	var ps []pruner
	add := func(name string, ok bool, fn func(context.Context) (int64, error)) {
		if ok {
			ps = append(ps, pruner{name, fn})
		}
	}
	add("markers", a.markers != nil, func(ctx context.Context) (int64, error) { return a.markers.Prune(ctx) })
	add("resolver", a.resolver != nil, func(ctx context.Context) (int64, error) { return a.resolver.Prune(ctx) })
	add("newznab", a.indexers != nil, func(ctx context.Context) (int64, error) { return a.indexers.Prune(ctx) })
	add("stats", a.stats != nil, func(ctx context.Context) (int64, error) { return a.stats.Prune(ctx) })
	add("ratings", a.ratings != nil, func(ctx context.Context) (int64, error) { return a.ratings.Prune(ctx) })
	add("collections", a.collections != nil, func(ctx context.Context) (int64, error) { return a.collections.Prune(ctx) })
	return ps
}

// runPruners runs every component prune, logging (not failing on) errors.
func (a *App) runPruners(ctx context.Context) {
	for _, p := range a.pruners() {
		n, err := p.prune(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			a.log.Warn("prune", "component", p.name, "err", err)
		} else if n > 0 {
			a.log.Info("pruned stale rows", "component", p.name, "rows", n)
		}
	}
}

// checkpointWAL copies the SQLite WAL from a background goroutine. A commit
// on a request must not run that checkpoint itself.
func (a *App) checkpointWAL(ctx context.Context) {
	if a.db == nil || store.DialectOf(a.db) != store.SQLite {
		return
	}
	store.RunCheckpointer(ctx, a.db, a.walTruncateAllowed, func(err error) {
		a.log.Warn("wal checkpoint", "err", err)
	})
}

// walTruncateAllowed reports whether a TRUNCATE checkpoint is safe. PASSIVE
// checkpoints still run when this is false. A live playback, or any other
// connection inside a query, keeps the exclusive truncate off the request.
func (a *App) walTruncateAllowed() bool {
	if a.playback != nil && len(a.playback.Sessions()) > 0 {
		return false
	}
	return a.db != nil && a.db.Stats().InUse == 0
}

// maintainDB prunes derived data and runs store.Maintain daily until ctx
// ends.
func (a *App) maintainDB(ctx context.Context) {
	delay := dbMaintenanceDelay
	for schedule.Wait(ctx, time.Now, delay) {
		delay = dbMaintenanceInterval
		start := time.Now()
		a.runPruners(ctx)
		err := store.Maintain(ctx, a.db)
		var corrupt *store.CorruptionError
		switch {
		case ctx.Err() != nil:
			return
		case errors.As(err, &corrupt):
			// Damage does not heal on a retry: report it and check again
			// at the next daily pass.
			a.recordIntegrity(corrupt.Integrity)
		case store.IsCorrupt(err):
			a.recordIntegrity(store.Integrity{Findings: []string{err.Error()}, Checked: time.Now().UTC()})
		case err != nil:
			a.log.Warn("database maintenance", "err", err)
			delay = dbMaintenanceDelay
		default:
			if store.DialectOf(a.db) != store.Postgres {
				a.recordIntegrity(store.Integrity{OK: true, Checked: time.Now().UTC()})
			}
			a.log.Info("database maintenance done", "took", time.Since(start))
		}
	}
}
