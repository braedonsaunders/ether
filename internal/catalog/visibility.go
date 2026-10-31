package catalog

import (
	"context"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/libraryfilter"
)

func (c *Catalog) visibilityFilter() *libraryfilter.Filter {
	c.cfgMu.RLock()
	defer c.cfgMu.RUnlock()
	return c.visibility
}
func (c *Catalog) Visible(t *core.Title) bool {
	return !c.visibilityFilter().Hidden(t) && (!c.config().GateMoviesByHomeRelease || c.homeVisible(t))
}

// FilterVisibility enriches remote summaries with already-known local facets;
// there are no detail fetches or indexer calls. Unknown metadata stays unknown.
func (c *Catalog) FilterVisibility(ctx context.Context, ts []core.Title) []core.Title {
	if err := c.admitFallbacks(ctx); err != nil {
		return nil
	}
	if len(c.config().Exclusions) == 0 && !c.config().GateMoviesByHomeRelease {
		return ts
	}
	if len(ts) == 0 {
		return ts
	}
	out := make([]core.Title, 0, len(ts))
	idx := map[core.RatingKey][]int{}
	for _, t := range ts {
		if _, ok := idx[t.Key]; !ok {
			idx[t.Key] = []int{len(out)}
			out = append(out, t)
		}
	}
	ts = out
	// Remote search summaries omit countries, and may omit genres. They also
	// omit certification (movieToTitle / tvToTitle). ts is deduplicated above,
	// so chunking bounds the IN parameters without querying a key twice.
	// A stored content rating replaces a summary value so library SQL and this
	// pass agree: a known allowed rating is not treated as missing, and a
	// known blocked rating is not revealed by a summary that says otherwise.
	// An empty local rating leaves the summary rating, if it has one.
	const batch = 200
	for _, p := range []struct {
		table, col string
		set        func(int, string)
	}{
		{"catalog_countries", "country", func(i int, v string) { out[i].Countries = append(out[i].Countries, v) }},
		{"catalog_genres", "genre", func(i int, v string) { out[i].Genres = append(out[i].Genres, v) }},
	} {
		for start := 0; start < len(ts); start += batch {
			part := ts[start:min(start+batch, len(ts))]
			if err := c.scanPairs(ctx, "SELECT key,"+p.col+" FROM "+p.table+" WHERE key IN ("+placeholders(len(part))+")", keyArgs(part), p.set, idx); err != nil {
				return nil
			}
		}
	}
	for start := 0; start < len(ts); start += batch {
		part := ts[start:min(start+batch, len(ts))]
		if err := c.scanPairs(ctx, "SELECT key, content_rating FROM catalog_titles WHERE key IN ("+placeholders(len(part))+") AND content_rating<>''", keyArgs(part), func(i int, v string) {
			out[i].ContentRating = v
		}, idx); err != nil {
			return nil
		}
	}
	if c.config().GateMoviesByHomeRelease {
		if err := c.hydrateHomeRelease(ctx, out); err != nil {
			return nil
		}
	}
	keep := out[:0]
	f := c.visibilityFilter()
	for i := range out {
		if !f.Hidden(&out[i]) && (!c.config().GateMoviesByHomeRelease || homeReleaseVisible(&out[i], c.now())) {
			keep = append(keep, out[i])
		}
	}
	return keep
}

type VisibilityPreview struct {
	Total          int          `json:"total"`
	Hidden         int          `json:"hidden"`
	Movies         int          `json:"movies"`
	Shows          int          `json:"shows"`
	UnknownCountry int          `json:"unknownCountry"`
	Items          []core.Title `json:"items"`
}

func (c *Catalog) PreviewVisibility(ctx context.Context, specs []core.LibraryExclusion) (any, error) {
	f, err := libraryfilter.Compile(specs)
	if err != nil {
		return nil, err
	}
	where, args := f.SQLHidden()
	p := VisibilityPreview{Items: []core.Title{}}
	err = c.db.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(CASE WHEN NOT EXISTS(SELECT 1 FROM catalog_countries co WHERE co.key=t.key) THEN 1 ELSE 0 END),0) FROM catalog_titles t WHERE `+mirrorBrowsableWhere).Scan(&p.Total, &p.UnknownCountry)
	if err != nil {
		return nil, err
	}
	base := mirrorBrowsableWhere + " AND (" + where + ")"
	err = c.db.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(CASE WHEN t.type='movie' THEN 1 ELSE 0 END),0),COALESCE(SUM(CASE WHEN t.type='show' THEN 1 ELSE 0 END),0) FROM catalog_titles t WHERE `+base, args...).Scan(&p.Hidden, &p.Movies, &p.Shows)
	if err != nil {
		return nil, err
	}
	p.Items, err = c.queryTitles(ctx, `SELECT `+prefixedCols("t")+` FROM catalog_titles t WHERE `+base+` ORDER BY t.popularity DESC,t.key LIMIT 48`, args...)
	if p.Items == nil {
		p.Items = []core.Title{}
	}
	return p, err
}
