// Package calendar assembles the owner's release calendar: upcoming and
// recent episodes of the shows the household watches or has watchlisted,
// and movie releases from watchlists, Look Ahead collections and the
// household's taste. Ether's catalogue is all of TMDB, so the calendar is
// derived from what viewers actually follow rather than from a library.
//
// The request path reads the local catalogue mirror and watch state only.
// A background pass (Run) keeps followed shows' current seasons mirrored so
// their future episodes are known; requests never contact TMDB.
package calendar

import (
	"context"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/core"
)

// Catalog is the local, read-only mirror surface the calendar needs, plus
// the background refresh of a followed show's seasons.
type Catalog interface {
	ScheduledShows(ctx context.Context, shows []core.RatingKey, from, until time.Time, limit int) ([]catalog.ScheduleEntry, error)
	ScheduledPremieres(ctx context.Context, shows []core.RatingKey, from, until time.Time) ([]catalog.ScheduleEntry, error)
	ScheduledMovies(ctx context.Context, keys []core.RatingKey, from, until time.Time) ([]catalog.ScheduleEntry, error)
	PopularScheduledMovies(ctx context.Context, from, until time.Time, limit int) ([]catalog.ScheduleEntry, error)
	CachedGetMany(ctx context.Context, keys []core.RatingKey) (map[core.RatingKey]core.Title, error)
	RefreshSchedule(ctx context.Context, show core.RatingKey) error
}

// Activity is a viewer's latest watch activity on a show.
type Activity struct {
	UserID     int64
	Show       core.RatingKey
	LastViewed time.Time
}

// LookAhead is a server-wide collection of upcoming releases.
type LookAhead struct {
	Name    string
	Members []core.RatingKey
}

// Options are the calendar's dependencies. Catalog, Viewers, Followed and
// Allowed are required; the rest may be nil.
type Options struct {
	Catalog Catalog
	// Viewers lists the household (active viewers, owner included).
	Viewers func(context.Context) ([]core.User, error)
	// Followed lists shows with episodes watched or in progress since a time.
	Followed func(context.Context, time.Time) ([]Activity, error)
	// Watched reports one viewer's watch state for the given titles.
	Watched func(context.Context, int64, []core.RatingKey) (map[core.RatingKey]core.WatchStatus, error)
	// Watchlisted lists every viewer's watchlisted movies and shows.
	Watchlisted func(context.Context) (map[int64][]core.RatingKey, error)
	// LookAheads lists the server-wide Look Ahead collections.
	LookAheads func(context.Context) ([]LookAhead, error)
	// Allowed applies the owner's content policy for a viewer (rating caps,
	// library restrictions, a show's inherited policy).
	Allowed func(context.Context, *core.User, *core.Title) bool
	// UpcomingMovies refreshes the mirror's upcoming movie releases in the
	// background (TMDB's upcoming list).
	UpcomingMovies func(context.Context) error
	// Image turns a TMDB image path into a console URL.
	Image  func(path string, width int) string
	Now    func() time.Time
	Logger *slog.Logger
}

const (
	// followWindow is how far back watch activity makes a show followed.
	followWindow = 365 * 24 * time.Hour
	// maxSpan bounds one request's date range.
	maxSpan = 100 * 24 * time.Hour
	// maxEntries bounds one response.
	maxEntries = 2500
	// cacheTTL is how long an assembled calendar is reused.
	cacheTTL = 2 * time.Minute
	// cacheSize bounds the number of cached calendars.
	cacheSize = 32
	// refreshEvery is the background pass interval; refreshBudget bounds
	// the shows one pass checks (fresh ones cost no TMDB request).
	refreshEvery  = 30 * time.Minute
	refreshBudget = 200
	// upcomingEvery is how often TMDB's upcoming movie list is mirrored.
	upcomingEvery = 6 * time.Hour
)

// Service builds calendars and keeps the mirror ready for them.
type Service struct {
	o Options

	mu    sync.Mutex
	cache map[string]cached
	// lastPass is when the last background pass finished (zero: never).
	lastPass time.Time
}

