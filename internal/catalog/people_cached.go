package catalog

import (
	"context"

	"github.com/braedonsaunders/ether/internal/core"
)

// CachedCreditItems pages existing cached credit membership, including titles
// opened on demand. It never fetches metadata or hydrates filmographies.
// Viewer and inherited-title policy belongs to the consuming Experience.
func (c *Catalog) CachedCreditItems(ctx context.Context, after core.RatingKey, limit int) ([]core.RatingKey, error) {
	if after < 0 || limit < 1 || limit > 500 {
		return nil, core.ErrNotAvailable
	}
	rows, err := c.db.QueryContext(ctx, `SELECT DISTINCT c.key FROM catalog_credits c JOIN catalog_titles t ON t.key=c.key WHERE c.person_tmdb>0 AND c.name<>'' AND c.key>? AND t.type IN ('movie','show','season','episode') ORDER BY c.key LIMIT ?`, int64(after), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keys := make([]core.RatingKey, 0, limit)
	for rows.Next() {
		var key core.RatingKey
		if err = rows.Scan(&key); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}
