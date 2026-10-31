package stats

import "github.com/braedonsaunders/ether/internal/store"

func init() {
	store.Register("stats", 7, `ALTER TABLE stats_provider_days ADD COLUMN spillovers INTEGER NOT NULL DEFAULT 0;`)
	store.Register("stats", 4, `ALTER TABLE stats_provider_days ADD COLUMN body_misses INTEGER NOT NULL DEFAULT 0;ALTER TABLE stats_provider_days ADD COLUMN body_takedowns INTEGER NOT NULL DEFAULT 0;`)
	store.Register("stats", 3, `ALTER TABLE stats_provider_days ADD COLUMN backfill_saves INTEGER NOT NULL DEFAULT 0;`)
}
