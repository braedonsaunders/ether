package catalog

import (
	"context"
	"database/sql"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// airAddedSQL derives the catalogue date from aired episodes or release dates.
// All seven placeholders bind the same captured Unix time.
const airAddedSQL = `COALESCE(CASE WHEN t.type='show' THEN COALESCE((SELECT e.released_ts FROM catalog_titles e WHERE e.type='episode' AND e.show_key=t.key AND e.released_ts>0 AND e.released_ts<=? ORDER BY e.released_ts DESC LIMIT 1), CASE WHEN t.released_ts>0 AND t.released_ts<=? THEN t.released_ts END) WHEN t.type='season' THEN COALESCE((SELECT e.released_ts FROM catalog_titles e WHERE e.type='episode' AND e.show_key=t.show_key AND e.season=t.season AND e.released_ts>0 AND e.released_ts<=? ORDER BY e.released_ts DESC LIMIT 1), CASE WHEN t.released_ts>0 AND t.released_ts<=? THEN t.released_ts END, (SELECT e.released_ts FROM catalog_titles e WHERE e.type='episode' AND e.show_key=t.show_key AND e.released_ts>0 AND e.released_ts<=? ORDER BY e.released_ts DESC LIMIT 1), (SELECT s.released_ts FROM catalog_titles s WHERE s.key=t.show_key AND s.type='show' AND s.released_ts>0 AND s.released_ts<=?)) WHEN t.type IN ('movie','episode') AND t.released_ts>0 AND t.released_ts<=? THEN t.released_ts END,0)`

func airAddedArgs(now time.Time) []any {
	return []any{now.Unix(), now.Unix(), now.Unix(), now.Unix(), now.Unix(), now.Unix(), now.Unix()}
}

// CachedAddedTimes reads air-derived dates without fetching or resolving
// metadata. Missing, unknown and future dates are omitted.
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
