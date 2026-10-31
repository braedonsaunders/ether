package catalog_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/tmdb"
)

// testClock is an adjustable clock for catalogue and mirror reads.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *testClock) set(t time.Time) { c.mu.Lock(); c.t = t; c.mu.Unlock() }

// returningSeries is a TMDB fake for show 700, a series between air dates.
// TMDB's number_of_seasons/number_of_episodes count announced episodes;
// the season payloads carry each episode's own air date. s2e3Date is
// mutable so a test can give an undated episode its air date later.
type returningSeries struct {
	mu       sync.Mutex
	s2e3Date string
}

func (f *returningSeries) server(t *testing.T) *httptest.Server {
	t.Helper()
	ep := func(season, n int, date string) map[string]any {
		e := map[string]any{"id": 70000 + season*100 + n, "name": "Episode", "season_number": season, "episode_number": n}
		if date != "" {
			e["air_date"] = date
		}
		return e
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		s2e3 := f.s2e3Date
		f.mu.Unlock()
		var body any
		switch p := r.URL.Path; {
		case p == "/tv/700":
			body = map[string]any{"id": 700, "name": "Returning Series", "first_air_date": "2020-01-10", "popularity": 50.0, "vote_count": 5000,
				"number_of_seasons": 5, "number_of_episodes": 12,
				"seasons": []any{
					map[string]any{"id": 1, "season_number": 0, "name": "Specials", "episode_count": 2, "air_date": "2019-12-25"},
					map[string]any{"id": 2, "season_number": 1, "episode_count": 2, "air_date": "2020-01-10"},
					map[string]any{"id": 3, "season_number": 2, "episode_count": 3, "air_date": "2026-03-01"},
					map[string]any{"id": 4, "season_number": 3, "episode_count": 2, "air_date": "2026-09-01"},
					map[string]any{"id": 5, "season_number": 4, "episode_count": 1},
					// Premiere date announced as past but no episode dated yet.
					map[string]any{"id": 6, "season_number": 5, "episode_count": 2, "air_date": "2026-02-01"},
				}}
		case p == "/tv/700/season/0":
			body = map[string]any{"id": 1, "season_number": 0, "air_date": "2019-12-25", "episodes": []any{ep(0, 1, "2019-12-25"), ep(0, 2, "")}}
		case p == "/tv/700/season/1":
			body = map[string]any{"id": 2, "season_number": 1, "air_date": "2020-01-10", "episodes": []any{ep(1, 1, "2020-01-10"), ep(1, 2, "2020-01-17")}}
		case p == "/tv/700/season/2":
			body = map[string]any{"id": 3, "season_number": 2, "air_date": "2026-03-01", "episodes": []any{ep(2, 1, "2026-03-01"), ep(2, 2, "2026-03-10"), ep(2, 3, s2e3)}}
		case p == "/tv/700/season/3":
			body = map[string]any{"id": 4, "season_number": 3, "air_date": "2026-09-01", "episodes": []any{ep(3, 1, "2026-09-01"), ep(3, 2, "2026-09-08")}}
		case p == "/tv/700/season/5":
			body = map[string]any{"id": 6, "season_number": 5, "air_date": "2026-02-01", "episodes": []any{ep(5, 1, ""), ep(5, 2, "")}}
		case strings.HasSuffix(p, "/external_ids"):
			body = map[string]any{"imdb_id": "tt0700001"}
		default:
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newAiredCatalog(t *testing.T, srv *httptest.Server, clock *testClock, opts ...catalog.Option) (*catalog.Catalog, *catalog.Mirror) {
	t.Helper()
	db := store.OpenTest(t)
	client := tmdb.New("testkey", slog.Default(), tmdb.WithBaseURL(srv.URL), tmdb.WithCacheTTL(0))
	t.Cleanup(client.Close)
	cfg := config.Catalog{Language: "en-US", Region: "US", MinPopularity: 5, MinVotes: 100, RefreshInterval: config.Duration(24 * time.Hour)}
	c, err := catalog.New(db, client, cfg, slog.Default(), append([]catalog.Option{catalog.WithNow(clock.now)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return c, catalog.NewMirror(db, clock.now)
}

func episodeKeys(ts []core.Title) []core.RatingKey {
	out := make([]core.RatingKey, len(ts))
	for i, t := range ts {
		out[i] = t.Key
	}
	return out
}

func sameKeys(got, want []core.RatingKey) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// A returning series lists, counts and navigates only aired episodes, and
// seasons only once one of their episodes has aired. Detail, list and
// mirror reads agree on the counts.
func TestReturningSeriesListsCountsAndNavigatesAiredEpisodesOnly(t *testing.T) {
	ctx := context.Background()
	clock := &testClock{t: time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)}
	fake := &returningSeries{s2e3Date: "2026-03-17"}
	c, mirror := newAiredCatalog(t, fake.server(t), clock)
	show := core.ShowKey(700)

	all, err := c.Grandchildren(ctx, show)
	if err != nil {
		t.Fatal(err)
	}
	// Specials keep the dated special; S2 has aired through E2 (today);
	// S3 is in the future, S4 undated and S5 has no dated episode.
	want := []core.RatingKey{core.EpisodeKey(700, 0, 1), core.EpisodeKey(700, 1, 1), core.EpisodeKey(700, 1, 2), core.EpisodeKey(700, 2, 1), core.EpisodeKey(700, 2, 2)}
	if got := episodeKeys(all); !sameKeys(got, want) {
		t.Fatalf("grandchildren = %v, want %v", got, want)
	}
	seasons, err := c.Children(ctx, show)
	if err != nil {
		t.Fatal(err)
	}
	if got := episodeKeys(seasons); !sameKeys(got, []core.RatingKey{core.SeasonKey(700, 0), core.SeasonKey(700, 1), core.SeasonKey(700, 2)}) {
		t.Fatalf("seasons = %v", got)
	}
	if seasons[2].ChildCount != 2 {
		t.Fatalf("listed season 2 childCount = %d, want 2 aired", seasons[2].ChildCount)
	}

	checkShow := func(name string, got core.Title, seasons, leaves int) {
		t.Helper()
		if got.ChildCount != seasons || got.LeafCount != leaves {
			t.Fatalf("%s counts = %d seasons/%d episodes, want %d/%d (TMDB announced 5/12)", name, got.ChildCount, got.LeafCount, seasons, leaves)
		}
	}
	detail, err := c.Get(ctx, show)
	if err != nil {
		t.Fatal(err)
	}
	// Show totals count regular seasons only (Specials are listed, not counted).
	checkShow("detail", *detail, 2, 4)
	page, err := c.Browse(ctx, core.BrowseQuery{Filter: core.Filter{Type: core.TypeShow}})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("browse: %+v %v", page, err)
	}
	checkShow("browse", page.Items[0], 2, 4)
	mirrored, err := mirror.Titles(ctx, []core.RatingKey{show, core.SeasonKey(700, 2)})
	if err != nil || len(mirrored) != 2 {
		t.Fatalf("mirror: %+v %v", mirrored, err)
	}
	checkShow("mirror", mirrored[0], 2, 4)
	if mirrored[1].ChildCount != 2 {
		t.Fatalf("mirror season childCount = %d, want 2", mirrored[1].ChildCount)
	}
	s2, err := c.Get(ctx, core.SeasonKey(700, 2))
	if err != nil || s2.ChildCount != 2 {
		t.Fatalf("season 2 detail: %+v %v", s2, err)
	}

	// Direct access to unaired episodes and to seasons with nothing aired.
	for _, k := range []core.RatingKey{
		core.EpisodeKey(700, 2, 3), core.EpisodeKey(700, 0, 2), core.EpisodeKey(700, 3, 1), core.EpisodeKey(700, 5, 1),
		core.SeasonKey(700, 3), core.SeasonKey(700, 4), core.SeasonKey(700, 5),
	} {
		if _, err := c.Get(ctx, k); !errors.Is(err, core.ErrNotFound) {
			t.Fatalf("Get(%v) = %v, want not found", k, err)
		}
		if _, err := c.Children(ctx, k); k.Kind() == core.KindSeason && !errors.Is(err, core.ErrNotFound) {
			t.Fatalf("Children(%v) = %v, want not found", k, err)
		}
	}

	// Next episode stops at the last aired one.
	if _, err := c.NextEpisode(ctx, core.EpisodeKey(700, 2, 2)); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("next after the latest aired episode = %v, want not found", err)
	}
	next, err := c.NextEpisode(ctx, core.EpisodeKey(700, 1, 2))
	if err != nil || next.Key != core.EpisodeKey(700, 2, 1) {
		t.Fatalf("next across seasons = %+v %v", next, err)
	}

	// Air dates are dates: the episode joins at the start (UTC) of its air
	// day, with no refetch, as movies do on their release date.
	clock.set(time.Date(2026, 3, 17, 0, 0, 0, 0, time.UTC))
	next, err = c.NextEpisode(ctx, core.EpisodeKey(700, 2, 2))
	if err != nil || next.Key != core.EpisodeKey(700, 2, 3) {
		t.Fatalf("next on the air date = %+v %v", next, err)
	}
	detail, err = c.Get(ctx, show)
	if err != nil {
		t.Fatal(err)
	}
	checkShow("detail after airing", *detail, 2, 5)
	page, err = c.Browse(ctx, core.BrowseQuery{Filter: core.Filter{Type: core.TypeShow}})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("browse: %+v %v", page, err)
	}
	checkShow("browse after airing", page.Items[0], 2, 5)
}

// An undated episode that was opened directly (and so stored as a detail
// row) picks up the air date TMDB later gives it when its season is
// refreshed, instead of staying hidden on the stale empty date.
func TestEpisodeGainsAirDateOnSeasonRefresh(t *testing.T) {
	ctx := context.Background()
	clock := &testClock{t: time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)}
	fake := &returningSeries{} // S2E3 not dated yet
	c, _ := newAiredCatalog(t, fake.server(t), clock, catalog.WithStaleAfter(time.Hour))
	if _, err := c.Get(ctx, core.EpisodeKey(700, 2, 3)); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("undated episode = %v, want not found", err)
	}
	if eps, err := c.Children(ctx, core.SeasonKey(700, 2)); err != nil || len(eps) != 2 {
		t.Fatalf("season 2 before dating: %d %v", len(eps), err)
	}

	fake.mu.Lock()
	fake.s2e3Date = "2026-03-12"
	fake.mu.Unlock()
	clock.set(time.Date(2026, 3, 12, 18, 0, 0, 0, time.UTC))
	// The stale season is served as stored and refreshed behind the read.
	if _, err := c.Children(ctx, core.SeasonKey(700, 2)); err != nil {
		t.Fatal(err)
	}
	c.WaitRefreshes()
	eps, err := c.Children(ctx, core.SeasonKey(700, 2))
	if err != nil || len(eps) != 3 || eps[2].Key != core.EpisodeKey(700, 2, 3) {
		t.Fatalf("season 2 after dating: %v %v", episodeKeys(eps), err)
	}
	ep, err := c.Get(ctx, core.EpisodeKey(700, 2, 3))
	if err != nil || ep.Released.Format("2006-01-02") != "2026-03-12" || ep.Year != 2026 {
		t.Fatalf("dated episode: %+v %v", ep, err)
	}
	s2, err := c.Get(ctx, core.SeasonKey(700, 2))
	if err != nil || s2.ChildCount != 3 {
		t.Fatalf("season 2 count after dating: %+v %v", s2, err)
	}
}
