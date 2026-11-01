package catalog_test

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/tmdb"
)

// downTMDB fails every metadata read and counts them. A library list must
// not repeat a failed show or season refresh once per episode.
type downTMDB struct {
	catalog.TMDB
	mu    sync.Mutex
	calls int
}

func (d *downTMDB) bump() {
	d.mu.Lock()
	d.calls++
	d.mu.Unlock()
}

func (d *downTMDB) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

func (d *downTMDB) TVDetails(context.Context, int64) (*tmdb.TVDetails, error) {
	d.bump()
	return nil, errors.New("down")
}

func (d *downTMDB) SeasonDetails(context.Context, int64, int) (*tmdb.SeasonDetails, error) {
	d.bump()
	return nil, errors.New("down")
}

func (d *downTMDB) EpisodeExternalIDs(context.Context, int64, int, int) (*tmdb.ExternalIDs, error) {
	d.bump()
	return nil, errors.New("down")
}

func (d *downTMDB) MovieDetails(context.Context, int64) (*tmdb.MovieDetails, error) {
	d.bump()
	return nil, errors.New("down")
}

func TestEpisodeListDoesNotRefreshEveryRowWhenMetadataIsDown(t *testing.T) {
	fake := &downTMDB{}
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	c, err := catalog.New(store.OpenTest(t), fake, config.Catalog{Language: "en-US", Region: "US", RefreshInterval: config.Duration(24 * time.Hour)}, slog.New(slog.DiscardHandler), catalog.WithNow(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	released := time.Date(2004, 1, 1, 0, 0, 0, 0, time.UTC)
	show := core.Title{Key: core.ShowKey(42), Type: core.TypeShow, Title: "The Scene", IDs: core.IDs{TMDB: 42}, Released: released, Year: 2004}
	if err = c.UpsertTitle(ctx, show, true); err != nil {
		t.Fatal(err)
	}
	season := core.Title{Key: core.SeasonKey(42, 1), Type: core.TypeSeason, Title: "Season 1", ShowKey: core.ShowKey(42), Season: 1, IDs: core.IDs{TMDB: 42}, Released: released, Year: 2004}
	if err = c.UpsertTitle(ctx, season, true); err != nil {
		t.Fatal(err)
	}
	var keys []core.RatingKey
	for n := 1; n <= 8; n++ {
		key := core.EpisodeKey(42, 1, n)
		keys = append(keys, key)
		ep := core.Title{Key: key, Type: core.TypeEpisode, Title: "Episode", ShowKey: core.ShowKey(42), ShowIDs: core.IDs{TMDB: 42}, Season: 1, Episode: n, Released: released, Year: 2004}
		if err = c.UpsertTitle(ctx, ep, true); err != nil {
			t.Fatal(err)
		}
	}
	for _, key := range keys {
		got, err := c.Get(ctx, key)
		if err != nil || got == nil || got.Key != key {
			t.Fatalf("get %s: %v %v", key, got, err)
		}
	}
	if calls := fake.count(); calls > 2 {
		t.Fatalf("metadata reads on first pass: %d, want at most one show and one season", calls)
	}
	for _, key := range keys {
		if _, err = c.Get(ctx, key); err != nil {
			t.Fatal(err)
		}
	}
	if calls := fake.count(); calls > 2 {
		t.Fatalf("metadata reads on repeat: %d, want the first pass only", calls)
	}
}
