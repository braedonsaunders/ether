package catalog

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/tmdb"
)

// settledReleaseAge is how long after theatrical/series release a movie or an
// ended TV show's detailed metadata (credits, keywords, runtime, certification)
// is treated as immutable and kept forever without re-querying TMDB.
const (
	settledReleaseAge  = 90 * 24 * time.Hour
	activeSeasonTTL    = time.Hour
	caughtUpRefreshTTL = 15 * time.Minute
	// refreshMissTTL is how long a failed metadata read is not retried.
	// Library lists call Get per row; without this, one outage becomes one
	// network wait per title.
	refreshMissTTL = time.Minute
)

func showRefreshKey(id int64) string  { return fmt.Sprintf("show:%d", id) }
func movieRefreshKey(id int64) string { return fmt.Sprintf("movie:%d", id) }
func seasonRefreshKey(id int64, season int) string {
	return fmt.Sprintf("season:%d:%d", id, season)
}

func (c *Catalog) refreshMissed(key string) bool {
	c.refreshMissMu.Lock()
	defer c.refreshMissMu.Unlock()
	at, ok := c.refreshMiss[key]
	if !ok {
		return false
	}
	if c.now().Sub(at) >= refreshMissTTL {
		delete(c.refreshMiss, key)
		return false
	}
	return true
}

func (c *Catalog) noteRefreshMiss(key string) {
	c.refreshMissMu.Lock()
	defer c.refreshMissMu.Unlock()
	if c.refreshMiss == nil {
		c.refreshMiss = map[string]time.Time{}
	}
	c.refreshMiss[key] = c.now()
}

func (c *Catalog) fresh(st *storedTitle) bool {
	// detailAt, not fetchedAt: summary upserts from sync, lists and search
	// refresh fetchedAt and would otherwise keep popular titles' details
	// (seasons, credits, release dates) from ever being refetched.
	if st == nil || !st.detail || st.detailAt.IsZero() {
		return false
	}
	now := c.now()
	if now.Sub(st.detailAt) < c.staleAfter {
		return true
	}
	switch st.t.Type {
	case core.TypeMovie:
		if !st.t.Released.IsZero() && now.Sub(st.t.Released) > settledReleaseAge {
			return true
		}
	case core.TypeShow:
		if (strings.EqualFold(st.t.Status, "Ended") || strings.EqualFold(st.t.Status, "Canceled")) &&
			!st.t.Released.IsZero() && now.Sub(st.t.Released) > settledReleaseAge {
			return true
		}
	}
	return false
}

// Get returns a title from the mirror, fetching on demand when missing/stale.
func (c *Catalog) Get(ctx context.Context, key core.RatingKey) (*core.Title, error) {
	if err := c.admitFallbacks(ctx); err != nil {
		return nil, err
	}
	t, err := c.get(ctx, key)
	if err == nil && !c.visibleTitle(t) {
		return nil, core.ErrNotFound
	}
	// A season none of whose episodes has aired is not listed (seasonsOfShow)
	// and must not open directly either.
	return t, err
}

func (c *Catalog) visibleTitle(t *core.Title) bool {
	return t != nil && !t.Adult && t.IsReleased(c.now()) && (!c.config().GateMoviesByHomeRelease || c.homeVisible(t)) && (t.Type != core.TypeSeason || listableSeason(t, c.now()))
}

// CachedGet applies the same catalogue visibility gates as Get without a
// network refresh. Missing or hidden records fail closed; callers may use
// the normal catalogue path separately to populate the local mirror.
func (c *Catalog) CachedGet(ctx context.Context, key core.RatingKey) (*core.Title, error) {
	stored, err := c.loadByKey(ctx, key)
	if err != nil {
		return nil, err
	}
	if !c.visibleTitle(&stored.t) {
		return nil, core.ErrNotFound
	}
	return &stored.t, nil
}

func (c *Catalog) StoredTitle(ctx context.Context, key core.RatingKey) (*core.Title, error) {
	st, err := c.loadByKey(ctx, key)
	if err != nil {
		return nil, err
	}
	return &st.t, nil
}

