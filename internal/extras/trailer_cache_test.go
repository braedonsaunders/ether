package extras

import (
	"context"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTrailerCacheBoundsAndEviction(t *testing.T) {
	ctx := context.Background()
	payload := []byte("0000ftypisom00000000000000000000")
	downloads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { downloads++; _, _ = w.Write(payload) }))
	defer server.Close()
	cfg := config.Extras{Enabled: true, TrailerCacheMaxBytes: 64}
	now := time.Now()
	opts := Options{CacheDir: t.TempDir(), Config: func() config.Extras { return cfg }, Now: func() time.Time { return now }, LookPath: func(s string) (string, error) { return s, nil }, Runner: func(context.Context, string, []string) ([]byte, error) { return []byte(server.URL), nil }}
	s := New(opts)
	extra := func(key string) Extra {
		return Extra{ParentKey: core.MovieKey(1), Source: SourceCommand, Site: "YouTube", SiteKey: key, Subtype: "trailer"}
	}
	a, err := s.CacheTrailer(ctx, extra("aaaaaaaaaaa"))
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	b, err := s.CacheTrailer(ctx, extra("bbbbbbbbbbb"))
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if _, err = s.CacheTrailer(ctx, extra("aaaaaaaaaaa")); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if _, err = s.CacheTrailer(ctx, extra("ccccccccccc")); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(a.Path); err != nil {
		t.Fatal("recent hit was evicted", err)
	}
	if _, err = os.Stat(b.Path); !os.IsNotExist(err) {
		t.Fatal("oldest file survived", err)
	}
	if downloads != 3 {
		t.Fatal("hit downloaded again", downloads)
	}
	// Restart retains bytes without invoking the resolver.
	restarted := New(opts)
	if _, err = restarted.CacheTrailer(ctx, extra("aaaaaaaaaaa")); err != nil || downloads != 3 {
		t.Fatal("cache did not survive restart", err, downloads)
	}
	cfg.TrailerCacheMaxBytes = 32
	if _, err = restarted.CacheTrailer(ctx, extra("aaaaaaaaaaa")); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(opts.CacheDir)
	var bytes int64
	for _, entry := range entries {
		info, _ := os.Stat(filepath.Join(opts.CacheDir, entry.Name()))
		bytes += info.Size()
	}
	if bytes > 32 {
		t.Fatal("lowered cap exceeded", bytes)
	}
	cfg.ResolverArgs = []string{"changed"}
	if f, err := s.OpenCachedTrailer(a); err == nil {
		f.Close()
		t.Fatal("stale source generation opened")
	}
	cfg.TrailerCacheMaxBytes = 16
	if _, err = s.CacheTrailer(ctx, extra("ddddddddddd")); err == nil {
		t.Fatal("oversize accepted")
	}
	if _, err = os.Stat(filepath.Join(opts.CacheDir, "download.part")); !os.IsNotExist(err) {
		t.Fatal("staging survived")
	}
	s.cacheDownloadMu.Lock()
	if _, err = s.CacheTrailer(ctx, extra("eeeeeeeeeee")); err != ErrCachePreparing {
		t.Fatal("concurrent fill admitted", err)
	}
	s.cacheDownloadMu.Unlock()
}
