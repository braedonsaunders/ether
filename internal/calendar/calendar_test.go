package calendar

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/core"
)

func day(d int) time.Time { return time.Date(2026, 11, d, 0, 0, 0, 0, time.UTC) }

type fakeCatalog struct {
	titles    []catalog.ScheduleEntry
	stored    map[core.RatingKey]core.Title
	refreshed []core.RatingKey
	calls     int
}

func (f *fakeCatalog) window(keep func(core.Title) bool, from, until time.Time) []catalog.ScheduleEntry {
	var out []catalog.ScheduleEntry
	for _, e := range f.titles {
		if keep(e.Title) && !e.Title.Released.Before(from) && e.Title.Released.Before(until) {
			out = append(out, e)
		}
	}
	return out
}

func (f *fakeCatalog) ScheduledShows(_ context.Context, shows []core.RatingKey, from, until time.Time, limit int) ([]catalog.ScheduleEntry, error) {
	f.calls++
	out := f.window(func(t core.Title) bool { return t.Type == core.TypeEpisode && slices.Contains(shows, t.ShowKey) }, from, until)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
func (f *fakeCatalog) ScheduledPremieres(_ context.Context, shows []core.RatingKey, from, until time.Time) ([]catalog.ScheduleEntry, error) {
	return f.window(func(t core.Title) bool { return t.Type == core.TypeSeason && slices.Contains(shows, t.ShowKey) }, from, until), nil
}
func (f *fakeCatalog) ScheduledMovies(_ context.Context, keys []core.RatingKey, from, until time.Time) ([]catalog.ScheduleEntry, error) {
	return f.window(func(t core.Title) bool { return t.Type == core.TypeMovie && slices.Contains(keys, t.Key) }, from, until), nil
}
func (f *fakeCatalog) PopularScheduledMovies(_ context.Context, from, until time.Time, limit int) ([]catalog.ScheduleEntry, error) {
	out := f.window(func(t core.Title) bool { return t.Type == core.TypeMovie && t.Popularity > 0 }, from, until)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Title.Popularity > out[j].Title.Popularity })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
func (f *fakeCatalog) CachedGetMany(_ context.Context, keys []core.RatingKey) (map[core.RatingKey]core.Title, error) {
	out := map[core.RatingKey]core.Title{}
	for _, k := range keys {
		if t, ok := f.stored[k]; ok {
			out[k] = t
		}
	}
	return out, nil
}
func (f *fakeCatalog) RefreshSchedule(_ context.Context, show core.RatingKey) error {
	f.refreshed = append(f.refreshed, show)
	return nil
}

var (
	showA = core.ShowKey(100) // TV-MA drama, watched by Alex (1)
	showB = core.ShowKey(200) // watchlisted by Sam (2)
)

func episode(show core.RatingKey, title string, season, ep int, released time.Time, playable bool, size int) catalog.ScheduleEntry {
	return catalog.ScheduleEntry{Title: core.Title{
		Key: core.EpisodeKey(show.TMDB(), season, ep), Type: core.TypeEpisode, ShowKey: show, ShowTitle: title,
		Season: season, Episode: ep, Title: fmt.Sprintf("Episode %d", ep), Released: released, ContentRating: map[core.RatingKey]string{showA: "TV-MA", showB: "TV-PG"}[show],
		StillPath: "/still.jpg", BackdropPath: "/art.jpg",
	}, Playable: playable, SeasonEpisodes: size, ShowPoster: "/poster.jpg"}
}

func movie(id int64, title string, released time.Time, pop float64, rating string, genres ...string) catalog.ScheduleEntry {
	return catalog.ScheduleEntry{Title: core.Title{Key: core.MovieKey(id), Type: core.TypeMovie, Title: title, Released: released, Popularity: pop, ContentRating: rating, Genres: genres, PosterPath: "/m.jpg"}, Playable: released.Before(day(10))}
}