// RefreshStoredTitle drops the minute cache and reads the stored row again.
// Portrait bytes use it when a credit changed inside the current cache minute.
func (c *Catalog) RefreshStoredTitle(ctx context.Context, key core.RatingKey) (*core.Title, error) {
	c.cacheMu.Lock()
	delete(c.titleCache, key)
	c.cacheMu.Unlock()
	return c.StoredTitle(ctx, key)
}

func (c *Catalog) get(ctx context.Context, key core.RatingKey) (*core.Title, error) {
	switch key.Kind() {
	case core.KindMovie:
		return c.getMovie(ctx, key.TMDB())
	case core.KindShow:
		return c.getShow(ctx, key.TMDB())
	case core.KindSeason:
		return c.getSeason(ctx, key)
	case core.KindEpisode:
		return c.getEpisode(ctx, key)
	default:
		return nil, fmt.Errorf("%w: key %s", core.ErrNotFound, key)
	}
}

func (c *Catalog) getMovie(ctx context.Context, id int64) (*core.Title, error) {
	key := core.MovieKey(id)
	st, err := c.loadByKey(ctx, key)
	if err == nil && c.fresh(st) {
		return &st.t, nil
	}
	if c.tmdb() != nil {
		if c.refreshMissed(movieRefreshKey(id)) {
			if err == nil {
				return &st.t, nil
			}
			return nil, core.ErrNotFound
		}
		d, ferr := c.tmdb().MovieDetails(ctx, id)
		if ferr != nil {
			if ctx.Err() == nil {
				c.noteRefreshMiss(movieRefreshKey(id))
			}
			if err == nil {
				return &st.t, nil // serve stale on fetch failure
			}
			return nil, ferr
		}
		t := c.movieDetailsToTitle(ctx, d)
		if uerr := c.upsert(ctx, t, upsertOpts{browsable: c.meetsThreshold(t), detail: true, releaseDates: d.ReleaseDates, gateEnabled: c.config().GateMoviesByHomeRelease}); uerr != nil {
			return nil, uerr
		}
		c.harvestMovieCrowd(ctx, key, d.Recommendations)
		st2, lerr := c.loadByKey(ctx, key)
		if lerr != nil {
			return &t, nil
		}
		return &st2.t, nil
	}
	if err != nil {
		return nil, err
	}
	return &st.t, nil
}

// storeShowDetails mirrors a show's details and its season summaries, so a
// season TMDB has added since the last fetch becomes listable.
func (c *Catalog) storeShowDetails(ctx context.Context, t core.Title, d *tmdb.TVDetails) error {
	var seasons []core.Title
	for _, s := range d.Seasons {
		if !episodeKeyFits(s.SeasonNumber, 0) {
			continue // its episodes have no rating keys (year-numbered seasons)
		}
		seasons = append(seasons, seasonToTitle(t, s))
	}
	// Upsert show as detail; seasons as non-detail summaries in same call would
	// mark them detail too, so split.
	if err := c.upsert(ctx, t, upsertOpts{browsable: c.meetsThreshold(t), detail: true}); err != nil {
		return err
	}
	if len(seasons) > 0 {
		return c.upsertMany(ctx, seasons, upsertOpts{airDates: true})
	}
	return nil
}

// showFresh shortens a Returning Series' detail TTL to activeSeasonTTL, as
// seasonFresh does for its latest season: the show details carry the season
// list, so a premiere is listed within the hour rather than after staleAfter.
func (c *Catalog) showFresh(st *storedTitle) bool {
	if !c.fresh(st) {
		return false
	}
	if strings.EqualFold(st.t.Status, "Returning Series") {
		return c.now().Sub(st.detailAt) < min(c.staleAfter, activeSeasonTTL)
	}
	return true
}

