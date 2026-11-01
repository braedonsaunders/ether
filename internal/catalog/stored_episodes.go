package catalog

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// Key bounds for episode and season rating keys.
var (
	episodeKeyStart = int64(core.KindEpisode)*1_000_000_000_000_000 - 1
	episodeKeyEnd   = int64(core.KindEpisode+1) * 1_000_000_000_000_000
	seasonKeyStart  = int64(core.KindSeason)*1_000_000_000_000_000 - 1
	seasonKeyEnd    = int64(core.KindSeason+1) * 1_000_000_000_000_000
)

// splitStoredFilter separates a child browse filter (episode or season) into
// the parent-series filter (t) and any child-level conditions.
func splitStoredFilter(f core.Filter, childKind core.KeyKind) (show core.Filter, childExclude []core.RatingKey) {
	show = core.Filter{
		KnownAddedDate:        f.KnownAddedDate,
		ExcludeGenres:         f.ExcludeGenres,
		ExcludeKeywords:       f.ExcludeKeywords,
		Type:                  core.TypeShow,
		Genres:                f.Genres,
		Keywords:              f.Keywords,
		Languages:             f.Languages,
		Countries:             f.Countries,
		ExcludeCountries:      f.ExcludeCountries,
		ContentRatings:        f.ContentRatings,
		ExcludeContentRatings: f.ExcludeContentRatings,
		MaxContentRating:      f.MaxContentRating,
		Collection:            f.Collection,
		Collections:           f.Collections,
		ExcludeCollections:    f.ExcludeCollections,
		Person:                f.Person,
		Credits:               f.Credits,
		Studio:                f.Studio,
		StudioFilters:         f.StudioFilters,
	}
	for _, k := range f.ExcludeKeys {
		switch k.Kind() {
		case childKind:
			childExclude = append(childExclude, k)
		default:
			show.ExcludeKeys = append(show.ExcludeKeys, k)
		}
	}
	return show, childExclude
}

