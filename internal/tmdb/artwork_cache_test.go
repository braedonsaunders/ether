package tmdb

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestCachedArtworkVariantsNeverFetchesAndRejectsStale(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`{"posters":[{"file_path":"/poster.jpg"}]}`))
	}))
	defer srv.Close()
	c := New("key", nil, WithBaseURL(srv.URL), WithCacheTTL(time.Hour))
	defer c.Close()
	title := &core.Title{Key: core.MovieKey(42), Type: core.TypeMovie, IDs: core.IDs{TMDB: 42}}
	if _, ok := c.CachedArtworkVariants(title, "fr-CA"); ok || requests.Load() != 0 {
		t.Fatal("cache miss made a request")
	}
	if _, err := c.ArtworkVariants(context.Background(), title, "fr-CA"); err != nil {
		t.Fatal(err)
	}
	got, ok := c.CachedArtworkVariants(title, "fr-CA")
	if !ok || len(got.Posters) != 1 || got.Posters[0].FilePath != "/poster.jpg" || requests.Load() != 1 {
		t.Fatalf("cache hit: %+v %v, requests %d", got, ok, requests.Load())
	}
	if _, ok := c.CachedArtworkVariants(title, "en-US"); ok {
		t.Fatal("different language shared an entry")
	}
	p, q, err := artworkVariantsRequest(title, "fr-CA")
	if err != nil {
		t.Fatal(err)
	}
	cfg := c.settings()
	key := strconv.FormatUint(cfg.generation, 10) + ":" + c.buildURL(p, q, cfg)
	c.mu.Lock()
	entry, found := c.cache.get(key)
	if !found {
		c.mu.Unlock()
		t.Fatal("cache entry missing")
	}
	entry.expires = time.Now().Add(-time.Second)
	c.cache.put(key, entry, time.Now())
	c.mu.Unlock()
	if _, ok := c.CachedArtworkVariants(title, "fr-CA"); ok || requests.Load() != 1 {
		t.Fatal("expired entry was served or refreshed")
	}
}