func (c *Catalog) getShow(ctx context.Context, id int64) (*core.Title, error) {
	key := core.ShowKey(id)
	st, err := c.loadByKey(ctx, key)
	if err == nil && c.showFresh(st) {
		return &st.t, nil
	}
	if c.tmdb() != nil {
		if c.refreshMissed(showRefreshKey(id)) {
			if err == nil {
				return &st.t, nil
			}
			return nil, core.ErrNotFound
		}
		d, ferr := c.tmdb().TVDetails(ctx, id)
		if ferr != nil {
			if ctx.Err() == nil {
				c.noteRefreshMiss(showRefreshKey(id))
			}
			if err == nil {
				return &st.t, nil
			}
			return nil, ferr
		}
		t := c.tvDetailsToTitle(ctx, d)
		if uerr := c.storeShowDetails(ctx, t, d); uerr != nil {
			return nil, uerr
		}
		c.harvestTVCrowd(ctx, key, d.Recommendations)
		st2, lerr := c.loadByKey(ctx, key)
		if lerr != nil {
			return &t, nil
		}
		return &st2.t, nil
	}
	if err != nil {
		return nil, err
	}
	return &st.t, nil
}

// ensureShow returns the show title, fetching if needed.
func (c *Catalog) ensureShow(ctx context.Context, showID int64) (*core.Title, error) {
	return c.getShow(ctx, showID)
}

func (c *Catalog) getSeason(ctx context.Context, key core.RatingKey) (*core.Title, error) {
	_, err := c.fetchSeason(ctx, key.TMDB(), key.Season())
	if err != nil {
		// Fall back to stored season summary.
		if st, lerr := c.loadByKey(ctx, key); lerr == nil {
			return &st.t, nil
		}
		return nil, err
	}
	st, err := c.loadByKey(ctx, key)
	if err != nil {
		return nil, err
	}
	return &st.t, nil
}

// seasonFresh uses a shorter TTL (1h) for the latest season of an active
// Returning Series so shows airing multiple times a week (e.g. Sun/Wed/Thu)
// pick up same-day TMDB episode/air-date additions without waiting 24h.
func (c *Catalog) seasonFresh(st *storedTitle, show *core.Title) bool {
	if !c.fresh(st) {
		return false
	}
	if show != nil && strings.EqualFold(show.Status, "Returning Series") && st.t.Season >= show.ChildCount {
		ttl := min(c.staleAfter, activeSeasonTTL)
		return c.now().Sub(st.detailAt) < ttl
	}
	return true
}

// fetchSeason ensures season+episodes are in the mirror, fetching TMDB when
// missing or stale. Returns the season title.
func (c *Catalog) fetchSeason(ctx context.Context, showID int64, season int) (*core.Title, error) {
	return c.fetchSeasonOpts(ctx, showID, season, false)
}

func (c *Catalog) fetchSeasonOpts(ctx context.Context, showID int64, season int, force bool) (*core.Title, error) {
	key := core.SeasonKey(showID, season)
	show, err := c.ensureShow(ctx, showID)
	if err != nil {
		return nil, err
	}
	// If season row is fresh-detailed and episodes exist, skip network.
	// Mirrored, not aired: a fresh season whose episodes are all still
	// to air must not refetch on every read.
	if !force {
		if st, lerr := c.loadByKey(ctx, key); lerr == nil && c.seasonFresh(st, show) {
			if ok, qerr := c.seasonMirrored(ctx, showID, season); qerr == nil && ok {
				return &st.t, nil
			}
		}
	}
	if c.tmdb() == nil {
		st, lerr := c.loadByKey(ctx, key)
		if lerr != nil {
			return nil, lerr
		}
		return &st.t, nil
	}
	if c.refreshMissed(seasonRefreshKey(showID, season)) {
		st, lerr := c.loadByKey(ctx, key)
		if lerr != nil {
			return nil, core.ErrNotFound
		}
		return &st.t, nil
	}
	sd, err := c.tmdb().SeasonDetails(ctx, showID, season)
	if err != nil {
		if ctx.Err() == nil {
			c.noteRefreshMiss(seasonRefreshKey(showID, season))
		}
		if st, lerr := c.loadByKey(ctx, key); lerr == nil {
			return &st.t, nil
		}
		return nil, err
	}
	seasonName := sd.Name
	if seasonName == "" {
		seasonName = "Season " + itoa(season)
	}
	seasonT := seasonToTitle(*show, tmdbSeasonSummary(sd))
	eps := make([]core.Title, 0, len(sd.Episodes))
	for _, e := range sd.Episodes {
		if !episodeKeyFits(e.SeasonNumber, e.EpisodeNumber) {
			// Its rating key would decode as another title's episode.
			continue
		}
		eps = append(eps, episodeToTitle(*show, seasonName, e))
	}
	seasonT.ChildCount = len(eps)
	if err := c.upsert(ctx, seasonT, upsertOpts{detail: true}); err != nil {
		return nil, err
	}
	// Episodes from a season payload lack their own external ids, so they are
	// not marked detail; Get(episode) enriches and marks them.
	if err := c.upsertMany(ctx, eps, upsertOpts{airDates: true}); err != nil {
		return nil, err
	}
	return &seasonT, nil
}