type fixture struct {
	svc     *Service
	cat     *fakeCatalog
	now     *time.Time
	watched map[int64]map[core.RatingKey]bool
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	now := time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC)
	cat := &fakeCatalog{
		titles: []catalog.ScheduleEntry{
			episode(showA, "Night Desk", 2, 1, day(3), true, 3),
			episode(showA, "Night Desk", 2, 2, day(10), true, 3),
			episode(showA, "Night Desk", 2, 3, day(17), false, 3),
			{Title: core.Title{Key: core.SeasonKey(100, 3), Type: core.TypeSeason, ShowKey: showA, ShowTitle: "Night Desk", Season: 3, Title: "Season 3", Released: day(24), ContentRating: "TV-MA"}, ShowPoster: "/poster.jpg"},
			episode(showB, "Little Engines", 1, 1, day(12), false, 8),
			movie(1, "Watchlisted Thriller", day(14), 30, "R", "Thriller"),
			movie(2, "Look Ahead Pick", day(20), 20, "PG", "Comedy"),
			movie(3, "Popular Drama", day(6), 90, "PG-13", "Drama"),
			movie(4, "Popular Horror", day(8), 95, "R", "Horror"),
			movie(5, "In Cinemas", day(2), 10, "PG", "Drama"),
		},
		stored: map[core.RatingKey]core.Title{
			showA: {Key: showA, Type: core.TypeShow, Genres: []string{"Drama", "Crime"}},
			showB: {Key: showB, Type: core.TypeShow, Genres: []string{"Animation", "Family"}},
		},
	}
	cat.titles[len(cat.titles)-1].Playable = false // released in cinemas, not streamable yet
	f := &fixture{cat: cat, now: &now, watched: map[int64]map[core.RatingKey]bool{1: {core.EpisodeKey(100, 2, 1): true}}}
	users := []core.User{
		{ID: 1, Title: "Alex", IsOwner: true},
		{ID: 2, Title: "Sam"},
		{ID: 3, Title: "The Kids", MaxContentRating: "PG"},
	}
	ranks := map[string]int{"G": 1, "TV-PG": 2, "PG": 2, "PG-13": 3, "TV-14": 3, "R": 4, "TV-MA": 4}
	f.svc = New(Options{
		Catalog: cat,
		Viewers: func(context.Context) ([]core.User, error) { return users, nil },
		Followed: func(_ context.Context, since time.Time) ([]Activity, error) {
			return []Activity{{UserID: 1, Show: showA, LastViewed: now.Add(-24 * time.Hour)}}, nil
		},
		Watchlisted: func(context.Context) (map[int64][]core.RatingKey, error) {
			return map[int64][]core.RatingKey{2: {showB, core.MovieKey(1)}}, nil
		},
		LookAheads: func(context.Context) ([]LookAhead, error) {
			return []LookAhead{{Name: "Coming soon", Members: []core.RatingKey{core.MovieKey(2), showA}}}, nil
		},
		Watched: func(_ context.Context, id int64, keys []core.RatingKey) (map[core.RatingKey]core.WatchStatus, error) {
			out := map[core.RatingKey]core.WatchStatus{}
			for _, k := range keys {
				if f.watched[id][k] {
					out[k] = core.WatchStatus{Key: k, ViewCount: 1}
				}
			}
			return out, nil
		},
		Allowed: func(_ context.Context, u *core.User, t *core.Title) bool {
			return u.MaxContentRating == "" || ranks[t.ContentRating] <= ranks[u.MaxContentRating]
		},
		Image: func(path string, w int) string {
			if path == "" {
				return ""
			}
			return fmt.Sprintf("img%s@%d", path, w)
		},
		Now: func() time.Time { return *f.now },
	})
	return f
}

