package catalog

import (
	"context"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/store"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/libraryfilter"
)

// VisibilityFilter returns the compiled canonical library visibility filter.
func (c *Catalog) VisibilityFilter() *libraryfilter.Filter {
	return c.visibilityFilter()
}

// visibilityFilter is the discovery filter: every rule applies.
func (c *Catalog) visibilityFilter() *libraryfilter.Filter {
	c.cfgMu.RLock()
	defer c.cfgMu.RUnlock()
	return c.visibility
}

// visibleFor is visibleWith for ctx's viewer, failing closed for a strict one.
func (c *Catalog) visibleFor(ctx context.Context, f *libraryfilter.Filter, t *core.Title) bool {
	id := core.CatalogViewerID(ctx)
	if t != nil && !c.hasCustomViewerRules(id) && c.isTitleOrShowOwnerIncluded(t) {
		return c.visibleWith(nil, t)
	}
	return c.strictVisible(f, c.strictViewer(id, core.CatalogViewerRestricted(ctx)), t)
}

// visibilityFor picks the precompiled filter for ctx. An explicit search
// skips rules the owner limited to browsing; everything else browses.
func (c *Catalog) visibilityFor(ctx context.Context) *libraryfilter.Filter {
	if core.CatalogCandidates(ctx) {
		return nil
	}
	return c.viewerFilter(core.CatalogViewerID(ctx), core.ExplicitSearch(ctx))
}

func (c *Catalog) searchFilter() *libraryfilter.Filter {
	c.cfgMu.RLock()
	defer c.cfgMu.RUnlock()
	return c.searchVis
}

// Visible applies every visibility rule, as browsing and recommendations do.
func (c *Catalog) Visible(t *core.Title) bool {
	if c.isTitleOrShowOwnerIncluded(t) {
		return c.visibleWith(nil, t)
	}
	return c.visibleWith(c.visibilityFilter(), t)
}

// SearchVisible skips rules limited to browsing, for explicit search results.
func (c *Catalog) SearchVisible(t *core.Title) bool {
	if c.isTitleOrShowOwnerIncluded(t) {
		return c.visibleWith(nil, t)
	}
	return c.visibleWith(c.searchFilter(), t)
}

func (c *Catalog) visibleWith(f *libraryfilter.Filter, t *core.Title) bool {
	return !f.Hidden(t) && (c.upcomingMovie(t) || !(c.config().ReleaseMode == config.ModeDigital) || c.homeVisible(t))
}