// episodeKeyFits reports whether an episode's numbers fit its rating key
// (core.EpisodeKey: season < 1000, episode < 10000).
func episodeKeyFits(season, episode int) bool {
	return season >= 0 && season < 1000 && episode >= 0 && episode < 10_000
}

func tmdbSeasonSummary(sd *tmdb.SeasonDetails) tmdb.SeasonSummary {
	return tmdb.SeasonSummary{ID: sd.ID, Name: sd.Name, Overview: sd.Overview, SeasonNumber: sd.SeasonNumber,
		EpisodeCount: len(sd.Episodes), AirDate: sd.AirDate, PosterPath: sd.PosterPath, VoteAverage: sd.VoteAverage}
}

func (c *Catalog) getEpisode(ctx context.Context, key core.RatingKey) (*core.Title, error) {
	if st, err := c.loadByKey(ctx, key); err == nil && c.fresh(st) {
		return &st.t, nil
	}
	if _, err := c.fetchSeason(ctx, key.TMDB(), key.Season()); err != nil {
		if st, lerr := c.loadByKey(ctx, key); lerr == nil {
			return &st.t, nil
		}
		return nil, err
	}
	st, err := c.loadByKey(ctx, key)
	if err != nil {
		return nil, core.ErrNotFound
	}
	// Best-effort episode external ids (its own IMDb id is useful to indexers).
	// A season refresh that just failed will fail these too; do not fan that
	// wait out across every episode in a list.
	if c.tmdb() != nil && st.t.IDs.IMDB == "" && !c.refreshMissed(seasonRefreshKey(key.TMDB(), key.Season())) {
		if ext, ferr := c.tmdb().EpisodeExternalIDs(ctx, key.TMDB(), key.Season(), key.Episode()); ferr == nil && ext != nil && (ext.IMDBID != "" || ext.TVDBID != 0) {
			t := st.t
			t.IDs.IMDB = ext.IMDBID
			if ext.TVDBID != 0 {
				t.IDs.TVDB = ext.TVDBID
			}
			// Nil facets so the upsert keeps the stored rows. Keywords
			// especially: the loaded episode carries its show's (inherited
			// on read), which must not be saved as the episode's own.
			t.Cast, t.Directors, t.Writers, t.Crew = nil, nil, nil, nil
			t.Genres, t.Studios, t.Countries, t.Keywords = nil, nil, nil, nil
			_ = c.upsert(ctx, t, upsertOpts{detail: true})
			st.t.IDs.IMDB = ext.IMDBID
			if ext.TVDBID != 0 {
				st.t.IDs.TVDB = ext.TVDBID
			}
		}
	}
	return &st.t, nil
}

// seasonsOfShow lists a show's seasons that have aired: their own air date
// has passed and at least one episode has aired (queryTitles drops
// unreleased rows and applies aired counts).
func (c *Catalog) seasonsOfShow(ctx context.Context, showID int64) ([]core.Title, error) {
	// show_key, not show_tmdb: only show_key is indexed, and a show's
	// pages read its seasons and episodes many times.
	all, err := c.queryTitles(ctx, `SELECT `+titleCols+` FROM catalog_titles WHERE type='season' AND show_key=? ORDER BY season ASC`, int64(core.ShowKey(showID)))
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for i := range all {
		if listableSeason(&all[i], c.now()) {
			out = append(out, all[i])
		}
	}
	return out, nil
}