func (c *Catalog) childFilterConds(alias string, f core.Filter, childExclude []core.RatingKey) ([]string, []any) {
	var conds []string
	var args []any
	if len(childExclude) > 0 {
		conds = append(conds, alias+".key NOT IN ("+placeholders(len(childExclude))+")")
		for _, k := range childExclude {
			args = append(args, k)
		}
	}
	if f.UpdatedAfter != 0 {
		conds = append(conds, alias+".fetched_at>=?")
		args = append(args, f.UpdatedAfter)
	}
	if f.UpdatedBefore != 0 {
		conds = append(conds, alias+".fetched_at<=? AND "+alias+".fetched_at>0")
		args = append(args, f.UpdatedBefore)
	}
	if f.AddedAfter != 0 {
		conds = append(conds, alias+".release_known=1 AND "+alias+".released_ts>=?")
		args = append(args, f.AddedAfter)
	}
	if f.AddedBefore != 0 {
		conds = append(conds, alias+".release_known=1 AND "+alias+".released_ts<=?")
		args = append(args, f.AddedBefore)
	}
	if f.YearFrom > 0 {
		conds = append(conds, alias+".year>=?")
		args = append(args, f.YearFrom)
	}
	if f.YearTo > 0 {
		conds = append(conds, alias+".year<=? AND "+alias+".year>0")
		args = append(args, f.YearTo)
	}
	if f.MinRating > 0 {
		conds = append(conds, alias+".rating>=?")
		args = append(args, f.MinRating)
	}
	if f.MaxRuntime > 0 {
		conds = append(conds, alias+".runtime_sec>0 AND "+alias+".runtime_sec<=?")
		args = append(args, int64(f.MaxRuntime.Seconds()))
	}
	if f.MinRuntime > 0 {
		conds = append(conds, alias+".runtime_sec>=?")
		args = append(args, int64(f.MinRuntime.Seconds()))
	}
	if fc := strings.TrimSpace(f.FirstCharacter); fc != "" {
		switch {
		case fc == "#" || fc == "0":
			conds = append(conds, alias+".sort_title<>'' AND "+alias+".sort_title < 'a'")
		case fc == "~" || fc == "…":
			conds = append(conds, alias+".sort_title >= '{'")
		case len(fc) == 1 && ((fc[0] >= 'a' && fc[0] <= 'z') || (fc[0] >= 'A' && fc[0] <= 'Z')):
			lo := strings.ToLower(fc)
			hi := string(lo[0] + 1)
			conds = append(conds, alias+".sort_title >= ? AND "+alias+".sort_title < ?")
			args = append(args, lo, hi)
		}
	}
	if len(f.Years) > 0 {
		conds = append(conds, alias+".year IN ("+placeholders(len(f.Years))+")")
		for _, y := range f.Years {
			args = append(args, y)
		}
	}
	if len(f.ExcludeYears) > 0 {
		conds = append(conds, alias+".year>0 AND "+alias+".year NOT IN ("+placeholders(len(f.ExcludeYears))+")")
		for _, y := range f.ExcludeYears {
			args = append(args, y)
		}
	}
	if f.MaxRating > 0 {
		conds = append(conds, alias+".rating>0 AND "+alias+".rating<=?")
		args = append(args, f.MaxRating)
	}
	if len(f.ExcludeRatings) > 0 {
		conds = append(conds, alias+".rating>0 AND "+alias+".rating NOT IN ("+placeholders(len(f.ExcludeRatings))+")")
		for _, r := range f.ExcludeRatings {
			args = append(args, r)
		}
	}
	if len(f.ExcludeRuntimes) > 0 {
		conds = append(conds, alias+".runtime_sec>0 AND "+alias+".runtime_sec NOT IN ("+placeholders(len(f.ExcludeRuntimes))+")")
		for _, d := range f.ExcludeRuntimes {
			args = append(args, int64(d.Seconds()))
		}
	}
	if f.Query != "" {
		match, matchArgs := c.titleMatch(alias+".", f.Query)
		conds = append(conds, match)
		args = append(args, matchArgs...)
	}
	for _, tf := range f.TitleFilters {
		val := strings.ToLower(tf.Value)
		switch tf.Op {
		case "==":
			conds = append(conds, "LOWER("+alias+".title)=?")
			args = append(args, val)
		case "!==":
			conds = append(conds, "LOWER("+alias+".title)<>?")
			args = append(args, val)
		case "!=":
			conds = append(conds, "LOWER("+alias+".title) NOT LIKE ? ESCAPE '\\'")
			args = append(args, "%"+escapeLike(val)+"%")
		case "<=":
			conds = append(conds, "LOWER("+alias+".title) LIKE ? ESCAPE '\\'")
			args = append(args, escapeLike(val)+"%")
		case ">=":
			conds = append(conds, "LOWER("+alias+".title) LIKE ? ESCAPE '\\'")
			args = append(args, "%"+escapeLike(val))
		default:
			match, matchArgs := c.titleMatch(alias+".", tf.Value)
			conds = append(conds, match)
			args = append(args, matchArgs...)
		}
	}
	for _, rf := range f.ReleasedFilters {
		const day = 86400
		switch rf.Op {
		case "<<":
			conds = append(conds, alias+".released_ts<?")
			args = append(args, rf.Unix)
		case "<=":
			end := rf.Unix
			if rf.SpanDay {
				end += day - 1
			}
			conds = append(conds, alias+".released_ts<=?")
			args = append(args, end)
		case ">>":
			conds = append(conds, alias+".released_ts>?")
			args = append(args, rf.Unix)
		case ">=":
			conds = append(conds, alias+".released_ts>=?")
			args = append(args, rf.Unix)
		case "!=", "!==":
			if rf.SpanDay {
				conds = append(conds, "("+alias+".released_ts<? OR "+alias+".released_ts>=?)")
				args = append(args, rf.Unix, rf.Unix+day)
			} else {
				conds = append(conds, alias+".released_ts<>?")
				args = append(args, rf.Unix)
			}
		default:
			if rf.SpanDay {
				conds = append(conds, alias+".released_ts>=? AND "+alias+".released_ts<?")
				args = append(args, rf.Unix, rf.Unix+day)
			} else {
				conds = append(conds, alias+".released_ts=?")
				args = append(args, rf.Unix)
			}
		}
	}
	return conds, args
}