type cached struct {
	at   time.Time
	resp *Response
}

// New returns a calendar service.
func New(o Options) *Service {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Image == nil {
		o.Image = func(path string, _ int) string { return path }
	}
	return &Service{o: o, cache: map[string]cached{}}
}

// Query selects a calendar.
type Query struct {
	From, To time.Time // dates; To is inclusive
	// Viewer is one viewer's calendar; 0 is the whole household.
	Viewer int64
	// Types: "" (both), "movie" or "show".
	Types string
	// Movies is the movie policy: "foryou" (default; watchlists, Look Ahead
	// collections and the household's taste), "popular", "watchlist" or
	// "hidden".
	Movies string
	// Location decides which day is "today".
	Location *time.Location
}

// Response is an assembled calendar.
type Response struct {
	From    string   `json:"from"`
	To      string   `json:"to"`
	Today   string   `json:"today"`
	Viewers []Viewer `json:"viewers"`
	Entries []Entry  `json:"entries"`
	// Shows is how many followed shows the calendar draws from.
	Shows int `json:"shows"`
	// Truncated is set when the range held more than maxEntries.
	Truncated   bool      `json:"truncated,omitempty"`
	GeneratedAt time.Time `json:"generatedAt"`
	// Warming is set until the first background pass has mirrored followed
	// shows' seasons, so the owner knows more episodes are on the way.
	Warming bool `json:"warming,omitempty"`
}

// Viewer is a household member as the calendar shows them.
type Viewer struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Thumb string `json:"thumb,omitempty"`
	Owner bool   `json:"owner,omitempty"`
}

// Entry kinds.
const (
	KindEpisode  = "episode"
	KindPremiere = "premiere" // an announced season whose episodes are not listed yet
	KindMovie    = "movie"
)

// Entry states.
const (
	StateAvailable   = "available"   // released and playable now
	StateToday       = "today"       // airs or releases today
	StateUpcoming    = "upcoming"    // later
	StateUnavailable = "unavailable" // released, but not playable yet (e.g. only in cinemas)
)

// Why an entry is on the calendar.
const (
	ReasonWatching  = "watching"
	ReasonWatchlist = "watchlist"
	ReasonLookAhead = "lookahead"
	ReasonTaste     = "taste"
	ReasonPopular   = "popular"
)

// Entry is one dated release.
type Entry struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	// Key is the dated title; Open is the title the console opens (the show
	// for episodes and premieres).
	Key           string   `json:"key"`
	Open          string   `json:"open"`
	Title         string   `json:"title"`
	EpisodeTitle  string   `json:"episodeTitle,omitempty"`
	Season        int      `json:"season,omitempty"`
	Episode       int      `json:"episode,omitempty"`
	Date          string   `json:"date"`
	Year          int      `json:"year,omitempty"`
	Poster        string   `json:"poster,omitempty"`
	Backdrop      string   `json:"backdrop,omitempty"`
	Still         string   `json:"still,omitempty"`
	Overview      string   `json:"overview,omitempty"`
	Runtime       int      `json:"runtimeMinutes,omitempty"`
	ContentRating string   `json:"contentRating,omitempty"`
	Rating        float64  `json:"rating,omitempty"`
	Genres        []string `json:"genres,omitempty"`
	State         string   `json:"state"`
	Badges        []string `json:"badges,omitempty"`
	Reasons       []string `json:"reasons"`
	// Collections names the Look Ahead collections listing a movie.
	Collections []string `json:"collections,omitempty"`
	// Viewers are who it is on the calendar for; Watched who has seen it.
	Viewers []int64 `json:"viewers"`
	Watched []int64 `json:"watched,omitempty"`

	released time.Time
	pop      float64
}

// Badges.
const (
	BadgeSeriesPremiere = "seriesPremiere"
	BadgeSeasonPremiere = "seasonPremiere"
	BadgeFinale         = "finale"
)

