package catalog

import (
	"context"
	"fmt"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// Browse queries the browsable mirror entirely in SQL, with generation-stamped
// in-memory count and page caching so repeated library/recommendation reads
// return without hitting disk.
func (c *Catalog) Browse(ctx context.Context, q core.BrowseQuery) (core.Page, error) {
	if err := c.admitFallbacks(ctx); err != nil {
		return core.Page{}, err
	}
	if q.Filter.Person > 0 {
		c.ensurePersonCredits(ctx, q.Filter.Person)
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	offset := q.Offset
	if offset < 0 {
		offset = 0
	}
	gen := c.writeGen.Load()
	minBucket := c.minuteBucket()
	ckey := fmt.Sprintf("%+v", q.Filter)
	bkey := fmt.Sprintf("%s|%s|%v|%d|%d", ckey, q.Sort, q.Desc, offset, limit)
	c.cacheMu.RLock()
	if ent, ok := c.browseCache[bkey]; ok && ent.gen == gen && ent.minute == minBucket {
		items := make([]core.Title, len(ent.page.Items))
		for i := range ent.page.Items {
			items[i] = cloneTitle(ent.page.Items[i])
		}
		c.cacheMu.RUnlock()
		return core.Page{Items: items, Total: ent.page.Total}, nil
	}
	cntEnt, hasCnt := c.countCache[ckey]
	c.cacheMu.RUnlock()

	where, args, err := c.browseWhere(q.Filter)
	if err != nil {
		return core.Page{}, err
	}
	var total int
	if hasCnt && cntEnt.gen == gen && cntEnt.minute == minBucket {
		total = cntEnt.total
	} else {
		if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM catalog_titles t WHERE `+where, args...).Scan(&total); err != nil {
			return core.Page{}, err
		}
		if c.writeGen.Load() == gen {
			c.cacheMu.Lock()
			if len(c.countCache) >= 1024 {
				c.countCache = map[string]cachedCount{}
			}
			c.countCache[ckey] = cachedCount{total: total, gen: gen, minute: minBucket}
			c.cacheMu.Unlock()
		}
	}
	if total == 0 || offset >= total {
		page := core.Page{Items: []core.Title{}, Total: total}
		if c.writeGen.Load() == gen {
			c.cacheMu.Lock()
			if len(c.browseCache) >= 512 {
				c.browseCache = map[string]cachedBrowse{}
			}
			c.browseCache[bkey] = cachedBrowse{page: page, gen: gen, minute: minBucket}
			c.cacheMu.Unlock()
		}
		return page, nil
	}
	order := browseOrder(q)
	orderArgs := []any{}
	asOf := c.now()
	if q.Sort == core.SortAdded {
		dir := "ASC"
		if q.Desc {
			dir = "DESC"
		}
		order = airAddedSQL + " " + dir
		orderArgs = airAddedArgs(asOf)
	}
	query := `SELECT ` + prefixedCols("t") + ` FROM catalog_titles t WHERE ` + where + ` ORDER BY ` + order + `, t.key ASC LIMIT ? OFFSET ?`
	queryArgs := append(append([]any{}, args...), orderArgs...)
	items, err := c.queryTitles(ctx, query, append(queryArgs, limit, offset)...)
	if err != nil {
		return core.Page{}, err
	}
	if q.Sort == core.SortAdded {
		if err := applyAirAddedTimes(ctx, c.db, items, asOf); err != nil {
			return core.Page{}, err
		}
	}
	if items == nil {
		items = []core.Title{}
	}
	if c.writeGen.Load() == gen {
		cachedItems := make([]core.Title, len(items))
		for i := range items {
			cachedItems[i] = cloneTitle(items[i])
		}
		c.cacheMu.Lock()
		if len(c.browseCache) >= 512 {
			c.browseCache = map[string]cachedBrowse{}
		}
		c.browseCache[bkey] = cachedBrowse{page: core.Page{Items: cachedItems, Total: total}, gen: gen, minute: minBucket}
		c.cacheMu.Unlock()
	}
	return core.Page{Items: items, Total: total}, nil
}

func prefixedCols(prefix string) string {
	parts := strings.Split(titleCols, ",")
	for i, p := range parts {
		parts[i] = prefix + "." + strings.TrimSpace(p)
	}
	return strings.Join(parts, ",")
}

func browseOrder(q core.BrowseQuery) string {
	col := "t.popularity"
	switch q.Sort {
	case core.SortTitle:
		col = "t.sort_title"
	case core.SortReleased:
		col = "t.released_ts"
	case core.SortRating:
		col = "t.rating"
	case core.SortAdded:
		col = "t.released_ts"
	case core.SortUpdated:
		col = "t.fetched_at"
	case core.SortPopularity, "":
		col = "t.popularity"
	}
	dir := "ASC"
	if q.Desc {
		dir = "DESC"
	}
	// Zero-value query: most popular first is the only sensible default.
	if q.Sort == "" && !q.Desc {
		dir = "DESC"
	}
	return col + " " + dir
}

// Browse filters share one SQL statement; their combined parameters remain subject
// to the database's statement-wide variable limit.
func (c *Catalog) browseWhere(f core.Filter) (string, []any, error) {
	var conds []string
	var args []any
	conds = append(conds, "t.browsable=1", "t.adult=0")
	if f.KnownAddedDate {
		conds = append(conds, airAddedSQL+">0")
		args = append(args, airAddedArgs(c.now())...)
	}
	if f := c.visibilityFilter(); f != nil {
		q, a := f.SQLHidden()
		conds = append(conds, "NOT ("+q+")")
		args = append(args, a...)
	}
	if c.config().GateMoviesByHomeRelease {
		conds = append(conds, homeReleaseSQL("t"))
		args = append(args, c.now().Unix(), c.now().Unix()-int64(homeReleaseGrace.Seconds()))
	}
	if len(f.ExcludeKeys) > 0 {
		conds = append(conds, "t.key NOT IN ("+placeholders(len(f.ExcludeKeys))+")")
		for _, k := range f.ExcludeKeys {
			args = append(args, k)
		}
	}
	switch f.Type {
	case core.TypeMovie, core.TypeShow:
		conds = append(conds, "t.type=?")
		args = append(args, string(f.Type))
	case "":
		conds = append(conds, "t.type IN ('movie','show')")
	default:
		return "", nil, fmt.Errorf("catalog: browse: unsupported type %q", f.Type)
	}
	if len(f.Genres) > 0 {
		conds = append(conds, `EXISTS (SELECT 1 FROM catalog_genres g WHERE g.key=t.key AND g.genre IN (`+placeholders(len(f.Genres))+`))`)
		for _, g := range f.Genres {
			args = append(args, g)
		}
	}
	if f.YearFrom > 0 {
		conds = append(conds, "t.year>=?")
		args = append(args, f.YearFrom)
	}
	if f.YearTo > 0 {
		conds = append(conds, "t.year<=? AND t.year>0")
		args = append(args, f.YearTo)
	}
	if f.MinRating > 0 {
		conds = append(conds, "t.rating>=?")
		args = append(args, f.MinRating)
	}
	if f.MaxRuntime > 0 {
		conds = append(conds, "t.runtime_sec>0 AND t.runtime_sec<=?")
		args = append(args, int64(f.MaxRuntime.Seconds()))
	}
	if f.MinRuntime > 0 {
		conds = append(conds, "t.runtime_sec>=?")
		args = append(args, int64(f.MinRuntime.Seconds()))
	}
	if len(f.Languages) > 0 {
		conds = append(conds, "t.language IN ("+placeholders(len(f.Languages))+")")
		for _, l := range f.Languages {
			args = append(args, l)
		}
	}
	if len(f.Countries) > 0 {
		conds = append(conds, `EXISTS (SELECT 1 FROM catalog_countries co WHERE co.key=t.key AND co.country IN (`+placeholders(len(f.Countries))+`))`)
		for _, v := range f.Countries {
			args = append(args, v)
		}
	}
	if len(f.ContentRatings) > 0 {
		conds = append(conds, "t.content_rating IN ("+placeholders(len(f.ContentRatings))+")")
		for _, v := range f.ContentRatings {
			args = append(args, v)
		}
	}
	if f.MaxContentRating != "" {
		if rank := RatingRank(f.MaxContentRating); rank > 0 {
			conds = append(conds, "t.content_rating_rank>0 AND t.content_rating_rank<=?")
			args = append(args, rank)
		} else {
			conds = append(conds, "0=1")
		}
	}
	if f.Collection > 0 {
		conds = append(conds, "t.collection_id=?")
		args = append(args, f.Collection)
	}
	if f.Person > 0 {
		conds = append(conds, `EXISTS (SELECT 1 FROM catalog_credits cr WHERE cr.key=t.key AND cr.person_tmdb=?)`)
		args = append(args, f.Person)
	}
	if f.Studio != "" {
		conds = append(conds, `EXISTS (SELECT 1 FROM catalog_studios s WHERE s.key=t.key AND s.studio=?)`)
		args = append(args, f.Studio)
	}
	if f.Query != "" {
		op := store.Like(store.DialectOf(c.db))
		conds = append(conds, "(t.title "+op+" ? ESCAPE '\\' OR t.original_title "+op+" ? ESCAPE '\\')")
		like := "%" + escapeLike(f.Query) + "%"
		args = append(args, like, like)
	}
	conds = append(conds, "t.released_ts<>0 AND t.released_ts<=?")
	args = append(args, c.now().Unix())
	return strings.Join(conds, " AND "), args, nil
}

func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// Facet returns value counts over the browsable set.
func (c *Catalog) Facet(ctx context.Context, t core.MediaType, facet string) ([]core.FacetValue, error) {
	if err := c.admitFallbacks(ctx); err != nil {
		return nil, err
	}
	gen := c.writeGen.Load()
	minBucket := c.minuteBucket()
	fkey := string(t) + "|" + facet
	c.cacheMu.RLock()
	if ent, ok := c.facetCache[fkey]; ok && ent.gen == gen && ent.minute == minBucket {
		out := append([]core.FacetValue(nil), ent.vals...)
		c.cacheMu.RUnlock()
		return out, nil
	}
	c.cacheMu.RUnlock()

	typeCond := "t.type IN ('movie','show')"
	var args []any
	if t == core.TypeMovie || t == core.TypeShow {
		typeCond = "t.type=?"
		args = append(args, string(t))
	}
	base := "t.browsable=1 AND t.adult=0 AND t.released_ts<>0 AND t.released_ts<=? AND " + typeCond
	args = append([]any{c.now().Unix()}, args...)
	if c.config().GateMoviesByHomeRelease {
		base += " AND " + homeReleaseSQL("t")
		args = append(args, c.now().Unix(), c.now().Unix()-int64(homeReleaseGrace.Seconds()))
	}
	var q string
	switch facet {
	case "language":
		q = `SELECT t.language,COUNT(*) FROM catalog_titles t WHERE ` + base + ` AND t.language<>'' GROUP BY t.language ORDER BY COUNT(*) DESC,t.language ASC LIMIT 200`
	case "genre":
		q = `SELECT g.genre, COUNT(*) FROM catalog_genres g JOIN catalog_titles t ON t.key=g.key WHERE ` + base + ` GROUP BY g.genre ORDER BY COUNT(*) DESC, g.genre ASC`
	case "studio":
		q = `SELECT s.studio, COUNT(*) FROM catalog_studios s JOIN catalog_titles t ON t.key=s.key WHERE ` + base + ` GROUP BY s.studio ORDER BY COUNT(*) DESC, s.studio ASC LIMIT 200`
	case "country":
		q = `SELECT co.country, COUNT(*) FROM catalog_countries co JOIN catalog_titles t ON t.key=co.key WHERE ` + base + ` GROUP BY co.country ORDER BY COUNT(*) DESC, co.country ASC LIMIT 200`
	case "year":
		q = `SELECT CAST(t.year AS TEXT), COUNT(*) FROM catalog_titles t WHERE ` + base + ` AND t.year>0 GROUP BY t.year ORDER BY t.year DESC`
	case "decade":
		q = `SELECT CAST((t.year/10)*10 AS TEXT), COUNT(*) FROM catalog_titles t WHERE ` + base + ` AND t.year>0 GROUP BY (t.year/10)*10 ORDER BY (t.year/10)*10 DESC`
	case "contentRating":
		q = `SELECT t.content_rating, COUNT(*) FROM catalog_titles t WHERE ` + base + ` AND t.content_rating<>'' GROUP BY t.content_rating ORDER BY MIN(t.content_rating_rank) ASC, t.content_rating ASC`
	case "firstCharacter":
		q = `SELECT CASE WHEN UPPER(SUBSTR(t.sort_title, 1, 1)) BETWEEN 'A' AND 'Z' THEN UPPER(SUBSTR(t.sort_title, 1, 1)) ELSE '#' END, COUNT(*) FROM catalog_titles t WHERE ` + base + ` AND t.sort_title<>'' GROUP BY 1 ORDER BY 1 ASC`
	default:
		return nil, fmt.Errorf("catalog: unknown facet %q", facet)
	}
	rows, err := c.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.FacetValue
	for rows.Next() {
		var v core.FacetValue
		if err := rows.Scan(&v.Key, &v.Count); err != nil {
			return nil, err
		}
		v.Title = v.Key
		if facet == "decade" {
			v.Title = v.Key + "s"
		}
		out = append(out, v)
	}
	if out == nil {
		out = []core.FacetValue{}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if c.writeGen.Load() == gen {
		c.cacheMu.Lock()
		c.facetCache[fkey] = cachedFacet{vals: append([]core.FacetValue(nil), out...), gen: gen, minute: minBucket}
		c.cacheMu.Unlock()
	}
	return out, nil
}
