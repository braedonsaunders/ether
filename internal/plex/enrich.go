package plex

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/lru"
)

// ---------------------------------------------------------------------------
// Title detail cache.
//
// Hub rows and other lists come from TMDB list endpoints, whose results
// carry no credits, studios or certification. PMS lists include those
// (Director/Writer/Role/Country/Collection tags, studio, contentRating,
// tagline, clearLogo), and the newer apps show them on hub tiles and hero
// rows. Fetching every title's details inline would make listings slow, so
// the detail is fetched in the background on first sight and merged into
// later renders of the same title. A season or episode uses its show's
// cached detail for show-level fields (studio, rating, clear logo).

const (
	titleCacheEntries  = 4000
	titleCacheTTL      = 6 * time.Hour
	titleCacheRetry    = 30 * time.Minute // after a failed fetch
	titleFetchTimeout  = 20 * time.Second
	titleCacheMaxCast  = 10
	titleCacheMaxCrew  = 5
	titleFetchWorkers  = 3
	titleFetchQueueLen = 512
)

type titleEntry struct {
	key       core.RatingKey
	t         *core.Title // nil after a failed fetch
	fetchedAt time.Time
}

type titleCache struct {
	fetch func(ctx context.Context, key core.RatingKey) (*core.Title, error)
	q     *bgQueue
	now   func() time.Time
	log   *slog.Logger

	mu    sync.Mutex
	items *lru.Cache[core.RatingKey, *titleEntry]
}

func newTitleCache(q *bgQueue, now func() time.Time, log *slog.Logger, fetch func(context.Context, core.RatingKey) (*core.Title, error)) *titleCache {
	return &titleCache{fetch: fetch, q: q, now: now, log: log, items: lru.New(lru.Options[core.RatingKey, *titleEntry]{MaxEntries: titleCacheEntries})}
}

// get returns the cached detail for key without blocking. A missing or
// expired entry schedules a background fetch; a stale entry is still
// returned meanwhile.
func (c *titleCache) get(key core.RatingKey) *core.Title {
	c.mu.Lock()
	var t *core.Title
	refresh := true
	if e, ok := c.items.Get(key); ok {
		t = e.t
		age := c.now().Sub(e.fetchedAt)
		refresh = (e.t != nil && age >= titleCacheTTL) || (e.t == nil && age >= titleCacheRetry)
	}
	c.mu.Unlock()
	if refresh {
		c.schedule(key)
	}
	return t
}

// peek returns the cached detail for key without scheduling a background fetch.
func (c *titleCache) peek(key core.RatingKey) *core.Title {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items.Peek(key); ok {
		return e.t
	}
	return nil
}

func (c *titleCache) schedule(key core.RatingKey) {
	c.q.submit("title:"+strconv.FormatInt(int64(key), 10), func(ctx context.Context) {
		fctx, cancel := context.WithTimeout(ctx, titleFetchTimeout)
		defer cancel()
		t, err := c.fetch(fctx, key)
		if err != nil {
			if ctx.Err() != nil {
				return // shutting down: don't record a failure
			}
			c.log.Debug("title detail fetch failed", "key", key, "err", err)
			c.store(key, nil)
			return
		}
		c.put(t)
	})
}

// put records a detailed title (e.g. one just fetched for a detail page).
func (c *titleCache) put(t *core.Title) {
	if t == nil || t.Key == 0 {
		return
	}
	c.store(t.Key, trimForCache(t))
}

func (c *titleCache) store(key core.RatingKey, t *core.Title) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := c.items.Peek(key); ok && t == nil && old.t != nil {
		// Keep serving the stale detail; retry later.
		old.fetchedAt = c.now().Add(-titleCacheTTL + titleCacheRetry)
		return
	}
	c.items.Add(key, &titleEntry{key: key, t: t, fetchedAt: c.now()})
}

// trimForCache copies the fields list renders need, bounding credit lists.
func trimForCache(t *core.Title) *core.Title {
	cp := *t
	cp.Keywords = nil
	cp.Cast = clipPeople(t.Cast, titleCacheMaxCast)
	cp.Directors = clipPeople(t.Directors, titleCacheMaxCrew)
	cp.Writers = clipPeople(t.Writers, titleCacheMaxCrew)
	cp.Genres = append([]string(nil), t.Genres...)
	cp.Studios = append([]string(nil), t.Studios...)
	cp.Countries = append([]string(nil), t.Countries...)
	return &cp
}