// Calendar returns the calendar for q, reusing a recent assembly.
func (s *Service) Calendar(ctx context.Context, q Query) (*Response, error) {
	q = normalize(q, s.o.Now())
	key := q.cacheKey()
	now := s.o.Now()
	s.mu.Lock()
	if c, ok := s.cache[key]; ok && now.Sub(c.at) < cacheTTL {
		s.mu.Unlock()
		return c.resp, nil
	}
	s.mu.Unlock()
	resp, err := s.build(ctx, q, now)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if len(s.cache) >= cacheSize {
		for k, c := range s.cache {
			if now.Sub(c.at) >= cacheTTL || len(s.cache) >= cacheSize {
				delete(s.cache, k)
			}
		}
	}
	s.cache[key] = cached{at: now, resp: resp}
	s.mu.Unlock()
	return resp, nil
}

// Invalidate drops cached calendars.
func (s *Service) Invalidate() {
	s.mu.Lock()
	s.cache = map[string]cached{}
	s.mu.Unlock()
}

func normalize(q Query, now time.Time) Query {
	if q.Location == nil {
		q.Location = time.UTC
	}
	today := dateOf(now.In(q.Location))
	if q.From.IsZero() {
		q.From = today.AddDate(0, 0, -7)
	}
	if q.To.IsZero() || q.To.Before(q.From) {
		q.To = q.From.AddDate(0, 0, 41)
	}
	if q.To.Sub(q.From) > maxSpan {
		q.To = q.From.Add(maxSpan)
	}
	q.From, q.To = dateOf(q.From), dateOf(q.To)
	switch q.Types {
	case "movie", "show":
	default:
		q.Types = ""
	}
	switch q.Movies {
	case "popular", "watchlist", "hidden":
	default:
		q.Movies = "foryou"
	}
	return q
}

func (q Query) cacheKey() string {
	return strings.Join([]string{q.From.Format(time.DateOnly), q.To.Format(time.DateOnly), q.Location.String(), q.Types, q.Movies, strconv.FormatInt(q.Viewer, 10)}, "|")
}

