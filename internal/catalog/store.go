package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/tmdb"
)

const titleCols = `key,type,tmdb_id,imdb,tvdb,show_key,show_tmdb,show_title,season_title,season,episode,
title,original_title,sort_title,tagline,summary,year,released_ts,runtime_sec,status,language,
content_rating,rating,vote_count,popularity,poster,backdrop,still,logo,collection_id,collection_name,
child_count,leaf_count,browsable,added_at,fetched_at,detail,adult,last_air_ts`

type storedTitle struct {
	t         core.Title
	browsable bool
	addedAt   time.Time
	fetchedAt time.Time
	detail    bool
	// detailAt is when the detail payload was last fetched (zero if never).
	detailAt time.Time
}

// extraScan appends trailing columns to a titleCols scan.
type extraScan struct {
	s     scanner
	extra []any
}

func (e extraScan) Scan(dest ...any) error { return e.s.Scan(append(dest, e.extra...)...) }

func cloneTitle(t core.Title) core.Title {
	t.Genres = append([]string(nil), t.Genres...)
	t.Keywords = append([]string(nil), t.Keywords...)
	t.Studios = append([]string(nil), t.Studios...)
	t.Countries = append([]string(nil), t.Countries...)
	t.Cast = append([]core.Person(nil), t.Cast...)
	t.Directors = append([]core.Person(nil), t.Directors...)
	t.Writers = append([]core.Person(nil), t.Writers...)
	t.Crew = append([]core.Person(nil), t.Crew...)
	return t
}

func cacheStoredTitle(kind core.KeyKind) bool {
	switch kind {
	case core.KindMovie, core.KindShow, core.KindSeason, core.KindEpisode:
		return true
	default:
		return false
	}
}

func (c *Catalog) minuteBucket() int64 {
	return c.now().Unix() / 60
}

func (c *Catalog) loadByKey(ctx context.Context, key core.RatingKey) (*storedTitle, error) {
	gen := c.writeGen.Load()
	minBucket := c.minuteBucket()
	// Movies, shows, seasons and episodes are read again while a list builds
	// image grants and versions. The cache is generation-scoped, same as movies.
	if cacheStoredTitle(key.Kind()) {
		c.cacheMu.RLock()
		ent, ok := c.titleCache[key]
		c.cacheMu.RUnlock()
		if ok && ent.gen == gen && ent.minute == minBucket {
			cp := ent.st
			cp.t = cloneTitle(ent.st.t)
			return &cp, nil
		}
	}
	row := c.db.QueryRowContext(ctx, `SELECT `+titleCols+`,detail_at FROM catalog_titles WHERE key=?`, int64(key))
	var detailAt int64
	st, err := scanTitle(extraScan{row, []any{&detailAt}})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, core.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if detailAt != 0 {
		st.detailAt = time.Unix(detailAt, 0).UTC()
	}
	items := []core.Title{st.t}
	if err := c.hydrateSlice(ctx, items); err != nil {
		return nil, err
	}
	st.t = items[0]
	if cacheStoredTitle(key.Kind()) && c.writeGen.Load() == gen {
		c.cacheMu.Lock()
		if len(c.titleCache) >= 8192 {
			c.titleCache = map[core.RatingKey]cachedStoredTitle{}
		}
		cp := *st
		cp.t = cloneTitle(st.t)
		c.titleCache[key] = cachedStoredTitle{st: cp, gen: gen, minute: minBucket}
		c.cacheMu.Unlock()
	}
	return st, nil
}

type scanner interface{ Scan(dest ...any) error }

