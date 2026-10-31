package store

import (
	"context"
	"database/sql"
	"fmt"
)

// journalSizeLimit bounds the WAL file kept on disk after checkpoints.
const journalSizeLimit = 64 << 20

// Maintain is the periodic (daily) upkeep for a long-running database: it
// checkpoints the WAL and truncates it when no request is using the database,
// then refreshes query-planner statistics. It deliberately never VACUUMs;
// that rewrites the whole file and is left to the owner.
//
// On PostgreSQL the server manages its own WAL and autovacuum; Maintain only
// refreshes planner statistics with ANALYZE.
func Maintain(ctx context.Context, db *sql.DB) error {
	if DialectOf(db) == Postgres {
		if _, err := db.ExecContext(ctx, `ANALYZE`); err != nil {
			return fmt.Errorf("analyze: %w", err)
		}
		return nil
	}
	// Truncate only when the checkpoint finds the database idle. A request
	// must not wait on TRUNCATE; the background checkpointer is the same gate.
	if err := Checkpoint(ctx, db, true); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `PRAGMA optimize`); err != nil {
		return fmt.Errorf("optimize: %w", err)
	}
	res, err := QuickCheck(ctx, db)
	if err != nil {
		return fmt.Errorf("integrity check: %w", err)
	}
	if !res.OK {
		return &CorruptionError{Integrity: res}
	}
	return nil
}
