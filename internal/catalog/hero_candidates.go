package catalog

import (
	"context"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	// Separate component: no collision with catalogue migrations on other slices.
	store.Register("catalog_hero_candidates", 1, `CREATE INDEX catalog_hero_recent ON catalog_titles(type,released_ts DESC,key ASC) WHERE backdrop<>'' AND browsable=1 AND adult=0 AND release_known=1;`)
}

// HeroCandidates reads at most six locally stored, art-bearing titles. Trending
// uses the persisted chart without refreshing it; recent fill uses a partial
// ordered index. The ordinary browse policy SQL runs before LIMIT. Source
// backdrop identity is retained; pixel dimensions remain lazy artwork metadata.
func (c *Catalog) HeroCandidates(ctx context.Context, filter core.Filter, limit int, trending bool) ([]core.Title, error) {
	if limit <= 0 || (filter.Type != core.TypeMovie && filter.Type != core.TypeShow) {
		return nil, nil
	}
	limit = min(limit, 6)
	filter = c.viewerPaging(ctx, filter)
	where, args, err := c.browseWhere(filter)
	if err != nil {
		return nil, err
	}
	where += " AND t.backdrop<>'' AND t.browsable=1 AND t.adult=0 AND t.release_known=1"
	order := "t.released_ts DESC,t.key ASC"
	hint := c.indexHint("catalog_hero_recent")
	if trending {
		snapshot := c.loadTrend(ctx, "trending_week", filter.Type)
		if snapshot.fetched.IsZero() || c.now().Sub(snapshot.fetched) > trendMaxAge || len(snapshot.items) == 0 {
			return nil, nil
		}
		keys := make([]string, 0, min(len(snapshot.items), trendPoolSize))
		ranks := []string{"CASE t.key"}
		// Bound chart input independently of a malformed persisted snapshot.
		for n, item := range snapshot.items[:min(len(snapshot.items), trendPoolSize)] {
			keys = append(keys, "?")
			args = append(args, item.Key)
			// Integer identities originate in the stored chart, never SQL fragments.
			ranks = append(ranks, "WHEN "+strconv.FormatInt(int64(item.Key), 10)+" THEN "+strconv.Itoa(n))
		}
		where += " AND t.key IN (" + strings.Join(keys, ",") + ")"
		order = strings.Join(ranks, " ") + " ELSE 1000 END,t.key ASC"
		hint = "" // primary-key lookups over the bounded chart
	}
	args = append(args, limit)
	return c.queryPagedTitles(ctx, "SELECT "+prefixedCols("t")+" FROM catalog_titles t"+hint+" WHERE "+where+" ORDER BY "+order+" LIMIT ?", args...)
}