// storedEpisodesWhere selects stored, aired, non-adult episodes (e) whose
// series (t) passes the browse filter f.
func (c *Catalog) storedEpisodesWhere(f core.Filter) (string, []any, error) {
	show, childExclude := splitStoredFilter(f, core.KindEpisode)
	where, args, err := c.browseWhere(show)
	if err != nil {
		return "", nil, err
	}
	conds := []string{
		"e.type='episode'",
		"e.adult=0",
		"e.release_known=1",
		"e.released_ts<=?",
	}
	outArgs := []any{c.now().Unix()}
	cConds, cArgs := c.childFilterConds("e", f, childExclude)
	conds = append(conds, cConds...)
	outArgs = append(outArgs, cArgs...)
	conds = append(conds, where)
	outArgs = append(outArgs, args...)
	return strings.Join(conds, " AND "), outArgs, nil
}

// StoredEpisodesByKey returns up to limit stored, aired, non-adult episodes
// after the key after, in key order (series, season, episode), of the
// series the browse filter show admits. It reads the key range and probes
// each episode's series, so a page costs the same however large the
// library; it refreshes nothing.
func (c *Catalog) StoredEpisodesByKey(ctx context.Context, show core.Filter, after core.RatingKey, limit int) ([]core.Title, error) {
	where, args, err := c.storedEpisodesWhere(show)
	if err != nil {
		return nil, err
	}
	lo := max(int64(after), episodeKeyStart)
	q := `SELECT ` + prefixedCols("e") + ` FROM catalog_titles e JOIN catalog_titles t ON t.key=e.show_key WHERE e.key>? AND e.key<? AND ` + where + ` ORDER BY e.key LIMIT ?`
	return c.queryTitles(ctx, q, append(append([]any{lo, episodeKeyEnd}, args...), limit)...)
}

