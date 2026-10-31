package catalog

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	"time"
)

// CachedTitle returns a mirrored title without contacting TMDB.
func (c *Catalog) CachedTitle(ctx context.Context, key core.RatingKey) (*core.Title, error) {
	st, err := c.loadByKey(ctx, key)
	if err != nil {
		return nil, err
	}
	return &st.t, nil
}

// RecentEpisodes returns only local, released episode identities for artwork
// badges. Callers apply viewer state through core.State instead of a SQL join.
func (c *Catalog) RecentEpisodes(ctx context.Context, show core.RatingKey, since time.Time) ([]core.Title, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT key,episode FROM catalog_titles WHERE show_key=? AND type='episode' AND adult=0 AND released_ts BETWEEN ? AND ?`, show, since.Unix(), c.now().Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []core.Title{}
	for rows.Next() {
		var t core.Title
		t.Type = core.TypeEpisode
		if err := rows.Scan(&t.Key, &t.Episode); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// CachedTitles returns released, non-adult mirrored titles in requested order,
// without refreshing remote metadata. Batch consumers such as recommendation
// builders use this to avoid one full hydration round trip per history entry.
func (c *Catalog) CachedTitles(ctx context.Context, keys []core.RatingKey) ([]core.Title, error) {
	return c.titlesByKeys(ctx, keys)
}

// ShowTrajectories computes season/episode rating trajectories for a batch of
// shows from local mirror data without contacting TMDB.
func (c *Catalog) ShowTrajectories(ctx context.Context, shows []core.RatingKey) (map[core.RatingKey]core.ShowTrajectory, error) {
	out := make(map[core.RatingKey]core.ShowTrajectory, len(shows))
	if len(shows) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(shows))
	seen := map[core.RatingKey]bool{}
	for _, k := range shows {
		if k.Kind() == core.KindShow && !seen[k] {
			seen[k] = true
			args = append(args, int64(k))
		}
	}
	if len(args) == 0 {
		return out, nil
	}
	nowTS := c.now().Unix()
	statusByShow := map[core.RatingKey]string{}
	showRating := map[core.RatingKey]float64{}
	type sPoint struct {
		season int
		rating float64
	}
	byShow := map[core.RatingKey][]sPoint{}
	// Chunk the deduplicated show args (same 200 as hydrateSlice). Each
	// show lands in exactly one chunk, so per-show points stay ordered
	// and are never merged across queries.
	const batch = 200
	for start := 0; start < len(args); start += batch {
		part := args[start:min(start+batch, len(args))]
		ph := placeholders(len(part))
		srows, err := c.db.QueryContext(ctx, `SELECT key, status, rating FROM catalog_titles WHERE key IN (`+ph+`)`, part...)
		if err != nil {
			return nil, err
		}
		for srows.Next() {
			var k int64
			var st string
			var r float64
			if err := srows.Scan(&k, &st, &r); err != nil {
				srows.Close()
				return nil, err
			}
			statusByShow[core.RatingKey(k)] = st
			showRating[core.RatingKey(k)] = r
		}
		if err := srows.Err(); err != nil {
			srows.Close()
			return nil, err
		}
		srows.Close()

		rows, err := c.db.QueryContext(ctx, `
			SELECT show_key, season, rating FROM catalog_titles
			WHERE type='season' AND season>0 AND rating>0 AND released_ts<>0 AND released_ts<=? AND show_key IN (`+ph+`)
			ORDER BY show_key ASC, season ASC`, append([]any{nowTS}, part...)...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var sk int64
			var sea int
			var r float64
			if err := rows.Scan(&sk, &sea, &r); err != nil {
				rows.Close()
				return nil, err
			}
			byShow[core.RatingKey(sk)] = append(byShow[core.RatingKey(sk)], sPoint{season: sea, rating: r})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()

		// Fallback/augmentation from mirrored episode ratings when season ratings are 0.
		erows, err := c.db.QueryContext(ctx, `
			SELECT show_key, season, AVG(rating) FROM catalog_titles
			WHERE type='episode' AND season>0 AND rating>0 AND released_ts<>0 AND released_ts<=? AND show_key IN (`+ph+`)
			GROUP BY show_key, season
			ORDER BY show_key ASC, season ASC`, append([]any{nowTS}, part...)...)
		if err == nil {
			epByShow := map[core.RatingKey][]sPoint{}
			scanDiscarded := 0
			for erows.Next() {
				var sk int64
				var sea int
				var r float64
				if err := erows.Scan(&sk, &sea, &r); err != nil {
					scanDiscarded++
					continue
				}
				epByShow[core.RatingKey(sk)] = append(epByShow[core.RatingKey(sk)], sPoint{season: sea, rating: r})
			}
			if err := erows.Err(); err != nil {
				// Episode augmentation is optional: keep the season
				// trajectories already loaded and discard the partial
				// episode points rather than failing the whole batch.
				c.log.Warn("catalog: episode trajectories not fully loaded", "err", err)
			} else if scanDiscarded > 0 {
				// A scan error means some episode points were dropped:
				// discard the partial episode points (like an iteration
				// error) rather than merging an incomplete set.
				c.log.Warn("catalog: episode trajectories not fully loaded", "dropped", scanDiscarded)
			} else {
				for k, pts := range epByShow {
					if len(byShow[k]) < len(pts) {
						byShow[k] = pts
					}
				}
			}
			erows.Close()
		} else {
			c.log.Warn("catalog: episode trajectories not fully loaded", "err", err)
		}
	}

	for k, pts := range byShow {
		if len(pts) == 0 {
			continue
		}
		early := pts[0].rating
		finale := pts[len(pts)-1].rating
		peak := 0.0
		for _, p := range pts {
			if p.rating > peak {
				peak = p.rating
			}
		}
		base := early
		if sr := showRating[k]; sr > 0 && sr < base {
			base = sr
		}
		ended := statusByShow[k] == "Ended" || statusByShow[k] == "Canceled"
		// A show "sticks the landing" when it ended (not canceled on a cliffhanger
		// with a collapsing finale) and its final season holds or beats its early bar.
		sticks := statusByShow[k] == "Ended" && finale >= 7.5 && finale >= base-0.35
		if !ended && len(pts) >= 2 && finale >= 8.0 && finale >= early {
			// Returning multi-season series that is getting better each season.
			sticks = false
		}
		out[k] = core.ShowTrajectory{
			ShowKey:       k,
			EarlyRating:   early,
			FinaleRating:  finale,
			PeakRating:    peak,
			RatedSeasons:  len(pts),
			SticksLanding: sticks,
		}
	}
	return out, nil
}

// StandaloneEpisodes returns acclaimed mirrored episodes from anthology series
// or top-rated standalone chapters that can be watched without a multi-season
// commitment.
func (c *Catalog) StandaloneEpisodes(ctx context.Context, minRating float64, limit int) ([]core.Title, error) {
	if minRating <= 0 {
		minRating = 8.0
	}
	if limit <= 0 {
		limit = 40
	}
	nowTS := c.now().Unix()
	q := `SELECT ` + prefixedCols("e") + ` FROM catalog_titles e
		JOIN catalog_titles s ON s.key = e.show_key
		WHERE e.type='episode' AND e.season>0 AND e.adult=0
		  AND e.released_ts<>0 AND e.released_ts<=?
		  AND e.rating>=?
		  AND s.browsable=1 AND s.adult=0
		  AND (
		    EXISTS (SELECT 1 FROM catalog_keywords kw WHERE kw.key=s.key AND LOWER(kw.keyword) IN ('anthology','anthology series','stand-alone','standalone','short stories','episodic'))
		    OR (e.season=1 AND e.episode=1 AND e.rating>=8.3)
		    OR (e.rating>=8.8 AND e.vote_count>=50)
		  )
		ORDER BY e.rating DESC, s.popularity DESC, e.key ASC
		LIMIT ?`
	return c.queryTitles(ctx, q, nowTS, minRating, limit)
}

// RecentMovies reads the release-date index, without refreshing any title.
func (c *Catalog) RecentMovies(ctx context.Context, since time.Time) ([]core.Title, error) {
	return c.queryTitles(ctx, `SELECT `+titleCols+` FROM catalog_titles WHERE type='movie' AND browsable=1 AND released_ts>? AND released_ts<=? ORDER BY key`, since.Unix(), c.now().Unix())
}

// RecentShows only reads the local premiere-date index.
func (c *Catalog) RecentShows(ctx context.Context, since time.Time) ([]core.Title, error) {
	return c.queryTitles(ctx, `SELECT `+titleCols+` FROM catalog_titles WHERE type='show' AND browsable=1 AND released_ts>? AND released_ts<=? ORDER BY key`, since.Unix(), c.now().Unix())
}

// CachedDiscoveryEpisodes returns regular episode metadata without hydration or
// remote lookups. Unknown/future dates are retained so callers can distinguish
// an unmirrored premiere from a known but unreleased episode.
func (c *Catalog) CachedDiscoveryEpisodes(ctx context.Context, shows []core.RatingKey) (map[core.RatingKey][]core.Title, error) {
	out := map[core.RatingKey][]core.Title{}
	for start := 0; start < len(shows); start += 500 {
		batch := shows[start:min(start+500, len(shows))]
		args := make([]any, len(batch))
		for i, k := range batch {
			args[i] = k
		}
		rows, err := c.db.QueryContext(ctx, `SELECT `+titleCols+` FROM catalog_titles WHERE type='episode' AND season>0 AND show_key IN (`+placeholders(len(batch))+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			st, err := scanTitle(rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			out[st.t.ShowKey] = append(out[st.t.ShowKey], st.t)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// CachedGetMany applies CachedGet's visibility gates to bounded local reads.
// Missing or hidden titles are omitted. It never refreshes or admits fallbacks.
func (c *Catalog) cachedListVisible(t *core.Title) bool {
	return !t.Adult && t.IsReleased(c.now()) && (!c.config().GateMoviesByHomeRelease || homeReleaseVisible(t, c.now())) && (t.Type != core.TypeSeason || listableSeason(t, c.now()))
}

func (c *Catalog) cachedStored(key core.RatingKey, gen uint64, minute int64) (core.Title, bool) {
	if !cacheStoredTitle(key.Kind()) {
		return core.Title{}, false
	}
	c.cacheMu.RLock()
	ent, ok := c.titleCache[key]
	c.cacheMu.RUnlock()
	if !ok || ent.gen != gen || ent.minute != minute {
		return core.Title{}, false
	}
	return cloneTitle(ent.st.t), true
}

func (c *Catalog) rememberTitles(gen uint64, minute int64, items []core.Title) {
	if c.writeGen.Load() != gen {
		return
	}
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()
	if c.titleCache == nil {
		c.titleCache = map[core.RatingKey]cachedStoredTitle{}
	}
	for i := range items {
		if !cacheStoredTitle(items[i].Key.Kind()) {
			continue
		}
		if len(c.titleCache) >= 8192 {
			c.titleCache = map[core.RatingKey]cachedStoredTitle{}
		}
		c.titleCache[items[i].Key] = cachedStoredTitle{st: storedTitle{t: cloneTitle(items[i])}, gen: gen, minute: minute}
	}
}

func (c *Catalog) CachedGetMany(ctx context.Context, keys []core.RatingKey) (map[core.RatingKey]core.Title, error) {
	out := make(map[core.RatingKey]core.Title, len(keys))
	gen := c.writeGen.Load()
	minute := c.minuteBucket()
	missing := make([]core.RatingKey, 0, len(keys))
	for _, key := range keys {
		if title, ok := c.cachedStored(key, gen, minute); ok {
			if c.cachedListVisible(&title) {
				out[key] = title
			}
			continue
		}
		missing = append(missing, key)
	}
	for start := 0; start < len(missing); start += 200 {
		part := missing[start:min(start+200, len(missing))]
		args := make([]any, len(part))
		for i, key := range part {
			args[i] = int64(key)
		}
		rows, err := c.db.QueryContext(ctx, `SELECT `+titleCols+` FROM catalog_titles WHERE key IN (`+placeholders(len(part))+`)`, args...)
		if err != nil {
			return nil, err
		}
		var items []core.Title
		for rows.Next() {
			st, e := scanTitle(rows)
			if e != nil {
				rows.Close()
				return nil, e
			}
			items = append(items, st.t)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		if err := c.hydrateSlice(ctx, items); err != nil {
			return nil, err
		}
		visible := make([]core.Title, 0, len(items))
		for i := range items {
			if c.cachedListVisible(&items[i]) {
				out[items[i].Key] = items[i]
				visible = append(visible, items[i])
			}
		}
		c.rememberTitles(gen, minute, visible)
	}
	return out, nil
}

// ResolveShowParents batches fresh mirrored shows and falls back to Get for
// stale, missing, or non-show keys. Individual Get failures are omitted, as
// inherited visibility treats them as inaccessible.
func (c *Catalog) ResolveShowParents(ctx context.Context, keys []core.RatingKey) (map[core.RatingKey]core.Title, error) {
	out := make(map[core.RatingKey]core.Title, len(keys))
	if len(keys) == 0 {
		return out, nil
	}
	fallback := func(part []core.RatingKey) {
		for _, key := range part {
			title, err := c.Get(ctx, key)
			if err == nil && title != nil && title.Key == key {
				out[key] = *title
			}
		}
	}
	if err := c.admitFallbacks(ctx); err != nil {
		fallback(keys)
		return out, nil
	}
	for start := 0; start < len(keys); start += 200 {
		part := keys[start:min(start+200, len(keys))]
		args := make([]any, len(part))
		for i, k := range part {
			args[i] = int64(k)
		}
		rows, err := c.db.QueryContext(ctx, `SELECT `+titleCols+`,detail_at FROM catalog_titles WHERE key IN (`+placeholders(len(part))+`)`, args...)
		if err != nil {
			fallback(part)
			continue
		}
		stored := map[core.RatingKey]*storedTitle{}
		for rows.Next() {
			var detail int64
			st, e := scanTitle(extraScan{rows, []any{&detail}})
			if e != nil {
				err = e
				break
			}
			if detail != 0 {
				st.detailAt = time.Unix(detail, 0).UTC()
			}
			stored[st.t.Key] = st
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			fallback(part)
			continue
		}
		var warm []core.Title
		for _, key := range part {
			st := stored[key]
			if key.Kind() == core.KindShow && st != nil && (c.fresh(st) || c.tmdb() == nil) {
				warm = append(warm, st.t)
			}
		}
		if err := c.hydrateSlice(ctx, warm); err != nil {
			fallback(part)
			continue
		}
		for i := range warm {
			if c.visibleTitle(&warm[i]) {
				out[warm[i].Key] = warm[i]
			}
		}
		for _, key := range part {
			st := stored[key]
			if key.Kind() == core.KindShow && st != nil && (c.fresh(st) || c.tmdb() == nil) {
				continue
			}
			title, e := c.Get(ctx, key)
			if e == nil && title != nil && title.Key == key {
				out[key] = *title
			}
		}
	}
	return out, nil
}
