package catalog

import (
	"context"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// CachedAddedTimes reads actual recorded catalogue addition times without
// fetching or resolving metadata. Missing items and zero timestamps are
// omitted. Duplicate keys have a single result.
func (c *Catalog) CachedAddedTimes(ctx context.Context, keys []core.RatingKey) (map[core.RatingKey]time.Time, error) {
	result := make(map[core.RatingKey]time.Time)
	const chunk = 400
	for start := 0; start < len(keys); start += chunk {
		end := min(start+chunk, len(keys))
		args := make([]any, end-start)
		for i, key := range keys[start:end] {
			args[i] = int64(key)
		}
		rows, err := c.db.QueryContext(ctx, `SELECT key,added_at FROM catalog_titles WHERE key IN (`+placeholders(len(args))+`) AND added_at<>0`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var key, added int64
			if err := rows.Scan(&key, &added); err != nil {
				rows.Close()
				return nil, err
			}
			result[core.RatingKey(key)] = time.Unix(added, 0).UTC()
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}
