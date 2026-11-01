package catalog

import (
	"context"
	"database/sql"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// airAddedSQL is the owner date-added timestamp. Movies and episodes use
// their own release or air date. A series uses its latest episode aired at
// or before the bound time, then its first-air date. A season uses the
// latest episode aired in that season, then the season premiere. Episodes
// with no air date or a future air date do not count. Catalogue insertion
// (added_at), metadata refresh (fetched_at), publication and observation
// times are not date added. All five placeholders bind the same Unix time.
const airAddedSQL = `COALESCE(CASE WHEN t.type='show' THEN COALESCE((SELECT e.released_ts FROM catalog_titles e WHERE e.type='episode' AND e.show_key=t.key AND e.release_known=1 AND e.released_ts<=? ORDER BY e.released_ts DESC LIMIT 1), CASE WHEN t.release_known=1 AND t.released_ts<=? THEN t.released_ts END) WHEN t.type='season' THEN COALESCE((SELECT e.released_ts FROM catalog_titles e WHERE e.type='episode' AND e.show_key=t.show_key AND e.season=t.season AND e.release_known=1 AND e.released_ts<=? ORDER BY e.released_ts DESC LIMIT 1), CASE WHEN t.release_known=1 AND t.released_ts<=? THEN t.released_ts END) WHEN t.type IN ('movie','episode') AND t.release_known=1 AND t.released_ts<=? THEN t.released_ts END,0)`

func airAddedArgs(now time.Time) []any {
	unix := now.Unix()
	return []any{unix, unix, unix, unix, unix}
}

// CachedAddedTimes reads date added without fetching or resolving metadata.
// Missing, unknown and future dates are omitted. A later aired episode
// replaces a series or season premiere; an unaired episode does not.
func (c *Catalog) CachedAddedTimes(ctx context.Context, keys []core.RatingKey) (map[core.RatingKey]time.Time, error) {
	return cachedAirAddedTimes(ctx, c.db, keys, c.now())
}

func cachedAirAddedTimes(ctx context.Context, db *sql.DB, keys []core.RatingKey, now time.Time) (map[core.RatingKey]time.Time, error) {
	result := make(map[core.RatingKey]time.Time)
	const chunk = 400
	for start := 0; start < len(keys); start += chunk {
		end := min(start+chunk, len(keys))
		args := airAddedArgs(now)
		for _, key := range keys[start:end] {
			args = append(args, int64(key))
		}
		rows, err := db.QueryContext(ctx, `SELECT t.key,`+airAddedSQL+` FROM catalog_titles t WHERE t.key IN (`+placeholders(end-start)+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var key, added int64
			if err := rows.Scan(&key, &added); err != nil {
				rows.Close()
				return nil, err
			}
			if added != 0 {
				result[core.RatingKey(key)] = time.Unix(added, 0).UTC()
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}

func applyAirAddedTimes(ctx context.Context, db *sql.DB, items []core.Title, now time.Time) error {
	keys := make([]core.RatingKey, len(items))
	for i := range items {
		keys[i] = items[i].Key
	}
	times, err := cachedAirAddedTimes(ctx, db, keys, now)
	if err != nil {
		return err
	}
	for i := range items {
		items[i].AddedAt = times[items[i].Key]
	}
	return nil
}