// dateOf is the calendar date of t in t's location, as midnight UTC: TMDB
// air and release dates are dates without a time and are stored that way.
func dateOf(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

type interest struct {
	reasons map[string]bool
	viewers map[int64]bool
	// collections is the Look Ahead collections listing a movie.
	collections []string
}

func (in *interest) add(viewer int64, reason string) {
	if in.reasons == nil {
		in.reasons = map[string]bool{}
		in.viewers = map[int64]bool{}
	}
	in.reasons[reason] = true
	if viewer != 0 {
		in.viewers[viewer] = true
	}
}

func (s *Service) build(ctx context.Context, q Query, now time.Time) (*Response, error) {
	users, err := s.o.Viewers(ctx)
	if err != nil {
		return nil, err
	}
	resp := &Response{
		From: q.From.Format(time.DateOnly), To: q.To.Format(time.DateOnly),
		Today: dateOf(now.In(q.Location)).Format(time.DateOnly), Viewers: []Viewer{}, Entries: []Entry{}, GeneratedAt: now.UTC(),
	}
	s.mu.Lock()
	resp.Warming = s.lastPass.IsZero()
	s.mu.Unlock()
	scope := map[int64]*core.User{}
	for i := range users {
		u := users[i]
		resp.Viewers = append(resp.Viewers, Viewer{ID: u.ID, Name: displayName(u), Thumb: u.Thumb, Owner: u.IsOwner})
		if q.Viewer == 0 || q.Viewer == u.ID {
			scope[u.ID] = &u
		}
	}
	sort.SliceStable(resp.Viewers, func(i, j int) bool {
		if resp.Viewers[i].Owner != resp.Viewers[j].Owner {
			return resp.Viewers[i].Owner
		}
		return strings.ToLower(resp.Viewers[i].Name) < strings.ToLower(resp.Viewers[j].Name)
	})
	if len(scope) == 0 {
		return resp, nil
	}

	shows := map[core.RatingKey]*interest{}
	movies := map[core.RatingKey]*interest{}
	var showOrder []core.RatingKey
	follow := func(k core.RatingKey, viewer int64, reason string) {
		in := shows[k]
		if in == nil {
			in = &interest{}
			shows[k] = in
			showOrder = append(showOrder, k)
		}
		in.add(viewer, reason)
	}
	acts, err := s.o.Followed(ctx, now.Add(-followWindow))
	if err != nil {
		return nil, err
	}
	for _, a := range acts {
		if scope[a.UserID] != nil {
			follow(a.Show, a.UserID, ReasonWatching)
		}
	}
	if s.o.Watchlisted != nil {
		lists, err := s.o.Watchlisted(ctx)
		if err != nil {
			return nil, err
		}
		for uid, keys := range lists {
			if scope[uid] == nil {
				continue
			}
			for _, k := range keys {
				switch k.Kind() {
				case core.KindShow:
					follow(k, uid, ReasonWatchlist)
				case core.KindMovie:
					in := movies[k]
					if in == nil {
						in = &interest{}
						movies[k] = in
					}
					in.add(uid, ReasonWatchlist)
				}
			}
		}
	}
	resp.Shows = len(shows)

	from, until := q.From, q.To.AddDate(0, 0, 1)
	var entries []Entry

	if q.Types != "movie" && len(showOrder) > 0 {
		eps, err := s.o.Catalog.ScheduledShows(ctx, showOrder, from, until, maxEntries+1)
		if err != nil {
			return nil, err
		}
		prem, err := s.o.Catalog.ScheduledPremieres(ctx, showOrder, from, until)
		if err != nil {
			return nil, err
		}
		for _, e := range append(eps, prem...) {
			in := shows[e.Title.ShowKey]
			if in == nil {
				continue
			}
			if en, ok := s.entry(ctx, e, in, scope, q, now); ok {
				entries = append(entries, en)
			}
		}
	}

	if q.Types != "show" && q.Movies != "hidden" {
		if q.Movies != "popular" {
			keys := make([]core.RatingKey, 0, len(movies))
			for k := range movies {
				keys = append(keys, k)
			}
			if q.Movies == "foryou" && s.o.LookAheads != nil {
				las, err := s.o.LookAheads(ctx)
				if err != nil {
					return nil, err
				}
				for _, la := range las {
					for _, k := range la.Members {
						if k.Kind() != core.KindMovie {
							continue
						}
						in := movies[k]
						if in == nil {
							in = &interest{}
							movies[k] = in
							keys = append(keys, k)
						}
						in.add(0, ReasonLookAhead)
						if !contains(in.collections, la.Name) {
							in.collections = append(in.collections, la.Name)
						}
					}
				}
			}
			sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
			got, err := s.o.Catalog.ScheduledMovies(ctx, keys, from, until)
			if err != nil {
				return nil, err
			}
			for _, e := range got {
				if en, ok := s.entry(ctx, e, movies[e.Title.Key], scope, q, now); ok {
					entries = append(entries, en)
				}
			}
		}
		if q.Movies == "foryou" || q.Movies == "popular" {
			picks, err := s.discoverMovies(ctx, q, from, until, shows, movies)
			if err != nil {
				return nil, err
			}
			for _, p := range picks {
				if en, ok := s.entry(ctx, p.e, p.in, scope, q, now); ok {
					entries = append(entries, en)
				}
			}
		}
	}

	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if !a.released.Equal(b.released) {
			return a.released.Before(b.released)
		}
		if a.Kind != b.Kind {
			return kindOrder(a.Kind) < kindOrder(b.Kind)
		}
		if a.Kind == KindMovie && a.pop != b.pop {
			return a.pop > b.pop
		}
		if a.Title != b.Title {
			return strings.ToLower(a.Title) < strings.ToLower(b.Title)
		}
		if a.Season != b.Season {
			return a.Season < b.Season
		}
		return a.Episode < b.Episode
	})
	if len(entries) > maxEntries {
		entries = entries[:maxEntries]
		resp.Truncated = true
	}
	if err := s.markWatched(ctx, entries, scope); err != nil {
		return nil, err
	}
	resp.Entries = entries
	return resp, nil
}