func (f *fixture) get(t *testing.T, q Query) *Response {
	t.Helper()
	if q.From.IsZero() {
		q.From, q.To = day(1), day(30)
	}
	r, err := f.svc.Calendar(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func byTitle(r *Response) map[string]Entry {
	out := map[string]Entry{}
	for _, e := range r.Entries {
		k := e.Title
		if e.Kind != KindMovie {
			k = fmt.Sprintf("%s %dx%d", e.Title, e.Season, e.Episode)
		}
		out[k] = e
	}
	return out
}

func TestHouseholdCalendarAssemblesWhatViewersFollow(t *testing.T) {
	f := newFixture(t)
	r := f.get(t, Query{})
	got := byTitle(r)
	want := []string{"Night Desk 2x1", "Night Desk 2x2", "Night Desk 2x3", "Night Desk 3x0", "Little Engines 1x1", "Watchlisted Thriller", "Look Ahead Pick", "Popular Drama", "In Cinemas"}
	if len(got) != len(want) {
		t.Fatalf("entries: %v", keys(got))
	}
	for _, w := range want {
		if _, ok := got[w]; !ok {
			t.Fatalf("missing %q in %v", w, keys(got))
		}
	}
	if _, ok := got["Popular Horror"]; ok {
		t.Fatal("a popular movie outside the household's taste is on the For you calendar")
	}
	// Ordered by date.
	for i := 1; i < len(r.Entries); i++ {
		if r.Entries[i].Date < r.Entries[i-1].Date {
			t.Fatalf("order: %s before %s", r.Entries[i-1].Date, r.Entries[i].Date)
		}
	}
	if r.Today != "2026-11-10" || r.Shows != 2 || len(r.Viewers) != 3 || !r.Viewers[0].Owner || !r.Warming {
		t.Fatalf("header: %+v", r)
	}

	ep := got["Night Desk 2x1"]
	if ep.Kind != KindEpisode || ep.Open != showA.String() || ep.State != StateAvailable || ep.EpisodeTitle != "Episode 1" ||
		ep.Poster != "img/poster.jpg@342" || ep.Still != "img/still.jpg@780" || !slices.Equal(ep.Badges, []string{BadgeSeasonPremiere}) ||
		!slices.Equal(ep.Viewers, []int64{1}) || !slices.Equal(ep.Watched, []int64{1}) || !slices.Equal(ep.Reasons, []string{ReasonWatching}) {
		t.Fatalf("aired premiere: %+v", ep)
	}
	if e := got["Night Desk 2x2"]; e.State != StateToday || len(e.Watched) != 0 {
		t.Fatalf("today: %+v", e)
	}
	if e := got["Night Desk 2x3"]; e.State != StateUpcoming || !slices.Equal(e.Badges, []string{BadgeFinale}) {
		t.Fatalf("finale: %+v", e)
	}
	if e := got["Night Desk 3x0"]; e.Kind != KindPremiere || e.State != StateUpcoming || !slices.Equal(e.Badges, []string{BadgeSeasonPremiere}) || e.Poster != "img/poster.jpg@342" {
		t.Fatalf("premiere: %+v", e)
	}
	// The kids' rating cap keeps them off everything rated above PG.
	if e := got["Little Engines 1x1"]; !slices.Equal(e.Viewers, []int64{2}) || !slices.Equal(e.Badges, []string{BadgeSeriesPremiere}) || !slices.Equal(e.Reasons, []string{ReasonWatchlist}) {
		t.Fatalf("watchlisted show: %+v", e)
	}
	if e := got["Look Ahead Pick"]; !slices.Equal(e.Viewers, []int64{1, 2, 3}) || !slices.Equal(e.Reasons, []string{ReasonLookAhead}) || !slices.Equal(e.Collections, []string{"Coming soon"}) {
		t.Fatalf("look ahead: %+v", e)
	}
	if e := got["Popular Drama"]; !slices.Equal(e.Reasons, []string{ReasonTaste}) || !slices.Equal(e.Viewers, []int64{1, 2}) {
		t.Fatalf("taste pick: %+v", e)
	}
	if e := got["In Cinemas"]; e.State != StateUnavailable {
		t.Fatalf("cinema-only release: %+v", e)
	}
	if e := got["Watchlisted Thriller"]; !slices.Equal(e.Viewers, []int64{2}) || e.State != StateUpcoming {
		t.Fatalf("watchlisted movie: %+v", e)
	}
}

func TestCalendarFilters(t *testing.T) {
	f := newFixture(t)
	kids := byTitle(f.get(t, Query{Viewer: 3}))
	if len(kids) != 1 || kids["Look Ahead Pick"].Title == "" {
		t.Fatalf("restricted viewer: %v", keys(kids))
	}
	sam := byTitle(f.get(t, Query{Viewer: 2}))
	if _, ok := sam["Night Desk 2x1"]; ok {
		t.Fatalf("Sam does not follow Night Desk: %v", keys(sam))
	}
	shows := f.get(t, Query{Types: "show"})
	for _, e := range shows.Entries {
		if e.Kind == KindMovie {
			t.Fatalf("show filter kept %s", e.Title)
		}
	}
	hidden := f.get(t, Query{Movies: "hidden"})
	for _, e := range hidden.Entries {
		if e.Kind == KindMovie {
			t.Fatalf("hidden movies kept %s", e.Title)
		}
	}
	lists := byTitle(f.get(t, Query{Movies: "watchlist", Types: "movie"}))
	if len(lists) != 1 || lists["Watchlisted Thriller"].Title == "" {
		t.Fatalf("watchlist movies: %v", keys(lists))
	}
	popular := byTitle(f.get(t, Query{Movies: "popular", Types: "movie"}))
	if popular["Popular Horror"].Title == "" || popular["Look Ahead Pick"].Title == "" {
		t.Fatalf("popular movies: %v", keys(popular))
	}
	// A popular movie that is also watchlisted is still a popular release.
	if e := popular["Watchlisted Thriller"]; !slices.Equal(e.Reasons, []string{ReasonPopular}) || len(e.Viewers) != 2 {
		t.Fatalf("watchlisted popular movie: %+v", e)
	}
	if e := popular["Popular Horror"]; !slices.Equal(e.Reasons, []string{ReasonPopular}) {
		t.Fatalf("popular reason: %+v", e)
	}
	nobody := f.get(t, Query{Viewer: 99})
	if len(nobody.Entries) != 0 || len(nobody.Viewers) != 3 {
		t.Fatalf("unknown viewer: %+v", nobody)
	}
}

func TestTodayFollowsTheOwnersTimeZone(t *testing.T) {
	f := newFixture(t)
	*f.now = time.Date(2026, 11, 10, 3, 0, 0, 0, time.UTC) // still the 9th in Toronto
	toronto, err := time.LoadLocation("America/Toronto")
	if err != nil {
		t.Skip("no tzdata")
	}
	r := f.get(t, Query{Location: toronto})
	if r.Today != "2026-11-09" || byTitle(r)["Night Desk 2x2"].State != StateUpcoming {
		t.Fatalf("toronto: today %s, %+v", r.Today, byTitle(r)["Night Desk 2x2"])
	}
	utc := f.get(t, Query{})
	if byTitle(utc)["Night Desk 2x2"].State != StateToday {
		t.Fatalf("utc: %+v", byTitle(utc)["Night Desk 2x2"])
	}
}

func TestCalendarIsCachedAndBounded(t *testing.T) {
	f := newFixture(t)
	a := f.get(t, Query{})
	b := f.get(t, Query{})
	if a != b || f.cat.calls != 1 {
		t.Fatalf("not cached: %d calls", f.cat.calls)
	}
	*f.now = f.now.Add(cacheTTL)
	f.get(t, Query{})
	if f.cat.calls != 2 {
		t.Fatalf("stale cache reused: %d calls", f.cat.calls)
	}
	f.svc.Invalidate()
	f.get(t, Query{})
	if f.cat.calls != 3 {
		t.Fatalf("invalidate: %d calls", f.cat.calls)
	}
	q := normalize(Query{From: day(1), To: day(1).AddDate(1, 0, 0)}, *f.now)
	if q.To.Sub(q.From) > maxSpan || q.Movies != "foryou" || q.Location != time.UTC {
		t.Fatalf("normalize: %+v", q)
	}
	d := normalize(Query{}, *f.now)
	if d.From != day(3) || d.To != day(3).AddDate(0, 0, 41) {
		t.Fatalf("default range: %s – %s", d.From, d.To)
	}
	for i := 0; i < cacheSize+5; i++ {
		f.get(t, Query{From: day(1).AddDate(0, 0, i), To: day(2).AddDate(0, 0, i)})
	}
	if n := len(f.svc.cache); n > cacheSize {
		t.Fatalf("cache grew to %d", n)
	}
}

func TestRefreshMirrorsFollowedShowsThenStopsWarming(t *testing.T) {
	f := newFixture(t)
	f.svc.Refresh(context.Background())
	if !slices.Equal(f.cat.refreshed, []core.RatingKey{showA, showB}) {
		t.Fatalf("refreshed %v", f.cat.refreshed)
	}
	if f.get(t, Query{}).Warming {
		t.Fatal("still warming after a pass")
	}
}

func TestHandler(t *testing.T) {
	f := newFixture(t)
	h := f.svc.Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/?from=2026-11-01&to=2026-11-30&viewer=2&tz=America/Toronto&movies=watchlist", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var r Response
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	if r.From != "2026-11-01" || r.To != "2026-11-30" || len(r.Entries) != 2 {
		t.Fatalf("response: %+v", r)
	}
	for _, bad := range []string{"/?from=yesterday", "/?viewer=sam", "/?to=2026-13-01"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, bad, nil))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d", bad, rec.Code)
		}
	}
}

func keys(m map[string]Entry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
