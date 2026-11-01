package catalog

import (
	"context"
	"errors"

	"github.com/braedonsaunders/ether/internal/core"
)

// CachedAiredEpisodes returns locally stored, aired, non-adult episodes in
// descending air-date order. A zero show key selects all shows. It performs
// no network access or viewer filtering; callers must enforce viewer policy
// before applying their final limit.
func (c *Catalog) CachedAiredEpisodes(ctx context.Context, show core.RatingKey, offset, limit int) (core.Page, error) {
	if show != 0 && show.Kind() != core.KindShow || offset < 0 {
		return core.Page{}, errors.New("catalog: invalid aired episode query")
	}
	if limit <= 0 {
		limit = 50
	}
	limit = min(limit, 500)
	asOf := c.now()
	where := `t.type='episode' AND t.adult=0 AND t.release_known=1 AND t.released_ts<=?`
	args := []any{asOf.Unix()}
	if show != 0 {
		where += ` AND t.show_key=?`
		args = append(args, int64(show))
	}
	var total int
	if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM catalog_titles t WHERE `+where, args...).Scan(&total); err != nil {
		return core.Page{}, err
	}
	query := `SELECT ` + prefixedCols("t") + ` FROM catalog_titles t WHERE ` + where + ` ORDER BY t.released_ts DESC,t.key ASC LIMIT ? OFFSET ?`
	items, err := c.queryTitles(ctx, query, append(append([]any{}, args...), limit, offset)...)
	if err != nil {
		return core.Page{}, err
	}
	if err := applyAirAddedTimes(ctx, c.db, items, asOf); err != nil {
		return core.Page{}, err
	}
	return core.Page{Items: items, Total: total}, nil
}
