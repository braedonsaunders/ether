package app

import (
	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/plexmeta"
)

// Leader-only schedules in a cluster restart on every leadership handoff.
// Catalogue sync and watchlist passes therefore continue the schedule
// recorded in the shared database (the last run by any node) instead of
// running at once; a standalone server keeps syncing on start. Collection
// refresh already schedules from each collection's recorded build time.

func (a *App) catalogOptions() []catalog.Option {
	var opts []catalog.Option
	if a.opts.CatalogNow != nil {
		opts = append(opts, catalog.WithNow(a.opts.CatalogNow))
	}
	if a.clustered() {
		opts = append(opts, catalog.WithResumeSchedule())
	}
	opts = append(opts, catalog.WithSaferRatingSQL(plexmeta.RatingSQL{}))
	return opts
}
