package watchlist

import (
	"context"
	"database/sql"
	"strconv"

	"github.com/braedonsaunders/ether/internal/store"
)

// Per-viewer lifecycle for this component's tables: how a viewer's rows
// are purged on removal and moved on merge. See store.RegisterViewerTable.
func init() {
	store.RegisterViewerTable("watchlist", "watchlist_items", "user_id", store.ViewerMergeMove, "item_key", "source")
	store.RegisterViewerTable("watchlist", "watchlist_episodes", "user_id", store.ViewerMergeMove, "item_key", "episode_key")
	store.RegisterViewerTable("watchlist", "watchlist_community_jobs", "user_id", store.ViewerMergeDrop)
	// Community locks use text keys because the writer also accepts external IDs.
	dropLock := func(ctx context.Context, tx *sql.Tx, viewerID int64) error {
		_, err := tx.ExecContext(ctx, "DELETE FROM watchlist_community_locks WHERE user_id=?", strconv.FormatInt(viewerID, 10))
		return err
	}
	store.RegisterViewerPurge("watchlist", []string{"watchlist_community_locks"}, dropLock)
	store.RegisterViewerMerge("watchlist", []string{"watchlist_community_locks"}, func(ctx context.Context, tx *sql.Tx, fromID, _ int64) error {
		return dropLock(ctx, tx, fromID)
	})
	store.RegisterViewerTable("watchlist", "watchlist_availability_jobs", "user_id", store.ViewerMergeDrop)
	store.RegisterViewerTable("watchlist", "watchlist_warm_intents", "user_id", store.ViewerMergeDrop)
}