func kindOrder(k string) int {
	switch k {
	case KindPremiere:
		return 0
	case KindEpisode:
		return 1
	}
	return 2
}

type pick struct {
	e  catalog.ScheduleEntry
	in *interest
}

// discoverMovies picks popular releases in the range. For "foryou" they
// must share a genre with what the household follows; "popular" takes the
// most popular regardless. Titles already on the calendar are skipped.
func (s *Service) discoverMovies(ctx context.Context, q Query, from, until time.Time, shows, movies map[core.RatingKey]*interest) ([]pick, error) {
	weeks := int(until.Sub(from).Hours()/(24*7)) + 1
	limit := weeks * 6
	if q.Movies == "foryou" {
		limit = weeks * 3
	}
	pool, err := s.o.Catalog.PopularScheduledMovies(ctx, from, until, max(limit*8, 60))
	if err != nil {
		return nil, err
	}
	var taste map[string]float64
	if q.Movies == "foryou" {
		taste, err = s.taste(ctx, shows, movies)
		if err != nil {
			return nil, err
		}
		if len(taste) == 0 {
			return nil, nil
		}
	}
	type scored struct {
		pick
		score float64
	}
	var out []scored
	for _, e := range pool {
		if movies[e.Title.Key] != nil {
			continue
		}
		score := e.Title.Popularity
		reason := ReasonPopular
		if taste != nil {
			match := 0.0
			for _, g := range e.Title.Genres {
				match += taste[g]
			}
			if match == 0 {
				continue
			}
			score *= 1 + match
			reason = ReasonTaste
		}
		in := &interest{}
		in.add(0, reason)
		out = append(out, scored{pick{e, in}, score})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].score > out[j].score })
	if len(out) > limit {
		out = out[:limit]
	}
	picks := make([]pick, len(out))
	for i, o := range out {
		picks[i] = o.pick
	}
	return picks, nil
}

// taste weighs genres by how much of what the household follows carries
// them, normalised so the strongest genre is 1. Only the top genres count.
func (s *Service) taste(ctx context.Context, shows, movies map[core.RatingKey]*interest) (map[string]float64, error) {
	keys := make([]core.RatingKey, 0, len(shows)+len(movies))
	for k := range shows {
		keys = append(keys, k)
	}
	for k := range movies {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	if len(keys) > 400 {
		keys = keys[:400]
	}
	titles, err := s.o.Catalog.CachedGetMany(ctx, keys)
	if err != nil {
		return nil, err
	}
	counts := map[string]float64{}
	for _, t := range titles {
		for _, g := range t.Genres {
			counts[g]++
		}
	}
	type gc struct {
		g string
		n float64
	}
	var ranked []gc
	for g, n := range counts {
		ranked = append(ranked, gc{g, n})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].n != ranked[j].n {
			return ranked[i].n > ranked[j].n
		}
		return ranked[i].g < ranked[j].g
	})
	if len(ranked) > 4 {
		ranked = ranked[:4]
	}
	out := map[string]float64{}
	for _, r := range ranked {
		out[r.g] = r.n / ranked[0].n
	}
	return out, nil
}