// FilterVisibility enriches remote summaries with already-known local facets;
// there are no detail fetches or indexer calls. Unknown metadata stays unknown,
// and for a restricted viewer an undetailed summary their rules cannot judge
// is dropped (fail closed).
// An explicit search context (core.WithExplicitSearch) skips rules the owner
// limited to browsing.
func (c *Catalog) FilterVisibility(ctx context.Context, ts []core.Title) []core.Title {
	f := c.visibilityFor(ctx)
	if f.Passive() && !(c.config().ReleaseMode == config.ModeDigital) {
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
		{"catalog_countries", "country", func(i int, v string) { out[i].Countries = appendUnique(out[i].Countries, v) }},
		{"catalog_genres", "genre", func(i int, v string) { out[i].Genres = appendUnique(out[i].Genres, v) }},
		{"catalog_studios", "studio", func(i int, v string) { out[i].Studios = appendUnique(out[i].Studios, v) }},
		{"catalog_keywords", "keyword", func(i int, v string) { out[i].Keywords = appendUnique(out[i].Keywords, v) }},
	} {
		for chunk := range store.Chunks(ts, batch) {
			part := chunk
			if err := c.scanPairs(ctx, "SELECT key,"+p.col+" FROM "+p.table+" WHERE key IN ("+placeholders(len(part))+")", keyArgs(part), p.set, idx); err != nil {
				return nil
			}
		}
	}
	for i := range out {
		out[i].Genres = uniqueStrings(out[i].Genres)
		out[i].Keywords = uniqueStrings(out[i].Keywords)
		out[i].Studios = uniqueStrings(out[i].Studios)
		out[i].Countries = uniqueStrings(out[i].Countries)
	}
	for chunk := range store.Chunks(ts, batch) {
		part := chunk
		if err := c.scanPairs(ctx, "SELECT key, content_rating FROM catalog_titles WHERE key IN ("+placeholders(len(part))+") AND content_rating<>''", keyArgs(part), func(i int, v string) {
			out[i].ContentRating = v
		}, idx); err != nil {
			return nil
		}
	}
	// A restricted viewer's rules must not wave through a summary they
	// cannot judge: a condition on an unknown field answers "no match", so
	// "hide rated R and above" would show an uncertified search hit. Titles
	// the catalogue holds details for are judged on those details, as
	// browsing judges them; anything else that is undecidable is dropped.
	viewerID := core.CatalogViewerID(ctx)
	allowIncluded := !c.hasCustomViewerRules(viewerID)
	strict := !f.Passive() && c.strictViewer(viewerID, core.CatalogViewerRestricted(ctx))
	detailed := map[int]bool{}
	if strict {
		for chunk := range store.Chunks(ts, batch) {
			part := chunk
			if err := c.scanPairs(ctx, "SELECT key, '1' FROM catalog_titles WHERE key IN ("+placeholders(len(part))+") AND detail<>0", keyArgs(part), func(i int, _ string) {
				detailed[i] = true
			}, idx); err != nil {
				return nil
			}
		}
	}
	if c.config().ReleaseMode == config.ModeDigital {
		if err := c.hydrateHomeRelease(ctx, out); err != nil {
			return nil
		}
	}
	keep := out[:0]
	for i := range out {
		included := allowIncluded && c.isTitleOrShowOwnerIncluded(&out[i])
		if !included && strict && !detailed[i] && !f.Decidable(&out[i]) {
			continue
		}
		if (included || !f.Hidden(&out[i])) && (c.upcomingMovie(&out[i]) || !(c.config().ReleaseMode == config.ModeDigital) || c.homeVisible(&out[i])) {
			keep = append(keep, out[i])
		}
	}
	return keep
}

type VisibilityRulePreview struct {
	Movies int `json:"movies"`
	Shows  int `json:"shows"`
}

type VisibilityPreview struct {
	Total          int                     `json:"total"`
	Hidden         int                     `json:"hidden"`
	Visible        int                     `json:"visible"`
	Movies         int                     `json:"movies"`
	Shows          int                     `json:"shows"`
	UnknownCountry int                     `json:"unknownCountry"`
	Items          []core.Title            `json:"items"`
	Shown          []core.Title            `json:"shown"`
	Rules          []VisibilityRulePreview `json:"rules"`
}

func (c *Catalog) PreviewVisibility(ctx context.Context, specs []core.LibraryExclusion) (any, error) {
	return c.PreviewVisibilityPolicy(ctx, specs, "")
}

func (c *Catalog) PreviewVisibilityPolicy(ctx context.Context, specs []core.LibraryExclusion, fallback string) (any, error) {
	return c.PreviewVisibilityScoped(ctx, specs, fallback, nil)
}