// CountStoredEpisodes counts StoredEpisodesByKey's whole set. Like a browse
// total, a large count is served from the last one while it recounts.
func (c *Catalog) CountStoredEpisodes(ctx context.Context, show core.Filter) (int, error) {
	where, args, err := c.storedEpisodesWhere(show)
	if err != nil {
		return 0, err
	}
	ckey := fmt.Sprintf("stored-episodes:%+v", show)
	gen, minute := c.writeGen.Load(), c.minuteBucket()
	c.cacheMu.RLock()
	ent, ok := c.countCache[ckey]
	c.cacheMu.RUnlock()
	count := func(ctx context.Context) (int, error) {
		var total int
		if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM catalog_titles e JOIN catalog_titles t ON t.key=e.show_key WHERE e.key>? AND e.key<? AND `+where,
			append([]any{episodeKeyStart, episodeKeyEnd}, args...)...).Scan(&total); err != nil {
			return 0, err
		}
		c.cacheMu.Lock()
		if len(c.countCache) >= 1024 {
			c.countCache = map[string]cachedCount{}
		}
		c.countCache[ckey] = cachedCount{total: total, gen: gen, minute: minute, counted: time.Now()}
		c.cacheMu.Unlock()
		return total, nil
	}
	switch {
	case ok && ent.gen == gen && ent.minute == minute:
		return ent.total, nil
	case ok && ent.total >= staleCountMin:
		if time.Since(ent.counted) >= recountEvery {
			_ = c.hydrate(ctx, "count:"+ckey, 0, func(ctx context.Context) error {
				_, err := count(ctx)
				return err
			})
		}
		return ent.total, nil
	}
	return count(ctx)
}

// EpisodesUnranked reports that no stored episode has a popularity, so a
// popularity order of episodes is their key order. TMDB gives episodes no
// popularity; this checks the index's two ends rather than assuming it.
func (c *Catalog) EpisodesUnranked(ctx context.Context) (bool, error) {
	// Separate MIN and MAX subqueries, so each is one index seek.
	var ranked bool
	err := c.db.QueryRowContext(ctx, `SELECT COALESCE((SELECT MIN(popularity) FROM catalog_titles WHERE type='episode' AND browsable=0),0)<>0
		OR COALESCE((SELECT MAX(popularity) FROM catalog_titles WHERE type='episode' AND browsable=0),0)<>0
		OR COALESCE((SELECT MIN(popularity) FROM catalog_titles WHERE type='episode' AND browsable=1),0)<>0
		OR COALESCE((SELECT MAX(popularity) FROM catalog_titles WHERE type='episode' AND browsable=1),0)<>0`).Scan(&ranked)
	return !ranked, err
}

// BrowseStoredEpisodes pages stored, aired episodes in the library without
// refreshing shows or seasons from TMDB. Sequential key-ordered pages advance
// via a cached key cursor so deep offsets stay O(limit).
func (c *Catalog) BrowseStoredEpisodes(ctx context.Context, q core.BrowseQuery) (core.Page, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 1000 {
		limit = 1000
	}
	offset := max(q.Offset, 0)
	gen, minBucket := c.writeGen.Load(), c.minuteBucket()
	ckey := fmt.Sprintf("ep:%+v", q.Filter)
	bkey := fmt.Sprintf("%s|%s|%v|%v|%d|%d|%v", ckey, q.Sort, q.Desc, q.SecondarySorts, offset, limit, q.NoTotal)
	cursorPrefix := fmt.Sprintf("cursor:%s|%s|%v|%v|", ckey, q.Sort, q.Desc, q.SecondarySorts)

	c.cacheMu.RLock()
	if ent, ok := c.browseCache[bkey]; ok && ent.gen == gen && ent.minute == minBucket {
		items := make([]core.Title, len(ent.page.Items))
		for i := range ent.page.Items {
			items[i] = cloneTitle(ent.page.Items[i])
		}
		c.cacheMu.RUnlock()
		return core.Page{Items: items, Total: ent.page.Total}, nil
	}
	var afterKey core.RatingKey
	if offset > 0 {
		if cur, ok := c.browseCache[cursorPrefix+strconv.Itoa(offset)]; ok && cur.gen == gen && cur.minute == minBucket && len(cur.page.Items) == 1 {
			afterKey = cur.page.Items[0].Key
		}
	}
	c.cacheMu.RUnlock()

	var total int
	var err error
	if q.NoTotal {
		total = offset + limit
	} else {
		total, err = c.CountStoredEpisodes(ctx, q.Filter)
		if err != nil {
			return core.Page{}, err
		}
		if total == 0 || offset >= total {
			return core.Page{Items: []core.Title{}, Total: total}, nil
		}
	}

	keyOrder := len(q.SecondarySorts) == 0 && (q.Sort == "" || (q.Sort == core.SortPopularity && !q.Desc))
	if !keyOrder && len(q.SecondarySorts) == 0 && q.Sort == core.SortPopularity {
		if unranked, err := c.EpisodesUnranked(ctx); err == nil && unranked {
			keyOrder = true
		}
	}

	var items []core.Title
	switch {
	case keyOrder && (offset == 0 || afterKey > 0):
		items, err = c.StoredEpisodesByKey(ctx, q.Filter, afterKey, limit)
	case keyOrder:
		where, args, werr := c.storedEpisodesWhere(q.Filter)
		if werr != nil {
			return core.Page{}, werr
		}
		sqlQ := `SELECT ` + prefixedCols("e") + ` FROM catalog_titles e JOIN catalog_titles t ON t.key=e.show_key WHERE e.key>? AND e.key<? AND ` + where + ` ORDER BY e.key ASC LIMIT ? OFFSET ?`
		items, err = c.queryTitles(ctx, sqlQ, append(append([]any{episodeKeyStart, episodeKeyEnd}, args...), limit, offset)...)
	default:
		where, args, werr := c.storedEpisodesWhere(q.Filter)
		if werr != nil {
			return core.Page{}, werr
		}
		if q.Sort == core.SortUpdated && q.Filter.UpdatedAfter == 0 {
			where = "e.fetched_at>=? AND " + where
			args = append([]any{int64(0)}, args...)
		}
		order := storedChildOrder("e", q)
		sqlQ := `SELECT ` + prefixedCols("e") + ` FROM catalog_titles e JOIN catalog_titles t ON t.key=e.show_key WHERE ` + where + ` ORDER BY ` + order + `, e.key ASC LIMIT ? OFFSET ?`
		items, err = c.queryTitles(ctx, sqlQ, append(append([]any{}, args...), limit, offset)...)
	}
	if err != nil {
		return core.Page{}, err
	}
	if items == nil {
		items = []core.Title{}
	}
	if q.NoTotal {
		total = offset + len(items)
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
		if keyOrder && len(items) > 0 {
			last := items[len(items)-1].Key
			c.browseCache[cursorPrefix+strconv.Itoa(offset+len(items))] = cachedBrowse{
				page: core.Page{Items: []core.Title{{Key: last}}}, gen: gen, minute: minBucket,
			}
		}
		c.cacheMu.Unlock()
	}
	return core.Page{Items: items, Total: total}, nil
}

func storedChildOrder(alias string, q core.BrowseQuery) string {
	col := alias + ".key"
	switch q.Sort {
	case core.SortTitle:
		col = alias + ".sort_title"
	case core.SortReleased, core.SortAdded:
		col = alias + ".released_ts"
	case core.SortYear:
		col = alias + ".year"
	case core.SortRating:
		col = alias + ".rating"
	case core.SortDuration:
		col = alias + ".runtime_sec"
	case core.SortUpdated:
		col = alias + ".fetched_at"
	case core.SortPopularity:
		col = alias + ".popularity"
	}
	dir := "ASC"
	if q.Desc {
		dir = "DESC"
	}
	return col + " " + dir + browseSecondaryOrder(alias, q.SecondarySorts)
}

// storedSeasonsWhere selects stored, non-adult seasons (s) whose series (t)
// passes the browse filter f. Regular seasons require a known past air date
// and positive stored child count; Specials (season 0) may derive their
// premiere from aired special episodes during hydration.
func (c *Catalog) storedSeasonsWhere(f core.Filter, requireKnownRelease bool) (string, []any, error) {
	show, childExclude := splitStoredFilter(f, core.KindSeason)
	where, args, err := c.browseWhere(show)
	if err != nil {
		return "", nil, err
	}
	nowTS := c.now().Unix()
	var conds []string
	var outArgs []any
	if requireKnownRelease {
		conds = []string{
			"s.type='season'",
			"s.adult=0",
			"s.release_known=1",
			"s.released_ts<=?",
		}
		outArgs = []any{nowTS}
	} else {
		conds = []string{
			"s.type='season'",
			"s.adult=0",
			"((s.release_known=1 AND s.released_ts<=?) OR s.season=0)",
		}
		outArgs = []any{nowTS}
	}
	cConds, cArgs := c.childFilterConds("s", f, childExclude)
	conds = append(conds, cConds...)
	outArgs = append(outArgs, cArgs...)
	conds = append(conds, where)
	outArgs = append(outArgs, args...)
	return strings.Join(conds, " AND "), outArgs, nil
}

// StoredSeasonsByKey returns up to limit stored, listable seasons after the
// key after, in key order (series, season), of the series the browse filter
// show admits. It never refreshes shows or seasons from TMDB.
func (c *Catalog) StoredSeasonsByKey(ctx context.Context, show core.Filter, after core.RatingKey, limit int) ([]core.Title, error) {
	where, args, err := c.storedSeasonsWhere(show, false)
	if err != nil {
		return nil, err
	}
	lo := max(int64(after), seasonKeyStart)
	q := `SELECT ` + prefixedCols("s") + ` FROM catalog_titles s JOIN catalog_titles t ON t.key=s.show_key WHERE s.key>? AND s.key<? AND ` + where + ` ORDER BY s.key ASC LIMIT ?`
	now := c.now()
	var out []core.Title
	for len(out) < limit {
		need := limit - len(out)
		batch, err := c.queryTitles(ctx, q, append(append([]any{lo, seasonKeyEnd}, args...), need)...)
		if err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			break
		}
		for i := range batch {
			if listableSeason(&batch[i], now) {
				out = append(out, batch[i])
			}
		}
		nextLo := int64(batch[len(batch)-1].Key)
		if len(batch) < need || nextLo <= lo {
			break
		}
		lo = nextLo
	}
	if out == nil {
		out = []core.Title{}
	}
	return out, nil
}

// CountStoredSeasons counts the stored, listable seasons in the library for
// the browse filter show.
func (c *Catalog) CountStoredSeasons(ctx context.Context, show core.Filter) (int, error) {
	where, args, err := c.storedSeasonsWhere(show, false)
	if err != nil {
		return 0, err
	}
	ckey := fmt.Sprintf("stored-seasons:%+v", show)
	gen, minute := c.writeGen.Load(), c.minuteBucket()
	c.cacheMu.RLock()
	ent, ok := c.countCache[ckey]
	c.cacheMu.RUnlock()
	count := func(ctx context.Context) (int, error) {
		var total int
		if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM catalog_titles s JOIN catalog_titles t ON t.key=s.show_key WHERE s.key>? AND s.key<? AND `+where,
			append([]any{seasonKeyStart, seasonKeyEnd}, args...)...).Scan(&total); err != nil {
			return 0, err
		}
		// For small libraries, refine the SQL count through listableSeason so
		// seasons whose mirrored episodes are all undated are excluded just as
		// in Children(show) and StoredSeasonsByKey.
		if total > 0 && total <= 500 {
			if exact, err := c.StoredSeasonsByKey(ctx, show, 0, total+1); err == nil {
				total = len(exact)
			}
		}
		c.cacheMu.Lock()
		if len(c.countCache) >= 1024 {
			c.countCache = map[string]cachedCount{}
		}
		c.countCache[ckey] = cachedCount{total: total, gen: gen, minute: minute, counted: time.Now()}
		c.cacheMu.Unlock()
		return total, nil
	}
	switch {
	case ok && ent.gen == gen && ent.minute == minute:
		return ent.total, nil
	case ok && ent.total >= staleCountMin:
		if time.Since(ent.counted) >= recountEvery {
			_ = c.hydrate(ctx, "count:"+ckey, 0, func(ctx context.Context) error {
				_, err := count(ctx)
				return err
			})
		}
		return ent.total, nil
	}
	return count(ctx)
}

