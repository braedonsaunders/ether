package catalog_test

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/tmdb"
)

func day(d int) time.Time { return time.Date(2026, 11, d, 0, 0, 0, 0, time.UTC) }

func TestScheduledShowsReadsTheMirrorWindow(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC)
	c, err := catalog.New(store.OpenTest(t), nil, config.Catalog{}, slog.New(slog.DiscardHandler), catalog.WithNow(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	show := core.Title{Key: core.ShowKey(900), Type: core.TypeShow, Title: "Night Desk", Released: day(1).AddDate(-2, 0, 0), ContentRating: "TV-MA", PosterPath: "/show.jpg", BackdropPath: "/art.jpg", Status: "Returning Series"}
	other := core.Title{Key: core.ShowKey(901), Type: core.TypeShow, Title: "Not followed", Released: show.Released}
	rows := []core.Title{show, other,
		{Key: core.SeasonKey(900, 2), Type: core.TypeSeason, ShowKey: show.Key, Season: 2, Released: day(3)},
		{Key: core.SeasonKey(900, 3), Type: core.TypeSeason, ShowKey: show.Key, Season: 3, Released: day(20), Title: "Season 3"},
		{Key: core.SeasonKey(901, 4), Type: core.TypeSeason, ShowKey: other.Key, Season: 4, Released: day(21)},
	}
	for i, d := range []time.Time{day(3), day(10), day(17)} {
		rows = append(rows, core.Title{Key: core.EpisodeKey(900, 2, i+1), Type: core.TypeEpisode, ShowKey: show.Key, ShowTitle: show.Title, Season: 2, Episode: i + 1, Title: fmt.Sprintf("Part %d", i+1), Released: d, PosterPath: "/still.jpg"})
	}
	rows = append(rows,
		core.Title{Key: core.EpisodeKey(900, 2, 4), Type: core.TypeEpisode, ShowKey: show.Key, Season: 2, Episode: 4, Released: day(28)}, // outside the window
		core.Title{Key: core.EpisodeKey(900, 0, 1), Type: core.TypeEpisode, ShowKey: show.Key, Season: 0, Episode: 1, Released: day(5)},  // specials are skipped
		core.Title{Key: core.EpisodeKey(901, 1, 1), Type: core.TypeEpisode, ShowKey: other.Key, Season: 1, Episode: 1, Released: day(5)},
	)
	for _, r := range rows {
		if err := c.UpsertTitle(ctx, r, r.Type == core.TypeShow); err != nil {
			t.Fatal(err)
		}
	}
	got, err := c.ScheduledShows(ctx, []core.RatingKey{show.Key}, day(1), day(25), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("episodes: %+v", got)
	}
	for i, e := range got {
		if e.Title.Episode != i+1 || e.Title.ContentRating != "TV-MA" || e.ShowPoster != "/show.jpg" || e.Title.BackdropPath != "/art.jpg" || e.SeasonEpisodes != 4 {
			t.Fatalf("episode %d: %+v", i, e)
		}
	}
	if !got[0].Playable || !got[1].Playable || got[2].Playable {
		t.Fatalf("playable: %v %v %v", got[0].Playable, got[1].Playable, got[2].Playable)
	}
	if lim, err := c.ScheduledShows(ctx, []core.RatingKey{show.Key}, day(1), day(25), 2); err != nil || len(lim) != 2 {
		t.Fatalf("limit: %d %v", len(lim), err)
	}

	prem, err := c.ScheduledPremieres(ctx, []core.RatingKey{show.Key}, day(1), day(25))
	if err != nil || len(prem) != 1 || prem[0].Title.Season != 3 || prem[0].Title.ShowTitle != "Night Desk" || prem[0].ShowPoster != "/show.jpg" {
		t.Fatalf("premieres (season 2 has episodes, season 3 none): %+v %v", prem, err)
	}
}

func TestScheduledMoviesAndPopularReleases(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC)
	c, err := catalog.New(store.OpenTest(t), nil, config.Catalog{}, slog.New(slog.DiscardHandler), catalog.WithNow(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	movies := []core.Title{
		{Key: core.MovieKey(1), Type: core.TypeMovie, Title: "Out now", Released: day(2), Popularity: 40},
		{Key: core.MovieKey(2), Type: core.TypeMovie, Title: "Soon", Released: day(20), Popularity: 90},
		{Key: core.MovieKey(3), Type: core.TypeMovie, Title: "Adult", Released: day(4), Popularity: 99, Adult: true},
		{Key: core.MovieKey(4), Type: core.TypeMovie, Title: "Long ago", Released: day(1).AddDate(-1, 0, 0), Popularity: 80},
		{Key: core.MovieKey(5), Type: core.TypeMovie, Title: "Unrated", Released: day(6)},
	}
	for _, m := range movies {
		if err := c.UpsertTitle(ctx, m, true); err != nil {
			t.Fatal(err)
		}
	}
	got, err := c.ScheduledMovies(ctx, []core.RatingKey{core.MovieKey(1), core.MovieKey(3), core.MovieKey(4), core.MovieKey(5)}, day(1), day(30))
	if err != nil || len(got) != 2 {
		t.Fatalf("by key: %+v %v", got, err)
	}
	pop, err := c.PopularScheduledMovies(ctx, day(1), day(30), 10)
	if err != nil || len(pop) != 2 || pop[0].Title.Key != core.MovieKey(2) || pop[1].Title.Key != core.MovieKey(1) {
		t.Fatalf("popular: %+v %v", pop, err)
	}
	if !pop[1].Playable || pop[0].Playable {
		t.Fatalf("playable: %+v", pop)
	}
}

type scheduleSource struct {
	catalog.TMDB
	status  string
	lastAir string
	seasons map[int]string // season -> premiere date
	shows   int
	fetched []int
}

func (f *scheduleSource) TVDetails(ctx context.Context, id int64) (*tmdb.TVDetails, error) {
	f.shows++
	d := &tmdb.TVDetails{TVShow: tmdb.TVShow{ID: id, Name: "Followed", FirstAirDate: "2024-01-01"}, Status: f.status, LastAirDate: f.lastAir}
	for n := 1; n <= len(f.seasons); n++ {
		d.Seasons = append(d.Seasons, tmdb.SeasonSummary{ID: int64(n), SeasonNumber: n, EpisodeCount: 2, AirDate: f.seasons[n]})
	}
	return d, nil
}
func (f *scheduleSource) TVDetailsFresh(ctx context.Context, id int64) (*tmdb.TVDetails, error) {
	return f.TVDetails(ctx, id)
}
func (f *scheduleSource) SeasonDetails(ctx context.Context, id int64, season int) (*tmdb.SeasonDetails, error) {
	f.fetched = append(f.fetched, season)
	premiere := tmdb.ParseDate(f.seasons[season])
	d := &tmdb.SeasonDetails{ID: int64(season), SeasonNumber: season, AirDate: f.seasons[season]}
	for e := 1; e <= 2; e++ {
		d.Episodes = append(d.Episodes, tmdb.Episode{ID: int64(season*100 + e), EpisodeNumber: e, SeasonNumber: season, Name: fmt.Sprintf("E%d", e), AirDate: premiere.AddDate(0, 0, 7*(e-1)).Format(time.DateOnly)})
	}
	return d, nil
}

func TestRefreshScheduleMirrorsCurrentAndAnnouncedSeasons(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC)
	f := &scheduleSource{status: "Returning Series", seasons: map[int]string{1: "2025-03-01", 2: "2026-11-01", 3: "2027-02-01"}}
	c, err := catalog.New(store.OpenTest(t), f, config.Catalog{RefreshInterval: config.Duration(24 * time.Hour)}, slog.New(slog.DiscardHandler), catalog.WithNow(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	show := core.ShowKey(700)
	if err := c.RefreshSchedule(ctx, show); err != nil {
		t.Fatal(err)
	}
	// Season 3 is announced but has not premiered, so season 2 is the one airing.
	if fmt.Sprint(f.fetched) != "[3 2]" {
		t.Fatalf("fetched seasons %v", f.fetched)
	}
	got, err := c.ScheduledShows(ctx, []core.RatingKey{show}, day(1), day(30), 0)
	if err != nil || len(got) != 2 || got[0].Title.Season != 2 {
		t.Fatalf("scheduled: %+v %v", got, err)
	}
	// Within the hour nothing is refetched.
	shows, seasons := f.shows, len(f.fetched)
	now = now.Add(30 * time.Minute)
	if err := c.RefreshSchedule(ctx, show); err != nil {
		t.Fatal(err)
	}
	if f.shows != shows || len(f.fetched) != seasons {
		t.Fatalf("fresh refresh fetched: shows %d→%d seasons %d→%d", shows, f.shows, seasons, len(f.fetched))
	}
	if err := c.RefreshSchedule(ctx, core.MovieKey(1)); err != nil {
		t.Fatal(err)
	}
}

func TestRefreshScheduleLeavesSettledShowsAlone(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC)
	f := &scheduleSource{status: "Ended", lastAir: "2026-01-01", seasons: map[int]string{1: "2025-12-01"}}
	c, err := catalog.New(store.OpenTest(t), f, config.Catalog{}, slog.New(slog.DiscardHandler), catalog.WithNow(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.RefreshSchedule(ctx, core.ShowKey(701)); err != nil {
		t.Fatal(err)
	}
	if len(f.fetched) != 0 {
		t.Fatalf("ended show's seasons fetched: %v", f.fetched)
	}
}