// entry turns a mirrored title into a calendar entry for the viewers in
// scope it is for and allowed to see. Interest without viewers (Look Ahead,
// taste, popular) is for everyone in scope.
func (s *Service) entry(ctx context.Context, e catalog.ScheduleEntry, in *interest, scope map[int64]*core.User, q Query, now time.Time) (Entry, bool) {
	if in == nil {
		return Entry{}, false
	}
	t := e.Title
	var viewers []int64
	for id, u := range scope {
		if len(in.viewers) > 0 && !in.viewers[id] {
			continue
		}
		if s.o.Allowed(ctx, u, &t) {
			viewers = append(viewers, id)
		}
	}
	if len(viewers) == 0 {
		return Entry{}, false
	}
	sort.Slice(viewers, func(i, j int) bool { return viewers[i] < viewers[j] })
	date := t.Released.UTC().Format(time.DateOnly)
	today := dateOf(now.In(q.Location)).Format(time.DateOnly)
	en := Entry{
		ID: t.Key.String(), Key: t.Key.String(), Open: t.Key.String(), Date: date, Year: t.Year,
		Overview: t.Summary, Runtime: int(t.Runtime / time.Minute), ContentRating: t.ContentRating,
		Rating: float64(int(t.Rating*10+0.5)) / 10, Genres: t.Genres, Viewers: viewers,
		Poster: s.o.Image(t.PosterPath, 342), Backdrop: s.o.Image(t.BackdropPath, 780),
		released: t.Released, pop: t.Popularity,
	}
	switch t.Type {
	case core.TypeEpisode:
		en.Kind = KindEpisode
		en.Open = t.ShowKey.String()
		en.Title = t.ShowTitle
		en.EpisodeTitle = t.Title
		en.Season, en.Episode = t.Season, t.Episode
		en.Still = s.o.Image(t.StillPath, 780)
		if en.Still == "" {
			en.Still = s.o.Image(t.PosterPath, 780)
		}
		// Episode rows carry the still as their poster; show the series'.
		en.Poster = s.o.Image(e.ShowPoster, 342)
		switch {
		case t.Episode == 1 && t.Season == 1:
			en.Badges = append(en.Badges, BadgeSeriesPremiere)
		case t.Episode == 1:
			en.Badges = append(en.Badges, BadgeSeasonPremiere)
		}
		if e.SeasonEpisodes > 1 && t.Episode == e.SeasonEpisodes {
			en.Badges = append(en.Badges, BadgeFinale)
		}
	case core.TypeSeason:
		en.Kind = KindPremiere
		en.Poster = s.o.Image(e.ShowPoster, 342)
		en.ID = "premiere:" + t.Key.String()
		en.Open = t.ShowKey.String()
		en.Title = t.ShowTitle
		en.EpisodeTitle = t.Title
		en.Season = t.Season
		if t.Season == 1 {
			en.Badges = append(en.Badges, BadgeSeriesPremiere)
		} else {
			en.Badges = append(en.Badges, BadgeSeasonPremiere)
		}
	case core.TypeMovie:
		en.Kind = KindMovie
		en.Title = t.Title
	default:
		return Entry{}, false
	}
	switch {
	case date > today:
		en.State = StateUpcoming
	case date == today:
		en.State = StateToday
	case e.Playable && en.Kind != KindPremiere:
		en.State = StateAvailable
	default:
		en.State = StateUnavailable
	}
	for _, r := range []string{ReasonWatching, ReasonWatchlist, ReasonLookAhead, ReasonTaste, ReasonPopular} {
		if in.reasons[r] {
			en.Reasons = append(en.Reasons, r)
		}
	}
	en.Collections = in.collections
	return en, true
}

// markWatched records which viewers in scope have watched each entry.
func (s *Service) markWatched(ctx context.Context, entries []Entry, scope map[int64]*core.User) error {
	if s.o.Watched == nil || len(entries) == 0 {
		return nil
	}
	byViewer := map[int64][]core.RatingKey{}
	for _, e := range entries {
		if e.Kind == KindPremiere || e.State == StateUpcoming {
			continue
		}
		k, err := core.ParseRatingKey(e.Key)
		if err != nil {
			continue
		}
		for _, v := range e.Viewers {
			byViewer[v] = append(byViewer[v], k)
		}
	}
	ids := make([]int64, 0, len(byViewer))
	for id := range byViewer {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	watched := map[string][]int64{}
	for _, id := range ids {
		st, err := s.o.Watched(ctx, id, byViewer[id])
		if err != nil {
			return err
		}
		for k, ws := range st {
			if ws.ViewCount > 0 {
				watched[k.String()] = append(watched[k.String()], id)
			}
		}
	}
	for i := range entries {
		entries[i].Watched = watched[entries[i].Key]
	}
	return nil
}

func displayName(u core.User) string {
	if u.Title != "" {
		return u.Title
	}
	return u.Username
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