func clipPeople(ps []core.Person, n int) []core.Person {
	if len(ps) > n {
		ps = ps[:n]
	}
	return append([]core.Person(nil), ps...)
}

// lacksDetail reports whether a movie or show came from a list summary
// without credits, studios or certification.
func lacksDetail(t *core.Title) bool {
	return len(t.Cast) == 0 && len(t.Directors) == 0 && len(t.Writers) == 0 && len(t.Studios) == 0 && t.ContentRating == ""
}

// mergeDetail fills fields the summary lacks from the detailed title.
func mergeDetail(t *core.Title, d *core.Title) {
	if len(t.Genres) == 0 {
		t.Genres = d.Genres
	}
	if len(t.Studios) == 0 {
		t.Studios = d.Studios
	}
	if len(t.Countries) == 0 {
		t.Countries = d.Countries
	}
	if len(t.Cast) == 0 {
		t.Cast = d.Cast
	}
	if len(t.Directors) == 0 {
		t.Directors = d.Directors
	}
	if len(t.Writers) == 0 {
		t.Writers = d.Writers
	}
	if t.ContentRating == "" {
		t.ContentRating = d.ContentRating
	}
	if t.Tagline == "" {
		t.Tagline = d.Tagline
	}
	if t.LogoPath == "" {
		t.LogoPath = d.LogoPath
	}
	if t.CollectionID == 0 && d.CollectionID != 0 {
		t.CollectionID, t.CollectionName = d.CollectionID, d.CollectionName
	}
	if t.Runtime == 0 {
		t.Runtime = d.Runtime
	}
	if t.ChildCount == 0 {
		t.ChildCount = d.ChildCount
	}
	if t.LeafCount == 0 {
		t.LeafCount = d.LeafCount
	}
	if t.IDs.IMDB == "" {
		t.IDs.IMDB = d.IDs.IMDB
	}
	if t.IDs.TVDB == 0 {
		t.IDs.TVDB = d.IDs.TVDB
	}
	if t.BackdropPath == "" {
		t.BackdropPath = d.BackdropPath
	}
	if t.PosterPath == "" {
		t.PosterPath = d.PosterPath
	}
}

// enrich returns t with list-summary gaps filled from the detail cache.
// Movies and shows are enriched from their own detail; t is returned
// unchanged when nothing is cached yet (a fetch is scheduled).
func (s *Server) enrich(t *core.Title) *core.Title {
	if s.titles == nil {
		return t
	}
	switch t.Type {
	case core.TypeMovie, core.TypeShow:
	default:
		return t
	}
	switch t.Key.Kind() {
	case core.KindMovie, core.KindShow:
	default:
		return t // spike/local items have no TMDB detail
	}
	if !lacksDetail(t) {
		return t
	}
	d := s.titles.get(t.Key)
	if d == nil {
		return t
	}
	cp := *t
	mergeDetail(&cp, d)
	return &cp
}

// showOf returns the cached detail of a season's or episode's show, or nil
// (scheduling a fetch) when it isn't cached yet.
func (s *Server) showOf(t *core.Title) *core.Title {
	return s.showOfOpts(t, false)
}

func (s *Server) showOfOpts(t *core.Title, storedOnly bool) *core.Title {
	if s.titles == nil || (t.Type != core.TypeSeason && t.Type != core.TypeEpisode) {
		return nil
	}
	show := t.ShowKey
	if show == 0 {
		show = t.Key.Parent()
		if t.Type == core.TypeEpisode {
			show = show.Parent()
		}
	}
	if show.Kind() != core.KindShow {
		return nil
	}
	if storedOnly {
		return s.titles.peek(show)
	}
	return s.titles.get(show)
}

// rememberTitle caches a detailed title fetched for a detail page.
func (s *Server) rememberTitle(t *core.Title) {
	if s.titles == nil || t == nil {
		return
	}
	switch t.Key.Kind() {
	case core.KindMovie, core.KindShow:
		if !lacksDetail(t) {
			s.titles.put(t)
		}
	}
}