func (c *Catalog) PreviewVisibilityScoped(ctx context.Context, specs []core.LibraryExclusion, fallback string, scopeFilter *core.Filter) (any, error) {
	if scopeFilter != nil {
		if err := c.ensureCatalogueSources(ctx, *scopeFilter); err != nil {
			return nil, err
		}
	}
	f, err := c.compileLibraryRules(specs, fallback)
	if err != nil {
		return nil, err
	}
	where, args := f.SQLHidden()
	p := VisibilityPreview{Items: []core.Title{}, Shown: []core.Title{}, Rules: []VisibilityRulePreview{}}
	scope, scopeArgs := mirrorBrowsableWhere, []any(nil)
	if scopeFilter != nil {
		sf := *scopeFilter
		sf.RuleVisibility = "all"
		scope, scopeArgs, err = c.browseWhere(sf)
		if err != nil {
			return nil, err
		}
	} else if cfg := c.config(); cfg.ReleaseMode == config.ModeDigital {
		scope += " AND " + homeReleaseSQL("t")
		scopeArgs = []any{homeReleaseCutoff(c.now()), homeReleaseDeadline(c.now(), cfg.HomeReleaseOffsetDays)}
	}
	err = c.db.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(CASE WHEN NOT EXISTS(SELECT 1 FROM catalog_countries co WHERE co.key=t.key) THEN 1 ELSE 0 END),0) FROM catalog_titles t WHERE `+scope, scopeArgs...).Scan(&p.Total, &p.UnknownCountry)
	if err != nil {
		return nil, err
	}
	base := scope + " AND (" + where + ")"
	args = append(append([]any(nil), scopeArgs...), args...)
	err = c.db.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(CASE WHEN t.type='movie' THEN 1 ELSE 0 END),0),COALESCE(SUM(CASE WHEN t.type='show' THEN 1 ELSE 0 END),0) FROM catalog_titles t WHERE `+base, args...).Scan(&p.Hidden, &p.Movies, &p.Shows)
	if err != nil {
		return nil, err
	}
	p.Visible = p.Total - p.Hidden
	if p.Visible < 0 {
		p.Visible = 0
	}
	p.Items, err = c.queryTitles(ctx, `SELECT `+prefixedCols("t")+` FROM catalog_titles t WHERE `+base+` ORDER BY t.popularity DESC,t.key LIMIT 24`, args...)
	if err != nil {
		return nil, err
	}
	if p.Items == nil {
		p.Items = []core.Title{}
	}
	shownWhere, shownArgs := f.SQLHidden()
	shownArgs = append(append([]any(nil), scopeArgs...), shownArgs...)
	p.Shown, err = c.queryTitles(ctx, `SELECT `+prefixedCols("t")+` FROM catalog_titles t WHERE `+scope+` AND NOT (`+shownWhere+`) ORDER BY t.popularity DESC,t.key LIMIT 24`, shownArgs...)
	if err != nil {
		return nil, err
	}
	if p.Shown == nil {
		p.Shown = []core.Title{}
	}
	sel, catchArgs := f.CatchCountsSQL()
	if sel != "" {
		row := c.db.QueryRowContext(ctx, `SELECT `+sel+` FROM catalog_titles t WHERE `+scope, append(append([]any(nil), catchArgs...), scopeArgs...)...)
		nums := make([]int, len(specs)*2)
		dest := make([]any, len(nums))
		for i := range nums {
			dest[i] = &nums[i]
		}
		if err = row.Scan(dest...); err != nil {
			return nil, err
		}
		p.Rules = make([]VisibilityRulePreview, len(specs))
		for i := range specs {
			p.Rules[i] = VisibilityRulePreview{Movies: nums[i*2], Shows: nums[i*2+1]}
		}
	}
	return p, nil
}

func (c *Catalog) compileLibraryRules(rules []core.LibraryExclusion, fallback string) (*libraryfilter.Filter, error) {
	return c.compileLibraryRulesMode(rules, fallback, false)
}

func (c *Catalog) compileLibraryRulesMode(rules []core.LibraryExclusion, fallback string, membershipOnly bool) (*libraryfilter.Filter, error) {
	f, err := libraryfilter.CompilePolicy(rules, fallback)
	if err != nil {
		return nil, err
	}
	return c.bindLibraryRulesMode(f, membershipOnly)
}

func (c *Catalog) bindLibraryRules(f *libraryfilter.Filter) (*libraryfilter.Filter, error) {
	return c.bindLibraryRulesMode(f, false)
}

func (c *Catalog) bindLibraryRulesMode(f *libraryfilter.Filter, membershipOnly bool) (*libraryfilter.Filter, error) {
	return f.BindCatalogue(func(p core.Filter) (string, []any, error) {
		p.LocalOnly = true
		p.CatalogCandidates = true
		where, args, err := c.browseWhereMode(p, membershipOnly)
		if err == nil && where == "" {
			where = "1=1"
		}
		return where, args, err
	}, func(q string, args []any, t *core.Title) bool {
		if t == nil || t.Key == 0 {
			return false
		}
		// Direct lookup decisions use one indexed point read. Browse, counts and
		// facets evaluate the complete policy in their existing SQL statement.
		var count int
		err := c.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM catalog_titles t WHERE t.key=? AND (`+q+`)`, append([]any{int64(t.Key)}, args...)...).Scan(&count)
		return err == nil && count > 0
	})
}
