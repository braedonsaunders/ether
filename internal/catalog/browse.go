package catalog

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

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
	switch q.Filter.Type {
	case core.TypeEpisode:
		return c.BrowseStoredEpisodes(ctx, q)
	case core.TypeSeason:
		return c.BrowseStoredSeasons(ctx, q)
	}
	if q.Filter.Person > 0 {
		c.ensurePersonCredits(ctx, q.Filter.Person)
	}
	for _, cf := range q.Filter.Credits {
		for _, p := range cf.People {
			if p > 0 {
				c.ensurePersonCredits(ctx, p)
			}
		}
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
	ckey := browseFilterKey(q.Filter)
	bkey := fmt.Sprintf("%s|%s|%v|%d|%d|%v", ckey, q.Sort, q.Desc, offset, limit, q.NoTotal)
	c.cacheMu.RLock()
	if ent, ok := c.browseCache[bkey]; ok && ent.gen == gen && ent.minute == minBucket {
		items := make([]core.Title, len(ent.page.Items))
		for i := range ent.page.Items {
			items[i] = cloneTitle(ent.page.Items[i])
		}
		c.cacheMu.RUnlock()
		c.kickMovieYears(items)
		return core.Page{Items: items, Total: ent.page.Total}, nil
	}
	cntEnt, hasCnt := c.countCache[ckey]
	c.cacheMu.RUnlock()

	where, args, err := c.browseWhere(q.Filter)
	if err != nil {
		return core.Page{}, err
	}
	var total int
	// A page answered with a stale total is not cached, so the recounted
	// total reaches the next read.
	staleTotal := false
	switch {
	case q.NoTotal:
		// The page query alone; the total is set from the page below.
		total = offset + limit
	case hasCnt && cntEnt.gen == gen && cntEnt.minute == minBucket:
		total = cntEnt.total
	case hasCnt && cntEnt.total >= staleCountMin:
		// A large total walks the whole filtered set. Serve the last count
		// and recount behind the read, at most once per recountEvery: the
		// catalogue writes continuously, and a total that old only moves
		// the end of the last page.
		total, staleTotal = cntEnt.total, true
		if time.Since(cntEnt.counted) >= recountEvery {
			_ = c.hydrate(ctx, "count:"+ckey, 0, func(ctx context.Context) error {
				_, err := c.countBrowse(ctx, ckey, where, args)
				return err
			})
		}
	default:
		if total, err = c.countBrowse(ctx, ckey, where, args); err != nil {
			return core.Page{}, err
		}
	}
	if total == 0 || offset >= total {
		page := core.Page{Items: []core.Title{}, Total: total}
		if c.writeGen.Load() == gen && !staleTotal {
			c.cacheMu.Lock()
			if len(c.browseCache) >= 512 {
				c.browseCache = map[string]cachedBrowse{}
			}
			c.browseCache[bkey] = cachedBrowse{page: page, gen: gen, minute: minBucket}
			c.cacheMu.Unlock()
		}
		return page, nil
	}
	pageWhere, pageArgs := where, args
	if q.Sort == core.SortUpdated && q.Filter.UpdatedAfter == 0 {
		pageWhere += " AND t.fetched_at>=?"
		pageArgs = append(append([]any{}, args...), int64(0))
	}
	order := browseOrder(q)
	orderArgs := []any{}
	asOf := c.now()
	computedAdded := q.Sort == core.SortAdded && q.Filter.Type != core.TypeMovie
	if computedAdded {
		dir := "ASC"
		if q.Desc {
			dir = "DESC"
		}
		order = airAddedSQL + " " + dir
		orderArgs = airAddedArgs(asOf)
	}
	// A movie's date added is its release date, and browseWhere keeps only
	// movies released by now, so browseOrder's t.released_ts gives the same
	// order from the (type, browsable, released_ts) index instead of a
	// computed expression sorted over the whole library.
	query := `SELECT ` + prefixedCols("t") + ` FROM catalog_titles t WHERE ` + pageWhere + ` ORDER BY ` + order + `, t.key ASC LIMIT ? OFFSET ?`
	if computedAdded {
		// The computed date added has no index, so the whole filtered set is
		// sorted. Sort narrow (key, date) rows from the covering index and
		// read the full rows of the page only.
		dir := "ASC"
		if q.Desc {
			dir = "DESC"
		}
		query = `SELECT ` + prefixedCols("t") + ` FROM catalog_titles t JOIN (SELECT t.key AS page_key, ` + airAddedSQL + ` AS page_added FROM catalog_titles t WHERE ` + pageWhere +
			` ORDER BY page_added ` + dir + `, t.key ASC LIMIT ? OFFSET ?) p ON t.key=p.page_key ORDER BY p.page_added ` + dir + `, p.page_key ASC`
	}
	queryArgs := append(append([]any{}, pageArgs...), orderArgs...)
	if computedAdded {
		// The date expression comes before the filter in the narrow subquery.
		queryArgs = append(append([]any{}, orderArgs...), pageArgs...)
	}
	var items []core.Title
	walked := false
	if computedAdded && q.Desc && q.Filter.Type == core.TypeShow && (q.NoTotal || total >= recentWalkMinTotal) {
		// Newest-first series by date added, from the aired-episode index.
		page, ok, err := c.recentShowsPage(ctx, where, args, asOf, offset, limit)
		if err != nil {
			return core.Page{}, err
		}
		if ok {
			walked = true
			keys := make([]core.RatingKey, len(page))
			for i, a := range page {
				keys[i] = a.key
			}
			if items, err = c.titlesInKeyOrder(ctx, keys); err != nil {
				return core.Page{}, err
			}
		}
	}
	switch {
	case walked:
	case q.Filter.Type == "" && !computedAdded && q.Sort != core.SortTitle && offset+limit <= mergedPageMax:
		// Movies and series together: no index orders both types, so the
		// sorted query reads every browsable title. Each type's page comes
		// from its own index and the two merge.
		keys, err := c.mergedTypeKeys(ctx, pageWhere, pageArgs, q, offset, limit)
		if err != nil {
			return core.Page{}, err
		}
		if items, err = c.titlesInKeyOrder(ctx, keys); err != nil {
			return core.Page{}, err
		}
	default:
		if items, err = c.queryTitles(ctx, query, append(queryArgs, limit, offset)...); err != nil {
			return core.Page{}, err
		}
	}
	if q.Sort == core.SortAdded {
		if err := applyAirAddedTimes(ctx, c.db, items, asOf); err != nil {
			return core.Page{}, err
		}
	}
	if items == nil {
		items = []core.Title{}
	}
	if q.NoTotal {
		total = offset + len(items)
	}
	if c.writeGen.Load() == gen && !staleTotal {
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
	c.kickMovieYears(items)
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

// browseFilterKey distinguishes a nil IncludeKeys (no restriction) from an
// empty list (match nothing). fmt %+v prints both as [].
func browseFilterKey(f core.Filter) string {
	key := fmt.Sprintf("%+v", f)
	if f.IncludeKeys != nil {
		key += "|include"
		for _, k := range f.IncludeKeys {
			key += fmt.Sprintf(",%d", k)
		}
	}
	return key
}

// Browse filters share one SQL statement; their combined parameters remain subject
// to the database's statement-wide variable limit.
func (c *Catalog) browseWhere(f core.Filter) (string, []any, error) {
	var conds []string
	var args []any
	nowTS := c.now().Unix()
	conds = append(conds, "t.browsable=1", "t.adult=0")
	if f.KnownAddedDate {
		// Same membership as airAddedSQL<>0. Browse already requires the
		// title's own premiere to be known and not in the future, and that
		// premiere is the date-added fallback, so the filter does not walk
		// episodes. The sort still uses airAddedSQL. released_ts>0 would drop
		// a pre-1970 premiere.
		conds = append(conds, "t.release_known=1 AND t.released_ts<=?")
		args = append(args, nowTS)
	}
	if f := c.visibilityFilter(); f != nil {
		q, a := f.SQLHidden()
		conds = append(conds, "NOT ("+q+")")
		args = append(args, a...)
	}
	if c.config().GateMoviesByHomeRelease && f.IncludeKeys == nil {
		conds = append(conds, homeReleaseSQL("t"))
		args = append(args, nowTS, nowTS-int64(homeReleaseGrace.Seconds()))
	}
	if f.IncludeKeys != nil {
		if len(f.IncludeKeys) == 0 {
			conds = append(conds, "1=0")
		} else {
			conds = append(conds, "t.key IN ("+placeholders(len(f.IncludeKeys))+")")
			for _, k := range f.IncludeKeys {
				args = append(args, k)
			}
		}
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
	if f.UpdatedAfter != 0 {
		conds = append(conds, "t.fetched_at>=?")
		args = append(args, f.UpdatedAfter)
	}
	if f.UpdatedBefore != 0 {
		conds = append(conds, "t.fetched_at<=? AND t.fetched_at>0")
		args = append(args, f.UpdatedBefore)
	}
	if f.AddedAfter != 0 {
		if f.Type == core.TypeMovie {
			conds = append(conds, "t.released_ts>=?")
			args = append(args, f.AddedAfter)
		} else {
			conds = append(conds, `(t.released_ts>=? OR (t.type='show' AND t.key IN (SELECT show_key FROM catalog_titles WHERE type='episode' AND release_known=1 AND adult=0 AND released_ts>=? AND released_ts<=?)))`)
			args = append(args, f.AddedAfter, f.AddedAfter, nowTS)
		}
	}
	if f.AddedBefore != 0 {
		if f.Type == core.TypeMovie {
			conds = append(conds, "t.released_ts<=?")
			args = append(args, f.AddedBefore)
		} else {
			conds = append(conds, `(t.released_ts<=? AND (t.type<>'show' OR t.key NOT IN (SELECT show_key FROM catalog_titles WHERE type='episode' AND release_known=1 AND adult=0 AND released_ts>? AND released_ts<=?)))`)
			args = append(args, f.AddedBefore, f.AddedBefore, nowTS)
		}
	}
	if len(f.Genres) > 0 {
		conds = append(conds, `EXISTS (SELECT 1 FROM catalog_genres g WHERE g.key=t.key AND g.genre IN (`+placeholders(len(f.Genres))+`))`)
		for _, g := range f.Genres {
			args = append(args, g)
		}
	}
	for _, k := range f.Keywords {
		k = strings.ToLower(strings.TrimSpace(k))
		if k == "" {
			continue
		}
		conds = append(conds, `EXISTS (SELECT 1 FROM catalog_keywords kw WHERE kw.key=t.key AND LOWER(kw.keyword)=?)`)
		args = append(args, k)
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
	if len(f.Years) > 0 {
		conds = append(conds, "t.year IN ("+placeholders(len(f.Years))+")")
		for _, y := range f.Years {
			args = append(args, y)
		}
	}
	if len(f.ExcludeYears) > 0 {
		conds = append(conds, "t.year>0 AND t.year NOT IN ("+placeholders(len(f.ExcludeYears))+")")
		for _, y := range f.ExcludeYears {
			args = append(args, y)
		}
	}
	if f.MinRating > 0 {
		conds = append(conds, "t.rating>=?")
		args = append(args, f.MinRating)
	}
	if f.MaxRating > 0 {
		conds = append(conds, "t.rating>0 AND t.rating<=?")
		args = append(args, f.MaxRating)
	}
	if len(f.ExcludeRatings) > 0 {
		conds = append(conds, "t.rating>0 AND t.rating NOT IN ("+placeholders(len(f.ExcludeRatings))+")")
		for _, r := range f.ExcludeRatings {
			args = append(args, r)
		}
	}
	if f.MaxRuntime > 0 {
		conds = append(conds, "t.runtime_sec>0 AND t.runtime_sec<=?")
		args = append(args, int64(f.MaxRuntime.Seconds()))
	}
	if f.MinRuntime > 0 {
		conds = append(conds, "t.runtime_sec>=?")
		args = append(args, int64(f.MinRuntime.Seconds()))
	}
	if len(f.ExcludeRuntimes) > 0 {
		conds = append(conds, "t.runtime_sec>0 AND t.runtime_sec NOT IN ("+placeholders(len(f.ExcludeRuntimes))+")")
		for _, d := range f.ExcludeRuntimes {
			args = append(args, int64(d.Seconds()))
		}
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
	if len(f.ExcludeCountries) > 0 {
		conds = append(conds, `NOT EXISTS (SELECT 1 FROM catalog_countries co WHERE co.key=t.key AND co.country IN (`+placeholders(len(f.ExcludeCountries))+`))`)
		for _, v := range f.ExcludeCountries {
			args = append(args, v)
		}
	}
	if len(f.ContentRatings) > 0 {
		conds = append(conds, "t.content_rating IN ("+placeholders(len(f.ContentRatings))+")")
		for _, v := range f.ContentRatings {
			args = append(args, v)
		}
	}
	if len(f.ExcludeContentRatings) > 0 {
		conds = append(conds, "t.content_rating NOT IN ("+placeholders(len(f.ExcludeContentRatings))+")")
		for _, v := range f.ExcludeContentRatings {
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
	if f.Collection > 0 && len(f.Collections) == 0 {
		conds = append(conds, "t.collection_id=?")
		args = append(args, f.Collection)
	}
	if len(f.Collections) > 0 {
		conds = append(conds, "t.collection_id IN ("+placeholders(len(f.Collections))+")")
		for _, id := range f.Collections {
			args = append(args, id)
		}
	}
	if len(f.ExcludeCollections) > 0 {
		conds = append(conds, "t.collection_id NOT IN ("+placeholders(len(f.ExcludeCollections))+")")
		for _, id := range f.ExcludeCollections {
			args = append(args, id)
		}
	}
	if f.Person > 0 && len(f.Credits) == 0 {
		// IN, not a correlated EXISTS: a person's few titles drive the
		// query by key instead of a walk over every browsable title.
		conds = append(conds, `t.key IN (SELECT key FROM catalog_credits WHERE person_tmdb=?)`)
		args = append(args, f.Person)
	}
	for _, cf := range f.Credits {
		if len(cf.People) == 0 {
			continue
		}
		ph := placeholders(len(cf.People))
		if cf.Exclude {
			if cf.Kind != "" {
				conds = append(conds, `NOT EXISTS (SELECT 1 FROM catalog_credits cc WHERE cc.key=t.key AND cc.person_tmdb IN (`+ph+`) AND cc.kind=?)`)
			} else {
				conds = append(conds, `NOT EXISTS (SELECT 1 FROM catalog_credits cc WHERE cc.key=t.key AND cc.person_tmdb IN (`+ph+`))`)
			}
		} else {
			if cf.Kind != "" {
				conds = append(conds, `t.key IN (SELECT key FROM catalog_credits WHERE person_tmdb IN (`+ph+`) AND kind=?)`)
			} else {
				conds = append(conds, `t.key IN (SELECT key FROM catalog_credits WHERE person_tmdb IN (`+ph+`))`)
			}
		}
		for _, p := range cf.People {
			args = append(args, p)
		}
		if cf.Kind != "" {
			args = append(args, cf.Kind)
		}
	}
	if f.Studio != "" && len(f.StudioFilters) == 0 {
		conds = append(conds, `t.key IN (SELECT key FROM catalog_studios WHERE studio=?)`)
		args = append(args, f.Studio)
	}
	for _, sf := range f.StudioFilters {
		val := strings.ToLower(sf.Value)
		switch sf.Op {
		case "==":
			conds = append(conds, `EXISTS (SELECT 1 FROM catalog_studios s WHERE s.key=t.key AND LOWER(s.studio)=?)`)
			args = append(args, val)
		case "!==":
			conds = append(conds, `NOT EXISTS (SELECT 1 FROM catalog_studios s WHERE s.key=t.key AND LOWER(s.studio)=?)`)
			args = append(args, val)
		case "!=":
			conds = append(conds, `NOT EXISTS (SELECT 1 FROM catalog_studios s WHERE s.key=t.key AND LOWER(s.studio) LIKE ? ESCAPE '\')`)
			args = append(args, "%"+escapeLike(val)+"%")
		case "<=":
			conds = append(conds, `EXISTS (SELECT 1 FROM catalog_studios s WHERE s.key=t.key AND LOWER(s.studio) LIKE ? ESCAPE '\')`)
			args = append(args, escapeLike(val)+"%")
		case ">=":
			conds = append(conds, `EXISTS (SELECT 1 FROM catalog_studios s WHERE s.key=t.key AND LOWER(s.studio) LIKE ? ESCAPE '\')`)
			args = append(args, "%"+escapeLike(val))
		default: // "="
			conds = append(conds, `EXISTS (SELECT 1 FROM catalog_studios s WHERE s.key=t.key AND LOWER(s.studio) LIKE ? ESCAPE '\')`)
			args = append(args, "%"+escapeLike(val)+"%")
		}
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
	for _, tf := range f.TitleFilters {
		val := strings.ToLower(tf.Value)
		switch tf.Op {
		case "==":
			conds = append(conds, `LOWER(t.title)=?`)
			args = append(args, val)
		case "!==":
			conds = append(conds, `LOWER(t.title)<>?`)
			args = append(args, val)
		case "!=":
			conds = append(conds, `LOWER(t.title) NOT LIKE ? ESCAPE '\'`)
			args = append(args, "%"+escapeLike(val)+"%")
		case "<=":
			conds = append(conds, `LOWER(t.title) LIKE ? ESCAPE '\'`)
			args = append(args, escapeLike(val)+"%")
		case ">=":
			conds = append(conds, `LOWER(t.title) LIKE ? ESCAPE '\'`)
			args = append(args, "%"+escapeLike(val))
		default: // "="
			match, matchArgs := c.titleMatch("t.", tf.Value)
			conds = append(conds, match)
			args = append(args, matchArgs...)
		}
	}
	for _, rf := range f.ReleasedFilters {
		const day = 86400
		switch rf.Op {
		case "<<":
			conds = append(conds, "t.released_ts<?")
			args = append(args, rf.Unix)
		case "<=":
			end := rf.Unix
			if rf.SpanDay {
				end += day - 1
			}
			conds = append(conds, "t.released_ts<=?")
			args = append(args, end)
		case ">>":
			conds = append(conds, "t.released_ts>?")
			args = append(args, rf.Unix)
		case ">=":
			conds = append(conds, "t.released_ts>=?")
			args = append(args, rf.Unix)
		case "!=", "!==":
			if rf.SpanDay {
				conds = append(conds, "(t.released_ts<? OR t.released_ts>=?)")
				args = append(args, rf.Unix, rf.Unix+day)
			} else {
				conds = append(conds, "t.released_ts<>?")
				args = append(args, rf.Unix)
			}
		default: // "=", "=="
			if rf.SpanDay {
				conds = append(conds, "t.released_ts>=? AND t.released_ts<?")
				args = append(args, rf.Unix, rf.Unix+day)
			} else {
				conds = append(conds, "t.released_ts=?")
				args = append(args, rf.Unix)
			}
		}
	}
	conds = append(conds, "t.release_known=1 AND t.released_ts<=?")
	args = append(args, nowTS)
	return strings.Join(conds, " AND "), args, nil
}

func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// staleCountMin is the total from which Browse serves a stale count while it
// recounts; a smaller count is cheap enough to take on the request.
const staleCountMin = 10000

// recountEvery spaces the background recounts of one stale total or facet.
var recountEvery = 30 * time.Second

// countBrowse counts a browse filter's matches and caches the total.
func (c *Catalog) countBrowse(ctx context.Context, ckey, where string, args []any) (int, error) {
	gen := c.writeGen.Load()
	minBucket := c.minuteBucket()
	var total int
	if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM catalog_titles t WHERE `+where, args...).Scan(&total); err != nil {
		return 0, err
	}
	// Stamped with the generation it started at, so a write during the
	// count makes the next read recount; storing it even then keeps a busy
	// catalogue's stale total moving.
	c.cacheMu.Lock()
	if len(c.countCache) >= 1024 {
		c.countCache = map[string]cachedCount{}
	}
	c.countCache[ckey] = cachedCount{total: total, gen: gen, minute: minBucket, counted: time.Now()}
	c.cacheMu.Unlock()
	return total, nil
}

// Facet returns value counts over the browsable set.
func (c *Catalog) Facet(ctx context.Context, t core.MediaType, facet string) ([]core.FacetValue, error) {
	return c.FacetFilter(ctx, core.Filter{Type: t}, facet)
}

// FacetFilter returns value counts over the browsable set matching f.
func (c *Catalog) FacetFilter(ctx context.Context, f core.Filter, facet string) ([]core.FacetValue, error) {
	if err := c.admitFallbacks(ctx); err != nil {
		return nil, err
	}
	if facet == "firstCharacter" {
		f.FirstCharacter = ""
	}
	gen := c.writeGen.Load()
	minBucket := c.minuteBucket()
	fkey := fmt.Sprintf("%+v|%s", f, facet)
	c.cacheMu.RLock()
	ent, cached := c.facetCache[fkey]
	c.cacheMu.RUnlock()
	if cached && ent.gen == gen && ent.minute == minBucket {
		return append([]core.FacetValue(nil), ent.vals...), nil
	}
	if cached && facet == "firstCharacter" && time.Since(ent.counted) < jumpBarRecountEvery {
		// The jump bar's counts must match the listing, so they are not
		// served stale while a recount runs; but a catalogue writing
		// continuously would recount them on every read.
		return append([]core.FacetValue(nil), ent.vals...), nil
	}
	if cached && facet != "firstCharacter" {
		// Library-wide counts walk every title. Serve the last counts and
		// recount behind the read, at most once per recountEvery.
		if time.Since(ent.counted) >= recountEvery {
			_ = c.hydrate(ctx, "facet:"+fkey, 0, func(ctx context.Context) error {
				_, err := c.countFacet(ctx, f, facet)
				return err
			})
		}
		return append([]core.FacetValue(nil), ent.vals...), nil
	}
	return c.countFacet(ctx, f, facet)
}

// jumpBarRecountEvery is the most a jump bar's counts lag the catalogue.
const jumpBarRecountEvery = 2 * time.Second

// countFacet computes a facet's value counts over the browsable set and
// caches them.
func (c *Catalog) countFacet(ctx context.Context, f core.Filter, facet string) ([]core.FacetValue, error) {
	if facet == "firstCharacter" {
		f.FirstCharacter = ""
	}
	gen := c.writeGen.Load()
	minBucket := c.minuteBucket()
	fkey := fmt.Sprintf("%+v|%s", f, facet)
	base, args, err := c.browseWhere(f)
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
	// Stamped with the generation it started at, as countBrowse does: a
	// write during the count makes the next read recount, and storing it
	// even then keeps a busy catalogue from counting on every read.
	c.cacheMu.Lock()
	if len(c.facetCache) >= 512 {
		c.facetCache = map[string]cachedFacet{}
	}
	c.facetCache[fkey] = cachedFacet{vals: append([]core.FacetValue(nil), out...), gen: gen, minute: minBucket, counted: time.Now()}
	c.cacheMu.Unlock()
	return out, nil
}

// mergedPageMax is the deepest page end mergedTypeKeys reads: each type
// reads offset+limit rows.
const mergedPageMax = 1000

type sortedKey struct {
	key core.RatingKey
	val any
}

// mergedTypeKeys returns the keys of a movies-and-series page in the sorted
// query's order (browseOrder, then key), from one indexed page per type.
// The sort column is numeric: text would merge by the database's collation.
func (c *Catalog) mergedTypeKeys(ctx context.Context, where string, args []any, q core.BrowseQuery, offset, limit int) ([]core.RatingKey, error) {
	order := browseOrder(q)
	col, dir, _ := strings.Cut(order, " ")
	desc := dir == "DESC"
	var all []sortedKey
	for _, typ := range []core.MediaType{core.TypeMovie, core.TypeShow} {
		rows, err := c.db.QueryContext(ctx, `SELECT t.key, `+col+` FROM catalog_titles t WHERE `+where+` AND t.type=? ORDER BY `+order+`, t.key ASC LIMIT ?`, append(append([]any{}, args...), string(typ), offset+limit)...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var key int64
			var val any
			if err := rows.Scan(&key, &val); err != nil {
				rows.Close()
				return nil, err
			}
			all = append(all, sortedKey{key: core.RatingKey(key), val: val})
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	slices.SortStableFunc(all, func(a, b sortedKey) int {
		if c := compareSQL(a.val, b.val); c != 0 {
			if desc {
				return -c
			}
			return c
		}
		return cmp.Compare(a.key, b.key)
	})
	if offset >= len(all) {
		return nil, nil
	}
	out := make([]core.RatingKey, 0, limit)
	for _, a := range all[offset:min(offset+limit, len(all))] {
		out = append(out, a.key)
	}
	return out, nil
}

// compareSQL orders two values of one numeric column as the database does:
// by value, NULL first; text, which does not occur, bytewise.
func compareSQL(a, b any) int {
	num := func(v any) (float64, bool) {
		switch x := v.(type) {
		case int64:
			return float64(x), true
		case float64:
			return x, true
		}
		return 0, false
	}
	if x, ok := num(a); ok {
		if y, ok := num(b); ok {
			return cmp.Compare(x, y)
		}
	}
	text := func(v any) string {
		switch x := v.(type) {
		case string:
			return x
		case []byte:
			return string(x)
		}
		return ""
	}
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return -1
	case b == nil:
		return 1
	}
	return strings.Compare(text(a), text(b))
}

// titlesInKeyOrder reads the titles of keys, in that order.
func (c *Catalog) titlesInKeyOrder(ctx context.Context, keys []core.RatingKey) ([]core.Title, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	args := make([]any, len(keys))
	at := make(map[core.RatingKey]int, len(keys))
	for i, k := range keys {
		args[i] = k
		at[k] = i
	}
	titles, err := c.queryTitles(ctx, `SELECT `+prefixedCols("t")+` FROM catalog_titles t WHERE t.key IN (`+placeholders(len(keys))+`)`, args...)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(titles, func(a, b core.Title) int { return at[a.Key] - at[b.Key] })
	return titles, nil
}
