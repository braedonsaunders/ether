package state

import (
	"context"
	"database/sql"

	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.RegisterViewerTable("state", "state_history_events", "user_id", store.ViewerMergeUnion)
	store.RegisterViewerTable("state", "state_lifetime_events", "user_id", store.ViewerMergeUnion)
	store.RegisterViewerPurge("state", []string{"state_history_rank"}, func(ctx context.Context, tx *sql.Tx, uid int64) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM state_history_rank WHERE user_id=?`, uid)
		return err
	})
	store.RegisterViewerMerge("state", []string{"state_history_rank"}, func(ctx context.Context, tx *sql.Tx, from, into int64) error {
		// Global rows already count both accounts; only per-account rows move.
		_, err := tx.ExecContext(ctx, `INSERT INTO state_history_rank SELECT rating_key,?,kind,history_views,lifetime_views,last_history,last_lifetime FROM state_history_rank WHERE user_id=? ON CONFLICT(rating_key,user_id) DO UPDATE SET history_views=state_history_rank.history_views+excluded.history_views,lifetime_views=state_history_rank.lifetime_views+excluded.lifetime_views,last_history=CASE WHEN state_history_rank.last_history>excluded.last_history THEN state_history_rank.last_history ELSE excluded.last_history END,last_lifetime=CASE WHEN state_history_rank.last_lifetime>excluded.last_lifetime THEN state_history_rank.last_lifetime ELSE excluded.last_lifetime END`, into, from)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM state_history_rank WHERE user_id=?`, from)
		return err
	})
}
