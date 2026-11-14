package app

import (
	"context"
	"time"

	"github.com/braedonsaunders/ether/internal/cachefill"
	"github.com/braedonsaunders/ether/internal/core"
)

// The queue is written on membership/availability edges. We never enumerate
// watchlists to replenish the cache, including after eviction or restart.
func (a *App) cacheWatchlistIntents(ctx context.Context) ([]cachefill.WarmIntent, error) {
	if a.watchlist == nil {
		return nil, nil
	}
	settings, err := a.watchlist.Settings(ctx)
	if err != nil || !settings.Prewarm {
		return nil, err
	}
	users, err := a.users.Users(ctx)
	if err != nil {
		return nil, err
	}
	byID := map[int64]core.User{}
	for _, u := range users {
		byID[u.ID] = u
	}
	rows, err := a.db.QueryContext(ctx, `SELECT user_id,item_key,added FROM watchlist_warm_intents q WHERE EXISTS(SELECT 1 FROM watchlist_items w WHERE w.user_id=q.user_id AND w.item_key=q.item_key) ORDER BY added DESC LIMIT 200`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []cachefill.WarmIntent
	for rows.Next() {
		var uid, added int64
		var key core.RatingKey
		if err := rows.Scan(&uid, &key, &added); err != nil {
			return nil, err
		}
		// Unknown RSS authors have no viewer activity evidence. Do not attribute
		// their viewing or client preferences to the server owner.
		u := byID[uid]
		out = append(out, cachefill.WarmIntent{User: u, Key: key, Added: time.Unix(added, 0), UserID: uid})
	}
	return out, rows.Err()
}
func (a *App) cacheWatchlistDone(ctx context.Context, intent cachefill.WarmIntent) error {
	_, err := a.db.ExecContext(ctx, `DELETE FROM watchlist_warm_intents WHERE user_id=? AND item_key=? AND added=?`, intent.UserID, intent.Key, intent.Added.Unix())
	return err
}

func (a *App) cacheWatchlistPosition(ctx context.Context, uid int64, show core.RatingKey) (core.WatchStatus, error) {
	return a.state.ShowPosition(ctx, uid, show)
}