func scanTitle(s scanner) (*storedTitle, error) {
	var st storedTitle
	var key, showKey int64
	var typ, imdb, showTitle, seasonTitle, title, orig, sortT, tagline, summary, status, lang, crating string
	var tmdbID, tvdb, showTMDB int64
	var season, episode, year, voteCount, collID, child, leaf, browsable, detail, adult int
	var releasedTS, runtimeSec, addedAt, fetchedAt, lastAirTS int64
	var rating, pop float64
	var collName, poster, backdrop, still, logo string
	err := s.Scan(&key, &typ, &tmdbID, &imdb, &tvdb, &showKey, &showTMDB, &showTitle, &seasonTitle, &season, &episode,
		&title, &orig, &sortT, &tagline, &summary, &year, &releasedTS, &runtimeSec, &status, &lang,
		&crating, &rating, &voteCount, &pop, &poster, &backdrop, &still, &logo, &collID, &collName,
		&child, &leaf, &browsable, &addedAt, &fetchedAt, &detail, &adult, &lastAirTS)
	if err != nil {
		return nil, err
	}
	t := core.Title{
		Key: core.RatingKey(key), Type: core.MediaType(typ),
		IDs:     core.IDs{TMDB: tmdbID, IMDB: imdb, TVDB: tvdb},
		ShowKey: core.RatingKey(showKey), ShowTitle: showTitle, SeasonTitle: seasonTitle,
		Season: season, Episode: episode, Title: title, OriginalTitle: orig, SortTitle: sortT,
		Tagline: tagline, Summary: summary, Year: year, Runtime: time.Duration(runtimeSec) * time.Second,
		Status: status, Language: lang, ContentRating: crating, Adult: adult != 0, Rating: rating, VoteCount: voteCount,
		Popularity: pop, PosterPath: poster, BackdropPath: backdrop, StillPath: still, LogoPath: logo,
		CollectionID: int64(collID), CollectionName: collName, ChildCount: child, LeafCount: leaf,
	}
	if showTMDB != 0 {
		t.ShowIDs.TMDB = showTMDB
	}
	if releasedTS != 0 {
		t.Released = time.Unix(releasedTS, 0).UTC()
	}
	if lastAirTS != 0 {
		t.LastAirDate = time.Unix(lastAirTS, 0).UTC()
	}
	if fetchedAt != 0 {
		t.UpdatedAt = time.Unix(fetchedAt, 0).UTC()
	}
	st.t = t
	st.browsable = browsable != 0
	st.detail = detail != 0
	if addedAt != 0 {
		st.addedAt = time.Unix(addedAt, 0).UTC()
	}
	if fetchedAt != 0 {
		st.fetchedAt = time.Unix(fetchedAt, 0).UTC()
	}
	// Fill ShowIDs imdb/tvdb lazily from the show row when needed by caller.
	return &st, nil
}

// hydrateSlice fills Genres/Studios/Countries/Cast/Directors/Writers/ShowIDs in place.
func (c *Catalog) hydrateSlice(ctx context.Context, items []core.Title) error {
	if len(items) == 0 {
		return nil
	}
	if err := c.airedCounts(ctx, items); err != nil {
		return err
	}
	// Same shape as hydrateHomeRelease: query once per distinct key, but
	// apply each row to EVERY occurrence of that key in the slice. A title
	// can appear on several shelves (in different chunks, too); mapping a
	// key to a single index would leave the other copies unhydrated, and
	// querying per occurrence would hydrate a copy once per chunk it
	// appears in.
	idx := map[core.RatingKey][]int{}
	keys := make([]any, 0, len(items))
	for i := range items {
		if _, ok := idx[items[i].Key]; !ok {
			keys = append(keys, int64(items[i].Key))
		}
		idx[items[i].Key] = append(idx[items[i].Key], i)
	}
	for i := range items {
		items[i].HomeReleaseChecked = false
		items[i].HomeReleased = time.Time{}
		items[i].HomeReleaseFallbackAdmitted = false
	}
	// Bound SQLite parameters independently of the caller's page size
	// (same 200 as titlesByKeys). One statement covers genres, keywords,
	// studios, countries, credits and home-release dates.
	const batch = 200
	for start := 0; start < len(keys); start += batch {
		if err := c.hydrateFactBatch(ctx, items, idx, keys[start:min(start+batch, len(keys))]); err != nil {
			return err
		}
	}
	if err := c.hydrateShows(ctx, items); err != nil {
		return err
	}
	return c.applyHydratedAdded(ctx, items)
}

type seriesIdentity struct {
	imdb, original string
	tvdb           int64
	year           int
	keywords       []string
}

type creditFact struct {
	person core.Person
	kind   string
	ord    int64
}

