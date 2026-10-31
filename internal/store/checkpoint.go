package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/schedule"
)

// walCheckpointInterval is how often a background PASSIVE checkpoint copies
// WAL frames into the database. Short enough that one checkpoint stays a
// small fsync, long enough that an idle server is not rewriting the file
// continuously.
const walCheckpointInterval = time.Second

// Checkpoint copies WAL frames back into the database. truncate also
// truncates the WAL, and only when this checkpoint itself found no readers
// in the way and no other connection is inside a query. TRUNCATE takes
// locks a request would have to wait on, so callers pass false while the
// server is busy.
//
// PostgreSQL checkpoints in its own background process. There is nothing
// to run on the client, and this does not issue CHECKPOINT.
func Checkpoint(ctx context.Context, db *sql.DB, truncate bool) error {
	if db == nil || DialectOf(db) == Postgres {
		return nil
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	var mode string
	if err := conn.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil {
		return err
	}
	if !strings.EqualFold(mode, "wal") {
		return nil
	}
	busy, logFrames, _, err := walCheckpoint(ctx, conn, "PASSIVE")
	if err != nil {
		return err
	}
	if !truncate || busy != 0 || logFrames == 0 || db.Stats().InUse > 1 {
		return nil
	}
	if _, _, _, err := walCheckpoint(ctx, conn, "TRUNCATE"); err != nil {
		return err
	}
	return nil
}

func walCheckpoint(ctx context.Context, conn *sql.Conn, mode string) (busy, logFrames, checkpointed int, err error) {
	var query string
	switch mode {
	case "PASSIVE":
		query = `PRAGMA wal_checkpoint(PASSIVE)`
	case "TRUNCATE":
		query = `PRAGMA wal_checkpoint(TRUNCATE)`
	default:
		return 0, 0, 0, fmt.Errorf("wal checkpoint %s: unknown mode", mode)
	}
	err = conn.QueryRowContext(ctx, query).Scan(&busy, &logFrames, &checkpointed)
	if err != nil {
		err = fmt.Errorf("wal checkpoint %s: %w", mode, err)
	}
	return busy, logFrames, checkpointed, err
}

// RunCheckpointer PASSIVE-checkpoints until ctx ends. truncate, when it
// reports true, allows a TRUNCATE on that pass; a nil truncate never
// truncates. onErr receives checkpoint failures and may be nil.
func RunCheckpointer(ctx context.Context, db *sql.DB, truncate func() bool, onErr func(error)) {
	if db == nil || DialectOf(db) == Postgres {
		return
	}
	for schedule.Wait(ctx, time.Now, walCheckpointInterval) {
		allow := truncate != nil && truncate()
		err := Checkpoint(ctx, db, allow)
		if err != nil && ctx.Err() == nil && onErr != nil {
			onErr(err)
		}
	}
}