// BrowseStoredSeasons pages stored, listable seasons in the library without
// refreshing shows or seasons from TMDB.
func (c *Catalog) BrowseStoredSeasons(ctx context.Context, q core.BrowseQuery) (core.Page, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 1000 {
		limit = 1000
	}
	offset := max(q.Offset, 0)
	gen, minBucket := c.writeGen.Load(), c.minuteBucket()
	ckey := fmt.Sprintf("sn:%+v", q.Filter)
	bkey := fmt.Sprintf("%s|%s|%v|%v|%d|%d|%v", ckey, q.Sort, q.Desc, q.SecondarySorts, offset, limit, q.NoTotal)
	cursorPrefix := fmt.Sprintf("cursor:%s|%s|%v|%v|", ckey, q.Sort, q.Desc, q.SecondarySorts)

	c.cacheMu.RLock()
	if ent, ok := c.browseCache[bkey]; ok && ent.gen == gen && ent.minute == minBucket {
		items := make([]core.Title, len(ent.page.Items))
		for i := range ent.page.Items {
			items[i] = cloneTitle(ent.page.Items[i])
		}
		c.cacheMu.RUnlock()
		return core.Page{Items: items, Total: ent.page.Total}, nil
	}
	var afterKey core.RatingKey
	if offset > 0 {
		if cur, ok := c.browseCache[cursorPrefix+strconv.Itoa(offset)]; ok && cur.gen == gen && cur.minute == minBucket && len(cur.page.Items) == 1 {
			afterKey = cur.page.Items[0].Key
		}
	}
	c.cacheMu.RUnlock()

	var total int
	var err error
	if q.NoTotal {
		total = offset + limit
	} else {
		total, err = c.CountStoredSeasons(ctx, q.Filter)
		if err != nil {
			return core.Page{}, err
		}
		if total == 0 || offset >= total {
			return core.Page{Items: []core.Title{}, Total: total}, nil
		}
	}

	keyOrder := len(q.SecondarySorts) == 0 && (q.Sort == "" || (q.Sort == core.SortPopularity && !q.Desc))
	now := c.now()
	var items []core.Title
	switch {
	case keyOrder && (offset == 0 || afterKey > 0):
		items, err = c.StoredSeasonsByKey(ctx, q.Filter, afterKey, limit)
	case keyOrder && total <= 500:
		all, aerr := c.StoredSeasonsByKey(ctx, q.Filter, 0, offset+limit)
		if aerr != nil {
			return core.Page{}, aerr
		}
		if offset < len(all) {
			items = all[offset:min(offset+limit, len(all))]
		}
	case keyOrder:
		where, args, werr := c.storedSeasonsWhere(q.Filter, false)
		if werr != nil {
			return core.Page{}, werr
		}
		sqlQ := `SELECT ` + prefixedCols("s") + ` FROM catalog_titles s JOIN catalog_titles t ON t.key=s.show_key WHERE s.key>? AND s.key<? AND ` + where + ` ORDER BY s.key ASC LIMIT ? OFFSET ?`
		raw, qerr := c.queryTitles(ctx, sqlQ, append(append([]any{seasonKeyStart, seasonKeyEnd}, args...), limit, offset)...)
		if qerr != nil {
			return core.Page{}, qerr
		}
		for i := range raw {
			if listableSeason(&raw[i], now) {
				items = append(items, raw[i])
			}
		}
	default:
		requireKnown := q.Sort == core.SortReleased || q.Sort == core.SortAdded
		where, args, werr := c.storedSeasonsWhere(q.Filter, requireKnown)
		if werr != nil {
			return core.Page{}, werr
		}
		if q.Sort == core.SortUpdated && q.Filter.UpdatedAfter == 0 {
			where = "s.fetched_at>=? AND " + where
			args = append([]any{int64(0)}, args...)
		}
		order := storedChildOrder("s", q)
		sqlQ := `SELECT ` + prefixedCols("s") + ` FROM catalog_titles s JOIN catalog_titles t ON t.key=s.show_key WHERE ` + where + ` ORDER BY ` + order + `, s.key ASC LIMIT ? OFFSET ?`
		raw, qerr := c.queryTitles(ctx, sqlQ, append(append([]any{}, args...), limit, offset)...)
		if qerr != nil {
			return core.Page{}, qerr
		}
		for i := range raw {
			if listableSeason(&raw[i], now) {
				items = append(items, raw[i])
			}
		}
	}
	if err != nil {
		return core.Page{}, err
	}
	if items == nil {
		items = []core.Title{}
	}
	if q.NoTotal {
		total = offset + len(items)
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
		if keyOrder && len(items) > 0 {
			last := items[len(items)-1].Key
			c.browseCache[cursorPrefix+strconv.Itoa(offset+len(items))] = cachedBrowse{
				page: core.Page{Items: []core.Title{{Key: last}}}, gen: gen, minute: minBucket,
			}
		}
		c.cacheMu.Unlock()
	}
	return core.Page{Items: items, Total: total}, nil
}
