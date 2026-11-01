package catalog_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/tmdb"
)

// Casablanca's primary release_date is 1942. An earlier limited theatrical
// date in another country is date added and must not become the title year
// Plex, Jellyfin and Silo match on. A later summary keeps both.
func TestPrimaryReleaseYearSurvivesTheatricalDateAdded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/movie/289" && r.URL.Path != "/movie/290" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/movie/290" {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 290, "title": "No primary", "release_dates": map[string]any{"results": []any{
				map[string]any{"iso_3166_1": "FR", "release_dates": []any{
					map[string]any{"type": 1, "release_date": "1951-06-01T00:00:00Z"},
					map[string]any{"type": 3, "release_date": "1952-01-04T00:00:00Z"},
				}},
			}}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 289, "title": "Casablanca", "release_date": "1942-11-26", "release_dates": map[string]any{"results": []any{
			map[string]any{"iso_3166_1": "US", "release_dates": []any{
				map[string]any{"type": 1, "release_date": "1942-11-26T00:00:00Z"},
				map[string]any{"type": 3, "release_date": "1943-01-23T00:00:00Z"},
			}},
			map[string]any{"iso_3166_1": "SE", "release_dates": []any{
				map[string]any{"type": 2, "release_date": "1943-01-15T00:00:00Z"},
			}},
		}}})
	}))
	t.Cleanup(srv.Close)
	ctx := context.Background()
	c := newTestCatalog(t, srv)
	got, err := c.Get(ctx, core.MovieKey(289))
	if err != nil {
		t.Fatal(err)
	}
	if got.Year != 1942 || got.Released.Format(time.DateOnly) != "1943-01-15" {
		t.Fatalf("detail: year %d released %s", got.Year, got.Released.Format(time.DateOnly))
	}
	summary := core.Title{Key: core.MovieKey(289), Type: core.TypeMovie, IDs: core.IDs{TMDB: 289}, Title: "Casablanca", Year: 1942, Released: tmdb.ParseDate("1942-11-26")}
	if err := c.UpsertTitle(ctx, summary, true); err != nil {
		t.Fatal(err)
	}
	got, err = c.CachedTitle(ctx, core.MovieKey(289))
	if err != nil {
		t.Fatal(err)
	}
	if got.Year != 1942 || got.Released.Format(time.DateOnly) != "1943-01-15" {
		t.Fatalf("after summary: year %d released %s", got.Year, got.Released.Format(time.DateOnly))
	}
	premiere, err := c.Get(ctx, core.MovieKey(290))
	if err != nil {
		t.Fatal(err)
	}
	if premiere.Year != 1951 || premiere.Released.Format(time.DateOnly) != "1952-01-04" {
		t.Fatalf("premiere year: year %d released %s", premiere.Year, premiere.Released.Format(time.DateOnly))
	}
}
