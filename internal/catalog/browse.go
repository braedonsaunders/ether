package catalog

import (
	"context"
	"fmt"
	"strings"

	"github.com/braedonsaunders/ether/internal/contentrating"
	"github.com/braedonsaunders/ether/internal/core"
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

// CountBrowsable is the library size behind a view list. The (type, browsable,
// adult) prefix of catalog_titles_type_browse_sort answers it without reading
// title rows.
func (c *Catalog) CountBrowsable(ctx context.Context, typ core.MediaType) (int, error) {
	if c == nil || c.db == nil {
		return 0, fmt.Errorf("catalog: count unavailable")
	}
	if typ != core.TypeMovie && typ != core.TypeShow {
		return 0, fmt.Errorf("catalog: count: unsupported type %q", typ)
	}
	gen := c.writeGen.Load()
	minute := c.minuteBucket()
	key := "count-browsable:" + string(typ)
	c.cacheMu.RLock()
	if ent, ok := c.countCache[key]; ok && ent.gen == gen && ent.minute == minute {
		c.cacheMu.RUnlock()
		return ent.total, nil
	}
	c.cacheMu.RUnlock()
	var total int
	if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM catalog_titles WHERE type=? AND browsable=1 AND adult=0`, string(typ)).Scan(&total); err != nil {
		return 0, err
	}
	if c.writeGen.Load() == gen {
		c.cacheMu.Lock()
		if c.countCache == nil {
			c.countCache = map[string]cachedCount{}
		}
		c.countCache[key] = cachedCount{total: total, gen: gen, minute: minute}
		c.cacheMu.Unlock()
	}
	return total, nil
}

// LibraryPosters returns the first poster paths for a library collage. It
// reads the popularity index and does not hydrate title rows.
func (c *Catalog) LibraryPosters(ctx context.Context, typ core.MediaType, limit int) ([]core.Title, error) {
	if c == nil || c.db == nil {
		return nil, fmt.Errorf("catalog: posters unavailable")
	}
	if typ != core.TypeMovie && typ != core.TypeShow {
		return nil, fmt.Errorf("catalog: posters: unsupported type %q", typ)
	}
	if limit <= 0 || limit > 16 {
		limit = 16
	}
	rows, err := c.db.QueryContext(ctx, `SELECT key, poster FROM catalog_titles WHERE type=? AND browsable=1 AND adult=0 AND poster<>'' ORDER BY popularity DESC LIMIT ?`, string(typ), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]core.Title, 0, limit)
	for rows.Next() {
		var key int64
		var poster string
		if err = rows.Scan(&key, &poster); err != nil {
			return nil, err
		}
		out = append(out, core.Title{Key: core.RatingKey(key), Type: typ, PosterPath: poster})
	}
	return out, rows.Err()
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
	if len(f.ExcludeGenres) > 0 {
		conds = append(conds, `NOT EXISTS (SELECT 1 FROM catalog_genres g WHERE g.key=t.key AND LOWER(g.genre) IN (`+placeholders(len(f.ExcludeGenres))+`))`)
		for _, g := range f.ExcludeGenres {
			args = append(args, strings.ToLower(strings.TrimSpace(g)))
		}
	}
	if len(f.ExcludeKeywords) > 0 {
		conds = append(conds, `NOT EXISTS (SELECT 1 FROM catalog_keywords k WHERE k.key=t.key AND LOWER(k.keyword) IN (`+placeholders(len(f.ExcludeKeywords))+`))`)
		for _, k := range f.ExcludeKeywords {
			args = append(args, strings.ToLower(strings.TrimSpace(k)))
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
		if level := contentrating.LevelOf(f.MaxContentRating); level != contentrating.Unrated {
			conds = append(conds, "t.content_rating_rank>0 AND t.content_rating_rank<=?")
			args = append(args, int(level))
		} else {
			conds = append(conds, "0=1")
		}
	}
	if f.Collection > 0 {
		conds = append(conds, "t.collection_id=?")
		args = append(args, f.Collection)
	}
	if f.Person > 0 {
		// IN, not a correlated EXISTS: a person's few titles drive the
		// query by key instead of a walk over every browsable title.
		conds = append(conds, `t.key IN (SELECT key FROM catalog_credits WHERE person_tmdb=?)`)
		args = append(args, f.Person)
	}
	if f.Studio != "" {
		conds = append(conds, `t.key IN (SELECT key FROM catalog_studios WHERE studio=?)`)
		args = append(args, f.Studio)
	}
	if fc := strings.TrimSpace(f.FirstCharacter); fc != "" {
		switch {
		case fc == "#" || fc == "0":
			conds = append(conds, "t.sort_title<>'' AND t.sort_title < 'a'")
		case fc == "~" || fc == "…":
			conds = append(conds, "t.sort_title >= '{'")
		case len(fc) == 1 && ((fc[0] >= 'a' && fc[0] <= 'z') || (fc[0] >= 'A' && fc[0] <= 'Z')):
			lo := strings.ToLower(fc)
			hi := string(lo[0] + 1)
			conds = append(conds, "t.sort_title >= ? AND t.sort_title < ?")
			args = append(args, lo, hi)
		}
	}
	if f.Query != "" {
		match, matchArgs := c.titleMatch("t.", f.Query)
		conds = append(conds, match)
		args = append(args, matchArgs...)
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

	base, args, err := c.browseWhere(core.Filter{Type: t})
	if err != nil {
		return nil, err
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
	case "keyword":
		q = `SELECT k.keyword, COUNT(*) FROM catalog_keywords k JOIN catalog_titles t ON t.key=k.key WHERE ` + base + ` GROUP BY k.keyword ORDER BY COUNT(*) DESC, k.keyword ASC LIMIT 200`
	case "contentRating":
		q = `SELECT t.content_rating, COUNT(*) FROM catalog_titles t WHERE ` + base + ` AND t.content_rating<>'' GROUP BY t.content_rating ORDER BY MIN(t.content_rating_rank) ASC, t.content_rating ASC`
	case "contentRatingLevel":
		// Titles per contentrating.Level ("0" is unrated), mildest first.
		q = `SELECT CAST(t.content_rating_rank AS TEXT), COUNT(*) FROM catalog_titles t WHERE ` + base + ` GROUP BY t.content_rating_rank ORDER BY t.content_rating_rank ASC`
	case "firstCharacter":
		q = `SELECT CASE WHEN t.sort_title < 'a' THEN '#' WHEN SUBSTR(t.sort_title, 1, 1) BETWEEN 'a' AND 'z' THEN UPPER(SUBSTR(t.sort_title, 1, 1)) ELSE '~' END, COUNT(*) FROM catalog_titles t WHERE ` + base + ` AND t.sort_title<>'' GROUP BY 1 ORDER BY 1 ASC`
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
		switch {
		case facet == "decade":
			v.Title = v.Key + "s"
		case facet == "firstCharacter" && v.Key == "~":
			v.Title = "#"
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
