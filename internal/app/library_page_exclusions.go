package app

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

type hiddenKeyCache struct {
	at   time.Time
	gen  uint64
	keys []core.RatingKey
}

// newLibraryPageExclusions caches discovery exclusions for a few seconds and
// reads blocked genres and keywords from the viewer's policy. The indexed
// library page pushes the result into SQL instead of scanning the catalogue.
func newLibraryPageExclusions(a *App) func(context.Context, *core.User) ([]core.RatingKey, []string, []string, string) {
	var mu sync.Mutex
	var cached hiddenKeyCache
	return func(ctx context.Context, viewer *core.User) ([]core.RatingKey, []string, []string, string) {
		var genres, keywords []string
		var rating string
		if a != nil && a.safety != nil {
			genres, keywords, rating = a.safety.BrowseBlocks(viewer)
		}
		return discoveryExclusions(ctx, a, &mu, &cached), genres, keywords, rating
	}
}

func discoveryExclusions(ctx context.Context, a *App, mu *sync.Mutex, cached *hiddenKeyCache) []core.RatingKey {
	if a == nil || a.availability == nil || a.catalog == nil {
		return nil
	}
	gen := a.catalog.WriteGeneration()
	mu.Lock()
	if cached.gen == gen && !cached.at.IsZero() && time.Since(cached.at) < 15*time.Second {
		keys := append([]core.RatingKey(nil), cached.keys...)
		mu.Unlock()
		return keys
	}
	mu.Unlock()
	keys := append(a.availability.HiddenMovies(ctx), a.availability.HiddenShows(ctx)...)
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	mu.Lock()
	cached.at = time.Now()
	cached.gen = gen
	cached.keys = append([]core.RatingKey(nil), keys...)
	mu.Unlock()
	return keys
}
