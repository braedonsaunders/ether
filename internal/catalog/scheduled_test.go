package catalog_test

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestScheduledEpisodesDoNotChangeReleasedCatalog(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	c, err := catalog.New(db, nil, config.Catalog{}, slog.Default(), catalog.WithNow(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	show := core.Title{Key: core.ShowKey(904), Type: core.TypeShow, Title: "Airing", Released: now.Add(-72 * time.Hour), ContentRating: "TV-14", Keywords: []string{"space"}}
	season := core.Title{Key: core.SeasonKey(904, 1), Type: core.TypeSeason, ShowKey: show.Key, Season: 1, Released: show.Released}
	for _, item := range []core.Title{show, season} {
		if err := c.UpsertTitle(ctx, item, true); err != nil {
			t.Fatal(err)
		}
	}
	for i, date := range []time.Time{now.Add(-48 * time.Hour), now.Add(48 * time.Hour), {}, now.Add(72 * time.Hour)} {
		ep := core.Title{Key: core.EpisodeKey(904, 1, i+1), Type: core.TypeEpisode, ShowKey: show.Key, Season: 1, Episode: i + 1, Released: date, Adult: i == 3}
		if err := c.UpsertTitle(ctx, ep, false); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.ScheduledEpisodes(ctx, season.Key); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("default schedule: %v", err)
	}
	c.UpdateConfig(config.Catalog{ShowUpcomingEpisodes: true})
	got, err := c.ScheduledEpisodes(ctx, season.Key)
	if err != nil || len(got) != 1 || got[0].Episode != 2 || got[0].ContentRating != "TV-14" || len(got[0].Keywords) != 1 {
		t.Fatalf("schedule: %+v %v", got, err)
	}
	for _, key := range []core.RatingKey{show.Key, core.SeasonKey(904, 0), core.SeasonKey(904, 2)} {
		if _, err := c.ScheduledEpisodes(ctx, key); !errors.Is(err, core.ErrNotFound) {
			t.Fatalf("invalid parent %s: %v", key, err)
		}
	}
	if _, err := c.Get(ctx, got[0].Key); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("future Get: %v", err)
	}
	for name, read := range map[string]func() ([]core.Title, error){
		"children":      func() ([]core.Title, error) { return c.Children(ctx, season.Key) },
		"grandchildren": func() ([]core.Title, error) { return c.Grandchildren(ctx, show.Key) },
	} {
		items, err := read()
		if err != nil || len(items) != 1 || items[0].Key != core.EpisodeKey(904, 1, 1) {
			t.Fatalf("%s: %+v %v", name, items, err)
		}
	}
	if _, err := c.NextEpisode(ctx, core.EpisodeKey(904, 1, 1)); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("autoplay admitted future: %v", err)
	}
	// A future season is not made visible by the option, even if it has dated episodes.
	futureSeason := core.Title{Key: core.SeasonKey(904, 2), Type: core.TypeSeason, ShowKey: show.Key, Season: 2, Released: now.Add(24 * time.Hour)}
	if err := c.UpsertTitle(ctx, futureSeason, true); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ScheduledEpisodes(ctx, futureSeason.Key); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("future season: %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := c.ScheduledEpisodes(cancelled, season.Key); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	// Airing changes ordinary catalogue access, without changing the episode ID.
	now = now.Add(49 * time.Hour)
	if items, err := c.ScheduledEpisodes(ctx, season.Key); err != nil || len(items) != 0 {
		t.Fatalf("aired placeholder remains: %+v %v", items, err)
	}
	if aired, err := c.Get(ctx, got[0].Key); err != nil || aired.Key != got[0].Key {
		t.Fatalf("aired episode: %+v %v", aired, err)
	}
	if items, err := c.Children(ctx, season.Key); err != nil || len(items) != 2 {
		t.Fatalf("aired children: %+v %v", items, err)
	}

	c.UpdateConfig(config.Catalog{})
	if _, err := c.ScheduledEpisodes(ctx, season.Key); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("disabled: %v", err)
	}
}