// episodesOfSeason lists a season's aired episodes; queryTitles drops those
// with unknown or future air dates.
func (c *Catalog) episodesOfSeason(ctx context.Context, showID int64, season int) ([]core.Title, error) {
	return c.queryTitles(ctx, `SELECT `+titleCols+` FROM catalog_titles WHERE type='episode' AND show_key=? AND season=? ORDER BY episode ASC`, int64(core.ShowKey(showID)), season)
}

// seasonMirrored reports whether any of a season's episodes, aired or not,
// are in the mirror.
func (c *Catalog) seasonMirrored(ctx context.Context, showID int64, season int) (bool, error) {
	var n int
	err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM catalog_titles WHERE type='episode' AND show_key=? AND season=?`, int64(core.ShowKey(showID)), season).Scan(&n)
	return n > 0, err
}

// Children: seasons of a show, episodes of a season.
func (c *Catalog) Children(ctx context.Context, key core.RatingKey) ([]core.Title, error) {
	if _, err := c.Get(ctx, key); err != nil {
		return nil, err
	}
	switch key.Kind() {
	case core.KindShow:
		if _, err := c.ensureShow(ctx, key.TMDB()); err != nil {
			return nil, err
		}
		out, err := c.seasonsOfShow(ctx, key.TMDB())
		if out == nil && err == nil {
			out = []core.Title{}
		}
		return out, err
	case core.KindSeason:
		if _, err := c.fetchSeason(ctx, key.TMDB(), key.Season()); err != nil {
			return nil, err
		}
		out, err := c.episodesOfSeason(ctx, key.TMDB(), key.Season())
		if out == nil && err == nil {
			out = []core.Title{}
		}
		return out, err
	case core.KindMovie, core.KindEpisode:
		return []core.Title{}, nil
	default:
		return nil, fmt.Errorf("%w: key %s", core.ErrNotFound, key)
	}
}

// Grandchildren: all episodes of a show.
func (c *Catalog) Grandchildren(ctx context.Context, key core.RatingKey) ([]core.Title, error) {
	if key.Kind() != core.KindShow {
		if key.Kind() == core.KindSeason || key.Kind() == core.KindMovie || key.Kind() == core.KindEpisode {
			return []core.Title{}, nil
		}
		return nil, fmt.Errorf("%w: key %s", core.ErrNotFound, key)
	}
	gen := c.writeGen.Load()
	minute := c.minuteBucket()
	c.cacheMu.RLock()
	if ent, ok := c.grandCache[key]; ok && ent.gen == gen && ent.minute == minute {
		items := make([]core.Title, len(ent.page.Items))
		for i := range ent.page.Items {
			items[i] = cloneTitle(ent.page.Items[i])
		}
		c.cacheMu.RUnlock()
		return items, nil
	}
	c.cacheMu.RUnlock()
	seasons, err := c.Children(ctx, key)
	if err != nil {
		return nil, err
	}
	if len(seasons) == 0 {
		return []core.Title{}, nil
	}
	if err := c.loadSeasons(ctx, key.TMDB(), seasons); err != nil {
		return nil, err
	}
	// Bound the season IN like every other key IN in the package, however
	// many seasons a show has. Seasons arrive in listing order, so chunk
	// results stay in season/episode order when concatenated.
	out := []core.Title{}
	for start := 0; start < len(seasons); start += 200 {
		part := seasons[start:min(start+200, len(seasons))]
		numbers := make([]any, 0, len(part)+1)
		numbers = append(numbers, int64(key))
		for _, s := range part {
			numbers = append(numbers, s.Season)
		}
		chunk, err := c.queryTitles(ctx, `SELECT `+titleCols+` FROM catalog_titles WHERE type='episode' AND show_key=? AND season IN (`+placeholders(len(part))+`) ORDER BY season ASC, episode ASC`, numbers...)
		if err != nil {
			return nil, err
		}
		out = append(out, chunk...)
	}
	if c.writeGen.Load() == gen {
		stored := make([]core.Title, len(out))
		for i := range out {
			stored[i] = cloneTitle(out[i])
		}
		c.cacheMu.Lock()
		if len(c.grandCache) >= 256 {
			c.grandCache = map[core.RatingKey]cachedBrowse{}
		}
		c.grandCache[key] = cachedBrowse{page: core.Page{Items: stored, Total: len(stored)}, gen: gen, minute: minute}
		c.cacheMu.Unlock()
	}
	return out, nil
}

// seasonLoads bounds a show's concurrent season fetches.
const seasonLoads = 6

// loadSeasons brings a show's listed seasons into the mirror for a whole-show
// read. A season is fetched unless a season fetch has mirrored it: its season
// record is detailed and it has episodes. A mirrored season is refetched when
// stale only if it can still change: it is the latest season, it has episodes
// yet to air, or the show's refreshed summary counts more episodes than the
// mirror holds (a former latest season's final episodes). Otherwise it stays
// as mirrored until its own page or episodes are opened (fetchSeason), so a
// long-running show does not refetch every past season once its details go
// stale. Fetches run concurrently.
func (c *Catalog) loadSeasons(ctx context.Context, showID int64, seasons []core.Title) error {
	type mirrored struct{ episodes, pending int }
	have := map[int]mirrored{}
	rows, err := c.db.QueryContext(ctx, `SELECT season,COUNT(*),COUNT(CASE WHEN released_ts=0 OR released_ts>? THEN 1 END)
		FROM catalog_titles WHERE type='episode' AND show_key=? GROUP BY season`, c.now().Unix(), int64(core.ShowKey(showID)))
	if err != nil {
		return err
	}
	for rows.Next() {
		var season int
		var m mirrored
		if err := rows.Scan(&season, &m.episodes, &m.pending); err != nil {
			rows.Close()
			return err
		}
		have[season] = m
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	type record struct {
		detailed, fresh bool
		episodes        int // the latest season summary's or fetch's count
	}
	records := map[int]record{}
	rows, err = c.db.QueryContext(ctx, `SELECT season,detail,detail_at,child_count FROM catalog_titles WHERE type='season' AND show_key=?`, int64(core.ShowKey(showID)))
	if err != nil {
		return err
	}
	for rows.Next() {
		var season, detail, child int
		var detailAt int64
		if err := rows.Scan(&season, &detail, &detailAt, &child); err != nil {
			rows.Close()
			return err
		}
		st := storedTitle{detail: detail != 0}
		if detailAt != 0 {
			st.detailAt = time.Unix(detailAt, 0).UTC()
		}
		records[season] = record{detailed: st.detail && detailAt != 0, fresh: c.fresh(&st), episodes: child}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	latest := 0
	for _, s := range seasons {
		latest = max(latest, s.Season)
	}
	var load []int
	for _, s := range seasons {
		m, ok := have[s.Season]
		r := records[s.Season]
		switch {
		case !ok || !r.detailed:
		case r.fresh:
			continue
		case s.Season != latest && m.pending == 0 && r.episodes <= m.episodes:
			continue
		}
		load = append(load, s.Season)
	}
	if len(load) == 0 {
		return nil
	}
	// Each fetch loads the show; make sure it is mirrored once, first.
	if _, err := c.ensureShow(ctx, showID); err != nil {
		return err
	}
	errs := make([]error, len(load))
	sem := make(chan struct{}, seasonLoads)
	var wg sync.WaitGroup
	for i, season := range load {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				errs[i] = ctx.Err()
				return
			}
			defer func() { <-sem }()
			_, errs[i] = c.fetchSeason(ctx, showID, season)
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

// NextEpisode returns the next regular episode (specials, season 0, are skipped
// as a target; calling it on a special jumps to S1E1-order sequence).
func (c *Catalog) NextEpisode(ctx context.Context, episode core.RatingKey) (*core.Title, error) {
	if episode.Kind() != core.KindEpisode {
		return nil, core.ErrNotFound
	}
	showID, curSeason, curEp := episode.TMDB(), episode.Season(), episode.Episode()
	show, err := c.ensureShow(ctx, showID)
	if err != nil {
		return nil, err
	}
	// Same season, higher episode number (only for regular seasons).
	if curSeason > 0 {
		if _, err := c.fetchSeason(ctx, showID, curSeason); err == nil {
			eps, err := c.episodesOfSeason(ctx, showID, curSeason)
			if err != nil {
				return nil, err
			}
			for _, e := range eps {
				if e.Episode > curEp {
					ep := e
					return &ep, nil
				}
			}
			// Caught up to the end of the mirrored episodes on a Returning
			// Series: if the season has not been re-checked in the last 15
			// minutes, poll TMDB in case tonight's episode was just added.
			if show != nil && strings.EqualFold(show.Status, "Returning Series") {
				sKey := core.SeasonKey(showID, curSeason)
				if st, lerr := c.loadByKey(ctx, sKey); lerr == nil && c.now().Sub(st.detailAt) >= min(c.staleAfter, caughtUpRefreshTTL) {
					if _, ferr := c.fetchSeasonOpts(ctx, showID, curSeason, true); ferr == nil {
						if eps2, eerr := c.episodesOfSeason(ctx, showID, curSeason); eerr == nil {
							for _, e := range eps2 {
								if e.Episode > curEp {
									ep := e
									return &ep, nil
								}
							}
						}
					}
				}
			}
		}
	}
	seasons, err := c.seasonsOfShow(ctx, showID)
	if err != nil {
		return nil, err
	}
	for _, s := range seasons {
		if s.Season <= curSeason || s.Season == 0 {
			continue
		}
		if _, err := c.fetchSeason(ctx, showID, s.Season); err != nil {
			if errors.Is(err, core.ErrNotFound) {
				continue
			}
			return nil, err
		}
		eps, err := c.episodesOfSeason(ctx, showID, s.Season)
		if err != nil {
			return nil, err
		}
		for _, e := range eps {
			if e.Episode > 0 {
				ep := e
				return &ep, nil
			}
		}
	}
	return nil, core.ErrNotFound
}

// RefreshTitle updates canonical details from TMDB without a stale fallback.
func (c *Catalog) RefreshTitle(ctx context.Context, key core.RatingKey, persist bool) (*core.Title, error) {
	client := c.tmdb()
	if client == nil {
		return nil, fmt.Errorf("catalog: TMDB unavailable")
	}
	var t core.Title
	switch key.Kind() {
	case core.KindMovie:
		var d *tmdb.MovieDetails
		var err error
		if fresh, ok := client.(interface {
			MovieDetailsFresh(context.Context, int64) (*tmdb.MovieDetails, error)
		}); ok {
			d, err = fresh.MovieDetailsFresh(ctx, key.TMDB())
		} else {
			return nil, fmt.Errorf("catalog: fresh movie details unavailable")
		}
		if err != nil {
			return nil, err
		}
		if d == nil || d.ID != key.TMDB() {
			return nil, fmt.Errorf("catalog: invalid movie details for %s", key)
		}
		t = c.movieDetailsToTitle(ctx, d)
		if persist {
			if err := c.upsert(ctx, t, upsertOpts{browsable: c.meetsThreshold(t), detail: true, releaseDates: d.ReleaseDates, gateEnabled: c.config().GateMoviesByHomeRelease}); err != nil {
				return nil, err
			}
		}
	case core.KindShow:
		var d *tmdb.TVDetails
		var err error
		if fresh, ok := client.(interface {
			TVDetailsFresh(context.Context, int64) (*tmdb.TVDetails, error)
		}); ok {
			d, err = fresh.TVDetailsFresh(ctx, key.TMDB())
		} else {
			return nil, fmt.Errorf("catalog: fresh show details unavailable")
		}
		if err != nil {
			return nil, err
		}
		if d == nil || d.ID != key.TMDB() {
			return nil, fmt.Errorf("catalog: invalid show details for %s", key)
		}
		t = c.tvDetailsToTitle(ctx, d)
		if persist {
			if err := c.storeShowDetails(ctx, t, d); err != nil {
				return nil, err
			}
		}
	default:
		return nil, core.ErrNotFound
	}
	if !persist {
		return &t, nil
	}
	st, err := c.loadByKey(ctx, key)
	if err != nil {
		return nil, err
	}
	return &st.t, nil
}

var _ core.AuthoritativeTitleRefresher = (*Catalog)(nil)
