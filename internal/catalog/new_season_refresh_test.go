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

type seasonDiscoverySource struct {
	catalog.TMDB
	seasons int
	calls   int
}

func (f *seasonDiscoverySource) TVDetails(ctx context.Context, id int64) (*tmdb.TVDetails, error) {
	f.calls++
	d := &tmdb.TVDetails{TVShow: tmdb.TVShow{ID: id, Name: "Returning Show", FirstAirDate: "2025-01-01"}, Status: "Returning Series"}
	for n := 1; n <= f.seasons; n++ {
		d.Seasons = append(d.Seasons, tmdb.SeasonSummary{ID: int64(n), Name: fmt.Sprintf("Season %d", n), SeasonNumber: n, EpisodeCount: 1, AirDate: "2026-10-30"})
	}
	return d, nil
}
func (f *seasonDiscoverySource) TVDetailsFresh(ctx context.Context, id int64) (*tmdb.TVDetails, error) {
	return f.TVDetails(ctx, id)
}

func TestReturningShowDiscoversNewSeason(t *testing.T) {
	for _, mode := range []string{"automatic", "explicit"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
			f := &seasonDiscoverySource{seasons: 1}
			c, err := catalog.New(store.OpenTest(t), f, config.Catalog{RefreshInterval: config.Duration(24 * time.Hour)}, slog.New(slog.DiscardHandler), catalog.WithNow(func() time.Time { return now }))
			if err != nil {
				t.Fatal(err)
			}
			key := core.ShowKey(800)
			initial, err := c.Children(ctx, key)
			if err != nil || len(initial) != 1 {
				t.Fatalf("initial: %v %v", initial, err)
			}
			f.seasons = 2
			if mode == "explicit" {
				if _, err := c.RefreshTitle(ctx, key, false); err != nil {
					t.Fatal(err)
				}
				preview, err := c.Children(ctx, key)
				if err != nil || len(preview) != 1 {
					t.Fatalf("preview persisted seasons: %v %v", preview, err)
				}
				if _, err := c.RefreshTitle(ctx, key, true); err != nil {
					t.Fatal(err)
				}
			} else {
				now = now.Add(59 * time.Minute)
				if _, err := c.Children(ctx, key); err != nil {
					t.Fatal(err)
				}
				if f.calls != 1 {
					t.Fatalf("fresh reads fetched %d times", f.calls)
				}
				now = now.Add(time.Minute)
			}
			got, err := c.Children(ctx, key)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 2 {
				t.Fatalf("new season missing: got %d seasons, want 2", len(got))
			}
			calls := f.calls
			for i := 0; i < 5; i++ {
				if _, err := c.Children(ctx, key); err != nil {
					t.Fatal(err)
				}
			}
			if f.calls != calls {
				t.Fatalf("repeated reads refetched: %d -> %d", calls, f.calls)
			}
		})
	}
}
