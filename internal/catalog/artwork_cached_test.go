package catalog_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestCachedGetDoesNotRefreshAndKeepsVisibilityGates(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); http.Error(w, "offline", 503) }))
	defer srv.Close()
	catalog := newTestCatalog(t, srv)
	ctx := context.Background()
	visible := core.Title{Key: core.MovieKey(41), Type: core.TypeMovie, IDs: core.IDs{TMDB: 41}, Title: "Visible", Released: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)}
	adult := visible
	adult.Key = core.MovieKey(42)
	adult.IDs.TMDB = 42
	adult.Adult = true
	future := visible
	future.Key = core.MovieKey(43)
	future.IDs.TMDB = 43
	future.Released = time.Now().Add(24 * time.Hour)
	for _, title := range []core.Title{visible, adult, future} {
		if err := catalog.UpsertTitle(ctx, title, true); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := catalog.CachedGet(ctx, visible.Key); err != nil || got.Title != visible.Title {
		t.Fatalf("visible title: %+v %v", got, err)
	}
	for _, key := range []core.RatingKey{adult.Key, future.Key, core.MovieKey(99)} {
		if _, err := catalog.CachedGet(ctx, key); !errors.Is(err, core.ErrNotFound) {
			t.Fatalf("hidden %s: %v", key, err)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("cache-only read made a network request")
	}
}
