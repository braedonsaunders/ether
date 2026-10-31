package catalog

import (
	"context"
	"database/sql"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// releaseAddedSQL is the title's own release or air timestamp when it is
// known and not in the future. Descendant episodes, catalogue insertion
// (added_at), metadata refresh (fetched_at), publication and observation
// times are not date added.
const releaseAddedSQL = `CASE WHEN t.released_ts>0 AND t.released_ts<=? THEN t.released_ts ELSE 0 END`

func releaseAddedArgs(now time.Time) []any {
	return []any{now.Unix()}
}

// CachedAddedTimes reads each title's own release or air date without
// fetching or resolving metadata. Missing, unknown and future dates are
// omitted. Descendant episodes do not replace a series or season premiere.
func (c *Catalog) CachedAddedTimes(ctx context.Context, keys []core.RatingKey) (map[core.RatingKey]time.Time, error) {
	return cachedAirAddedTimes(ctx, c.db, keys, c.now())
}

func cachedAirAddedTimes(ctx context.Context, db *sql.DB, keys []core.RatingKey, now time.Time) (map[core.RatingKey]time.Time, error) {
	result := make(map[core.RatingKey]time.Time)
	const chunk = 400
	for start := 0; start < len(keys); start += chunk {
		end := min(start+chunk, len(keys))
		args := releaseAddedArgs(now)
		for _, key := range keys[start:end] {
			args = append(args, int64(key))
		}
		rows, err := db.QueryContext(ctx, `SELECT t.key,`+releaseAddedSQL+` FROM catalog_titles t WHERE t.key IN (`+placeholders(end-start)+`)`, args...)
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
