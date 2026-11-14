package watchlist

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/braedonsaunders/ether/internal/core"
)

// ImportTitleTx adds a reviewed title to the live viewer watchlist in the
// caller's transaction, so import bookkeeping and all list writes are atomic.
func ImportTitleTx(ctx context.Context, tx *sql.Tx, userID int64, title core.Title, added int64) error {
	if userID <= 0 || !title.Key.Valid() || title.Title == "" {
		return errors.New("invalid imported watchlist item")
	}
	raw, err := json.Marshal(title)
	if err != nil {
		return err
	}
	if err := queueWarmAdd(ctx, tx, userID, title.Key, added); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO watchlist_items(user_id,item_key,source,title,added) VALUES(?,?,'imported',?,?) ON CONFLICT(user_id,item_key,source) DO NOTHING`, userID, title.Key, string(raw), added)
	return err
}

// ExportKeys reads the live watchlist, including manual and synced sources.
// Multiple sources for one title produce one exported entry.
func ExportKeys(ctx context.Context, db *sql.DB, userID int64) ([]core.RatingKey, error) {
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT item_key FROM watchlist_items WHERE user_id=? ORDER BY item_key`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keys := []core.RatingKey{}
	for rows.Next() {
		var key core.RatingKey
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}