func (c *Catalog) hydrateFactBatch(ctx context.Context, items []core.Title, idx map[core.RatingKey][]int, args []any) error {
	ph := placeholders(len(args))
	q := `SELECT key, tag, a, b, c, d, e, f, g, extra FROM (
SELECT key, 'genre' AS tag, genre AS a, '' AS b, '' AS c, '' AS d, '' AS e, '' AS f, '' AS g, '' AS extra FROM catalog_genres WHERE key IN (` + ph + `)
UNION ALL
SELECT key, 'keyword', keyword, '', '', '', '', '', '', '' FROM catalog_keywords WHERE key IN (` + ph + `)
UNION ALL
SELECT key, 'studio', studio, '', '', '', '', '', '', '' FROM catalog_studios WHERE key IN (` + ph + `)
UNION ALL
SELECT key, 'country', country, '', '', '', '', '', '', '' FROM catalog_countries WHERE key IN (` + ph + `)
UNION ALL
SELECT c.key, 'credit', c.kind, c.name, c.role, c.thumb, CAST(c.person_tmdb AS TEXT), COALESCE(p.imdb,''), CAST(c.ord AS TEXT), c.provider_id FROM catalog_credits c LEFT JOIN catalog_people p ON p.person_tmdb=c.person_tmdb WHERE c.key IN (` + ph + `)
UNION ALL
SELECT h.key, 'home', CAST(h.checked AS TEXT), CAST(h.earliest_ts AS TEXT), CAST(h.fallback_admitted AS TEXT), CAST(t.released_ts AS TEXT), CAST(t.year AS TEXT), '', '', '' FROM catalog_home_release h JOIN catalog_titles t ON t.key=h.key WHERE h.key IN (` + ph + `)
)`
	bound := make([]any, 0, len(args)*6)
	for i := 0; i < 6; i++ {
		bound = append(bound, args...)
	}
	rows, err := c.db.QueryContext(ctx, q, bound...)
	if err != nil {
		return err
	}
	defer rows.Close()
	credits := map[core.RatingKey][]creditFact{}
	touched := map[core.RatingKey]bool{}
	for rows.Next() {
		var key int64
		var tag, a, b, ccol, d, e, f, g, extra string
		if err := rows.Scan(&key, &tag, &a, &b, &ccol, &d, &e, &f, &g, &extra); err != nil {
			return err
		}
		k := core.RatingKey(key)
		touched[k] = true
		switch tag {
		case "genre":
			for _, i := range idx[k] {
				items[i].Genres = append(items[i].Genres, a)
			}
		case "keyword":
			for _, i := range idx[k] {
				items[i].Keywords = append(items[i].Keywords, a)
			}
		case "studio":
			for _, i := range idx[k] {
				items[i].Studios = append(items[i].Studios, a)
			}
		case "country":
			for _, i := range idx[k] {
				items[i].Countries = append(items[i].Countries, a)
			}
		case "credit":
			pid, _ := strconv.ParseInt(e, 10, 64)
			ord, _ := strconv.ParseInt(g, 10, 64)
			credits[k] = append(credits[k], creditFact{kind: a, ord: ord, person: core.Person{TMDB: pid, IMDB: f, ID: extra, Name: b, Role: ccol, ThumbPath: d}})
		case "home":
			checked, _ := strconv.Atoi(a)
			earliest, _ := strconv.ParseInt(b, 10, 64)
			admitted, _ := strconv.Atoi(ccol)
			released, _ := strconv.ParseInt(d, 10, 64)
			year, _ := strconv.Atoi(e)
			for _, i := range idx[k] {
				items[i].Released = time.Time{}
				if released != 0 {
					items[i].Released = time.Unix(released, 0).UTC()
				}
				items[i].Year = year
				items[i].HomeReleaseChecked = checked != 0
				items[i].HomeReleaseFallbackAdmitted = admitted != 0
				if earliest != 0 {
					items[i].HomeReleased = time.Unix(earliest, 0).UTC()
				}
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for key := range touched {
		for _, i := range idx[key] {
			sort.Strings(items[i].Genres)
			sort.Strings(items[i].Keywords)
			sort.Strings(items[i].Studios)
			sort.Strings(items[i].Countries)
		}
	}
	for key, list := range credits {
		sort.SliceStable(list, func(i, j int) bool {
			if list[i].kind != list[j].kind {
				return list[i].kind < list[j].kind
			}
			return list[i].ord < list[j].ord
		})
		for _, fact := range list {
			for _, i := range idx[key] {
				switch fact.kind {
				case "director":
					items[i].Directors = append(items[i].Directors, fact.person)
				case "writer":
					items[i].Writers = append(items[i].Writers, fact.person)
				case "crew":
					items[i].Crew = append(items[i].Crew, fact.person)
				default:
					items[i].Cast = append(items[i].Cast, fact.person)
				}
			}
		}
	}
	return nil
}

func (c *Catalog) hydrateShows(ctx context.Context, items []core.Title) error {
	shows := map[core.RatingKey]*seriesIdentity{}
	var showArgs []any
	for i := range items {
		if k := items[i].ShowKey; k != 0 && shows[k] == nil {
			shows[k] = &seriesIdentity{}
			showArgs = append(showArgs, int64(k))
		}
		if items[i].Type == core.TypeEpisode && items[i].Season == 1 && items[i].Episode > 0 {
			items[i].AbsoluteEpisode = items[i].Episode
		}
	}
	if len(shows) == 0 {
		return nil
	}
	counts := map[core.RatingKey]map[int]int{}
	const batch = 200
	for start := 0; start < len(showArgs); start += batch {
		part := showArgs[start:min(start+batch, len(showArgs))]
		ph := placeholders(len(part))
		q := `SELECT tag, key, a, b, c, d FROM (
SELECT 'id' AS tag, key, imdb AS a, CAST(tvdb AS TEXT) AS b, CAST(year AS TEXT) AS c, original_title AS d FROM catalog_titles WHERE key IN (` + ph + `)
UNION ALL
SELECT 'kw', key, keyword, '', '', '' FROM catalog_keywords WHERE key IN (` + ph + `)
UNION ALL
SELECT 'season', show_key, CAST(season AS TEXT), CAST(child_count AS TEXT), '', '' FROM catalog_titles WHERE type='season' AND season>0 AND show_key IN (` + ph + `)
)`
		bound := make([]any, 0, len(part)*3)
		for i := 0; i < 3; i++ {
			bound = append(bound, part...)
		}
		rows, err := c.db.QueryContext(ctx, q, bound...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var key int64
			var tag, a, b, ccol, d string
			if err := rows.Scan(&tag, &key, &a, &b, &ccol, &d); err != nil {
				rows.Close()
				return err
			}
			k := core.RatingKey(key)
			switch tag {
			case "id":
				if show := shows[k]; show != nil {
					tvdb, _ := strconv.ParseInt(b, 10, 64)
					year, _ := strconv.Atoi(ccol)
					show.imdb, show.tvdb, show.year, show.original = a, tvdb, year, d
				}
			case "kw":
				if show := shows[k]; show != nil {
					show.keywords = append(show.keywords, a)
				}
			case "season":
				season, _ := strconv.Atoi(a)
				n, _ := strconv.Atoi(b)
				if counts[k] == nil {
					counts[k] = map[int]int{}
				}
				counts[k][season] = n
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	for show := range shows {
		sort.Strings(shows[show].keywords)
	}
	for i := range items {
		show := shows[items[i].ShowKey]
		if show == nil {
			continue
		}
		if len(items[i].Keywords) == 0 && len(show.keywords) > 0 && (items[i].Type == core.TypeSeason || items[i].Type == core.TypeEpisode) {
			items[i].Keywords = append([]string(nil), show.keywords...)
		}
		if items[i].ShowYear == 0 {
			items[i].ShowYear = show.year
		}
		if show.original != "" {
			items[i].ShowOriginalTitle = show.original
		}
		if items[i].ShowIDs.IMDB == "" {
			items[i].ShowIDs.IMDB = show.imdb
		}
		if items[i].ShowIDs.TVDB == 0 {
			items[i].ShowIDs.TVDB = show.tvdb
		}
		// Number episodes from the series start. An episode stays unnumbered
		// when an earlier season is missing or has no known count.
		seasons := counts[items[i].ShowKey]
		t := &items[i]
		if t.Type != core.TypeEpisode || t.Season <= 1 || t.Episode <= 0 || seasons == nil {
			continue
		}
		total := 0
		for s := 1; s < t.Season; s++ {
			if seasons[s] <= 0 {
				total = -1
				break
			}
			total += seasons[s]
		}
		if total >= 0 {
			t.AbsoluteEpisode = total + t.Episode
		}
	}
	return nil
}

func (c *Catalog) applyHydratedAdded(ctx context.Context, items []core.Title) error {
	now := c.now()
	needsAir := false
	for i := range items {
		switch items[i].Type {
		case core.TypeShow, core.TypeSeason:
			needsAir = true
		default:
			if !items[i].Released.IsZero() && !items[i].Released.After(now) {
				items[i].AddedAt = items[i].Released
			}
		}
	}
	if !needsAir {
		return nil
	}
	return applyAirAddedTimes(ctx, c.db, items, now)
}

// scanPairs applies each (key, value) row to every slice index registered
// for that key in idx, so duplicated titles are hydrated exactly once per
// occurrence.
func (c *Catalog) scanPairs(ctx context.Context, q string, args []any, apply func(i int, v string), idx map[core.RatingKey][]int) error {
	rows, err := c.db.QueryContext(ctx, q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var k int64
		var v string
		if err := rows.Scan(&k, &v); err != nil {
			return err
		}
		for _, i := range idx[core.RatingKey(k)] {
			apply(i, v)
		}
	}
	return rows.Err()
}

func keyArgs(items []core.Title) []any {
	args := make([]any, len(items))
	for i := range items {
		args[i] = int64(items[i].Key)
	}
	return args
}
func placeholders(n int) string {
	if n <= 0 {
		return "NULL"
	}
	s := "?"
	for i := 1; i < n; i++ {
		s += ",?"
	}
	return s
}

type upsertOpts struct {
	browsable bool // request browsable (added_at set if newly browsable)
	detail    bool
	// airDates: the payload is authoritative for the title's air date even
	// though it is not a detail fetch. Season and episode air dates come
	// only from TMDB's show and season payloads, so a date that is added,
	// moved or withdrawn must replace the stored one, including on rows an
	// earlier detail upsert marked (which otherwise keep their date).
	airDates     bool
	releaseDates *tmdb.ReleaseDates
	gateEnabled  bool
}

// UpsertTitle stores a title (exported for tests/seeders and other components that
// need to insert known titles). Browsable/added_at are preserved once set.
// The row is not a fresh detail record, so a later Get may refresh it.
func (c *Catalog) UpsertTitle(ctx context.Context, t core.Title, browsable bool) error {
	return c.upsert(ctx, t, upsertOpts{browsable: browsable})
}

// UpsertDetailedTitle stores a title as a fresh detail record. A later Get
// serves the row without a metadata refresh until it is stale. The scorecard
// fixture uses this: its seed is the source of truth, and a failed refresh
// must not be repeated for every library row.
func (c *Catalog) UpsertDetailedTitle(ctx context.Context, t core.Title) error {
	return c.upsert(ctx, t, upsertOpts{browsable: true, detail: true})
}

func (c *Catalog) upsert(ctx context.Context, t core.Title, o upsertOpts) error {
	return c.upsertMany(ctx, []core.Title{t}, o)
}

func (c *Catalog) upsertMany(ctx context.Context, titles []core.Title, o upsertOpts) error {
	if len(titles) == 0 {
		return nil
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := c.now().Unix()
	for _, t := range titles {
		if err := upsertOne(ctx, tx, t, o, now); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	c.writeGen.Add(1)
	return nil
}

func upsertOne(ctx context.Context, tx *sql.Tx, t core.Title, o upsertOpts, now int64) error {
	if t.Key == 0 {
		return fmt.Errorf("catalog: upsert title with zero key")
	}
	if t.SortTitle == "" {
		t.SortTitle = sortTitle(t.Title)
	}
	showTMDB := t.ShowIDs.TMDB
	if showTMDB == 0 && t.ShowKey != 0 {
		showTMDB = t.ShowKey.TMDB()
	}
	// For episodes/seasons, IDs.TMDB is their own TMDB id. show_tmdb stored separately.
	var releasedTS int64
	var lastAirTS int64
	if !t.LastAirDate.IsZero() {
		lastAirTS = t.LastAirDate.Unix()
	}
	if !t.Released.IsZero() {
		releasedTS = t.Released.Unix()
		if t.Year == 0 {
			t.Year = t.Released.Year()
		}
	}
	browsableInt := 0
	if o.browsable {
		browsableInt = 1
	}
	detailInt := 0
	if o.detail {
		detailInt = 1
	}
	if o.releaseDates != nil && o.gateEnabled && t.Type == core.TypeMovie {
		if _, err := tx.ExecContext(ctx, `UPDATE catalog_home_release SET fallback_admitted=1 WHERE key=? AND checked=1 AND earliest_ts=0 AND EXISTS (SELECT 1 FROM catalog_titles t WHERE t.key=catalog_home_release.key AND t.released_ts<>0 AND t.released_ts<=?)`, int64(t.Key), now-int64(homeReleaseGrace.Seconds())); err != nil {
			return err
		}
	}
	// Keep richer existing fields when a summary upsert would blank them:
	// handled per-field with CASE in ON CONFLICT for a few detail-only fields.
	_, err := tx.ExecContext(ctx, `
INSERT INTO catalog_titles (key,type,tmdb_id,imdb,tvdb,show_key,show_tmdb,show_title,season_title,season,episode,
 title,original_title,sort_title,tagline,summary,year,released_ts,runtime_sec,status,language,
 content_rating,content_rating_rank,rating,vote_count,popularity,poster,backdrop,still,logo,
 collection_id,collection_name,child_count,leaf_count,browsable,added_at,fetched_at,detail,adult,detail_at,last_air_ts)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(key) DO UPDATE SET
 type=excluded.type, tmdb_id=excluded.tmdb_id, adult=excluded.adult,
 imdb=CASE WHEN excluded.imdb<>'' THEN excluded.imdb ELSE catalog_titles.imdb END,
 tvdb=CASE WHEN excluded.tvdb<>0 THEN excluded.tvdb ELSE catalog_titles.tvdb END,
 show_key=excluded.show_key, show_tmdb=excluded.show_tmdb, show_title=excluded.show_title,
 season_title=excluded.season_title, season=excluded.season, episode=excluded.episode,
 title=CASE WHEN excluded.title<>'' THEN excluded.title ELSE catalog_titles.title END,
 original_title=CASE WHEN excluded.original_title<>'' THEN excluded.original_title ELSE catalog_titles.original_title END,
 sort_title=CASE WHEN excluded.sort_title<>'' THEN excluded.sort_title ELSE catalog_titles.sort_title END,
 tagline=CASE WHEN excluded.tagline<>'' THEN excluded.tagline ELSE catalog_titles.tagline END,
 summary=CASE WHEN excluded.summary<>'' THEN excluded.summary ELSE catalog_titles.summary END,
 year=CASE WHEN excluded.detail=1 OR (catalog_titles.detail=0 AND excluded.year<>0) THEN excluded.year ELSE catalog_titles.year END,
 released_ts=CASE WHEN excluded.detail=1 OR (catalog_titles.detail=0 AND excluded.released_ts<>0) THEN excluded.released_ts ELSE catalog_titles.released_ts END,
 runtime_sec=CASE WHEN excluded.runtime_sec<>0 THEN excluded.runtime_sec ELSE catalog_titles.runtime_sec END,
 status=CASE WHEN excluded.status<>'' THEN excluded.status ELSE catalog_titles.status END,
 last_air_ts=CASE WHEN excluded.last_air_ts<>0 THEN excluded.last_air_ts ELSE catalog_titles.last_air_ts END,
 language=CASE WHEN excluded.language<>'' THEN excluded.language ELSE catalog_titles.language END,
 content_rating=CASE WHEN excluded.content_rating<>'' THEN excluded.content_rating ELSE catalog_titles.content_rating END,
 content_rating_rank=CASE WHEN excluded.content_rating<>'' THEN excluded.content_rating_rank ELSE catalog_titles.content_rating_rank END,
 rating=excluded.rating, vote_count=excluded.vote_count, popularity=excluded.popularity,
 poster=CASE WHEN excluded.poster<>'' THEN excluded.poster ELSE catalog_titles.poster END,
 backdrop=CASE WHEN excluded.backdrop<>'' THEN excluded.backdrop ELSE catalog_titles.backdrop END,
 still=CASE WHEN excluded.still<>'' THEN excluded.still ELSE catalog_titles.still END,
 logo=CASE WHEN excluded.logo<>'' THEN excluded.logo ELSE catalog_titles.logo END,
 collection_id=CASE WHEN excluded.collection_id<>0 THEN excluded.collection_id ELSE catalog_titles.collection_id END,
 collection_name=CASE WHEN excluded.collection_name<>'' THEN excluded.collection_name ELSE catalog_titles.collection_name END,
 child_count=CASE WHEN excluded.child_count<>0 THEN excluded.child_count ELSE catalog_titles.child_count END,
 leaf_count=CASE WHEN excluded.leaf_count<>0 THEN excluded.leaf_count ELSE catalog_titles.leaf_count END,
 browsable=CASE WHEN excluded.browsable>catalog_titles.browsable THEN excluded.browsable ELSE catalog_titles.browsable END,
 added_at=CASE WHEN catalog_titles.added_at<>0 THEN catalog_titles.added_at
               WHEN excluded.browsable=1 THEN excluded.added_at ELSE 0 END,
 fetched_at=excluded.fetched_at,
 detail=CASE WHEN excluded.detail>catalog_titles.detail THEN excluded.detail ELSE catalog_titles.detail END,
 detail_at=CASE WHEN excluded.detail=1 THEN excluded.detail_at ELSE catalog_titles.detail_at END`,
		int64(t.Key), string(t.Type), t.IDs.TMDB, t.IDs.IMDB, t.IDs.TVDB, int64(t.ShowKey), showTMDB, t.ShowTitle, t.SeasonTitle, t.Season, t.Episode,
		t.Title, t.OriginalTitle, t.SortTitle, t.Tagline, t.Summary, t.Year, releasedTS, int64(t.Runtime.Seconds()), t.Status, t.Language,
		t.ContentRating, RatingRank(t.ContentRating), t.Rating, t.VoteCount, t.Popularity, t.PosterPath, t.BackdropPath, t.StillPath, t.LogoPath,
		t.CollectionID, t.CollectionName, t.ChildCount, t.LeafCount, browsableInt, addedAtFor(browsableInt, now), now, detailInt, t.Adult, detailAtFor(detailInt, now), lastAirTS)
	if err != nil {
		return fmt.Errorf("catalog: upsert %d: %w", t.Key, err)
	}
	if o.releaseDates != nil && t.Type == core.TypeMovie {
		if err := saveHomeReleases(ctx, tx, t, o.releaseDates, now); err != nil {
			return err
		}
	}
	if o.airDates && !o.detail {
		if _, err := tx.ExecContext(ctx, `UPDATE catalog_titles SET released_ts=?, year=? WHERE key=?`, releasedTS, t.Year, int64(t.Key)); err != nil {
			return fmt.Errorf("catalog: upsert %d air date: %w", t.Key, err)
		}
	}
	// Replace join rows only when the incoming title carries that facet, so
	// summary upserts don't wipe detail-fetched genres/credits.
	if t.Genres != nil {
		if err := replacePairs(ctx, tx, "catalog_genres", "genre", int64(t.Key), t.Genres); err != nil {
			return err
		}
	}
	if t.Keywords != nil {
		if err := replacePairs(ctx, tx, "catalog_keywords", "keyword", int64(t.Key), t.Keywords); err != nil {
			return err
		}
	}
	if t.Studios != nil {
		if err := replacePairs(ctx, tx, "catalog_studios", "studio", int64(t.Key), t.Studios); err != nil {
			return err
		}
	}
	if t.Countries != nil {
		if err := replacePairs(ctx, tx, "catalog_countries", "country", int64(t.Key), t.Countries); err != nil {
			return err
		}
	}
	if t.Cast != nil || t.Directors != nil || t.Writers != nil || t.Crew != nil {
		if _, err := tx.ExecContext(ctx, `DELETE FROM catalog_credits WHERE key=?`, int64(t.Key)); err != nil {
			return err
		}
		insert := func(ps []core.Person, kind string) error {
			for i, p := range ps {
				if _, err := tx.ExecContext(ctx, `INSERT INTO catalog_credits(key,person_tmdb,name,role,kind,ord,thumb,provider_id) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`,
					int64(t.Key), p.TMDB, p.Name, p.Role, kind, i, p.ThumbPath, p.ID); err != nil {
					return err
				}
				if p.TMDB > 0 && validPersonIMDB(p.IMDB) {
					if _, err := tx.ExecContext(ctx, `INSERT INTO catalog_people(person_tmdb,imdb) VALUES(?,?) ON CONFLICT(person_tmdb) DO UPDATE SET imdb=excluded.imdb`, p.TMDB, p.IMDB); err != nil {
						return err
					}
				}
			}
			return nil
		}
		if err := insert(t.Cast, "cast"); err != nil {
			return err
		}
		if err := insert(t.Directors, "director"); err != nil {
			return err
		}
		if err := insert(t.Writers, "writer"); err != nil {
			return err
		}
		if err := insert(t.Crew, "crew"); err != nil {
			return err
		}
	}
	return nil
}

func detailAtFor(detail int, now int64) int64 {
	if detail != 0 {
		return now
	}
	return 0
}

func addedAtFor(browsable int, now int64) int64 {
	if browsable != 0 {
		return now
	}
	return 0
}

func replacePairs(ctx context.Context, tx *sql.Tx, table, col string, key int64, vals []string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE key=?`, key); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, v := range vals {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		if _, err := tx.ExecContext(ctx, `INSERT INTO `+table+`(key,`+col+`) VALUES(?,?) ON CONFLICT DO NOTHING`, key, v); err != nil {
			return err
		}
	}
	return nil
}

func (c *Catalog) queryTitles(ctx context.Context, q string, args ...any) ([]core.Title, error) {
	if err := c.admitFallbacks(ctx); err != nil {
		return nil, err
	}
	gen := c.writeGen.Load()
	minute := c.minuteBucket()
	rows, err := c.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.Title
	for rows.Next() {
		st, err := scanTitle(rows)
		if err != nil {
			return nil, err
		}
		// Let hydration establish an undated Specials container's premiere
		// from its aired children; all other unknown dates remain excluded.
		undatedSpecials := st.t.Type == core.TypeSeason && st.t.Key.Season() == 0 && st.t.Released.IsZero()
		if !st.t.Adult && (st.t.IsReleased(c.now()) || undatedSpecials) {
			out = append(out, st.t)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	needAt := make([]int, 0, len(out))
	for i := range out {
		if cached, ok := c.cachedStored(out[i].Key, gen, minute); ok {
			out[i] = cached
			continue
		}
		needAt = append(needAt, i)
	}
	if len(needAt) > 0 {
		need := make([]core.Title, len(needAt))
		for j, i := range needAt {
			need[j] = out[i]
		}
		if err := c.hydrateSlice(ctx, need); err != nil {
			return nil, err
		}
		for j, i := range needAt {
			out[i] = need[j]
		}
	}
	visible := out[:0]
	for _, t := range out {
		if t.IsReleased(c.now()) && (!c.config().GateMoviesByHomeRelease || homeReleaseVisible(&t, c.now())) {
			visible = append(visible, t)
		}
	}
	c.rememberTitles(gen, minute, visible)
	return visible, nil
}

func (c *Catalog) getMeta(ctx context.Context, k string) string {
	var v string
	_ = c.db.QueryRowContext(ctx, `SELECT v FROM catalog_meta WHERE k=?`, k).Scan(&v)
	return v
}
func (c *Catalog) setMeta(ctx context.Context, k, v string) error {
	_, err := c.db.ExecContext(ctx, `INSERT INTO catalog_meta(k,v) VALUES(?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, k, v)
	if err == nil {
		c.writeGen.Add(1)
	}
	return err
}
