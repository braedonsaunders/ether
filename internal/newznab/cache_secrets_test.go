package newznab

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

const cacheTestKey = "cache-test-api-key"

func TestCachedResultsHoldNoAPIKeyAndStillDownload(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var searches, grabs atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("apikey") != cacheTestKey {
			fmt.Fprint(w, `<?xml version="1.0"?><error code="100" description="Incorrect user credentials"/>`)
			return
		}
		base := "http://" + r.Host
		switch q.Get("t") {
		case "caps":
			fmt.Fprint(w, loadFixture(t, "caps.xml", base))
		default:
			searches.Add(1)
			fmt.Fprintf(w, `<?xml version="1.0"?><rss version="2.0"><channel><item><title>The.Signal.1999.1080p.BluRay.x264-GRP</title>`+
				`<guid>%[1]s/details/abc?apikey=%[2]s</guid><link>%[1]s/getnzb?id=abc&amp;r=%[2]s</link>`+
				`<enclosure url="%[1]s/getnzb?id=abc&amp;r=%[2]s" length="8000000000" type="application/x-nzb"/></item></channel></rss>`, base, cacheTestKey)
		}
	})
	mux.HandleFunc("/getnzb", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("r") != cacheTestKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		grabs.Add(1)
		fmt.Fprint(w, fakeNZB)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	cfg := config.Indexer{ID: "idx1", Name: "Fake", URL: srv.URL, APIKey: cacheTestKey, Enabled: true}
	req := core.SearchRequest{Type: core.TypeMovie, Title: "The Signal", Year: 1999}
	m := NewManager(db, []config.Indexer{cfg}, Options{NZBDir: t.TempDir()})
	first, err := m.Search(ctx, req)
	if err != nil || len(first) == 0 || !strings.Contains(first[0].NZBURL, cacheTestKey) {
		t.Fatalf("search: %v %+v", err, first)
	}
	var rows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM newznab_cache`).Scan(&rows); err != nil || rows == 0 {
		t.Fatalf("nothing cached: %v", err)
	}
	var leaked int
	if err := db.QueryRow(`SELECT COUNT(*) FROM newznab_cache WHERE results LIKE ?`, "%"+cacheTestKey+"%").Scan(&leaked); err != nil || leaked != 0 {
		t.Fatalf("cached rows hold the API key: %d %v", leaked, err)
	}
	// A restarted server (empty NZB directory) answers from the cache and
	// the hydrated link authenticates the download.
	m = NewManager(db, []config.Indexer{cfg}, Options{NZBDir: t.TempDir()})
	cached, err := m.Search(ctx, req)
	if err != nil || len(cached) == 0 {
		t.Fatalf("cached search: %v", err)
	}
	if searches.Load() != 1 {
		t.Fatalf("cache missed: %d searches", searches.Load())
	}
	if cached[0].NZBURL != first[0].NZBURL || cached[0].GUID != first[0].GUID || cached[0].ID != first[0].ID {
		t.Fatalf("hydrated release differs:\n%+v\n%+v", cached[0], first[0])
	}
	if b, err := m.FetchNZB(ctx, cached[0]); err != nil || string(b) != fakeNZB || grabs.Load() != 1 {
		t.Fatalf("download after hydration: %v grabs=%d", err, grabs.Load())
	}
	// Feed bootstrap reads paid-for results locally without spending a hit/grab.
	beforeHits, beforeGrabs := searches.Load(), grabs.Load()
	observed, err := m.CachedReleases(ctx)
	if err != nil || len(observed) != 1 || observed[0].NZBURL != first[0].NZBURL || searches.Load() != beforeHits || grabs.Load() != beforeGrabs {
		t.Fatal("cached discovery issued a request or lost authenticated descriptor", err)
	}
	rotated := cfg
	rotated.URL = "https://changed.invalid"
	other := NewManager(db, []config.Indexer{rotated}, Options{})
	if rs, err := other.CachedReleases(ctx); err != nil || len(rs) != 0 {
		t.Fatal("bulk cache reused another destination", err)
	}
	// A changed key misses the cache instead of hydrating another key.
	cfg.APIKey = "rotated-key"
	c := New(cfg, db, Options{})
	if _, ok := c.cacheGet(ctx, cacheKey(c, req)); ok {
		t.Fatal("cache hit across a key change")
	}
}

func TestLegacyCacheRowsPurged(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO newznab_cache(cache_key, indexer_id, results, expires_at) VALUES('old','idx1',?,9999999999)`,
		`[{"id":"1","nzbUrl":"https://x/api?t=get&apikey=legacy-key"}]`); err != nil {
		t.Fatal(err)
	}
	c := New(config.Indexer{ID: "idx1", URL: "https://x", APIKey: "legacy-key"}, db, Options{})
	c.cachePut(ctx, "new", []core.Release{{ID: "2", NZBURL: "https://x/api?t=get&apikey=legacy-key"}})
	// Legacy rows still read (an older node may have written them).
	if got, ok := c.cacheGet(ctx, "old"); !ok || got[0].NZBURL != "https://x/api?t=get&apikey=legacy-key" {
		t.Fatal("legacy row unreadable")
	}
	m := NewManager(db, nil, Options{})
	if n, err := m.PurgeLegacyCache(ctx); err != nil || n != 1 {
		t.Fatalf("purge: %d %v", n, err)
	}
	if got, ok := c.cacheGet(ctx, "new"); !ok || got[0].NZBURL != "https://x/api?t=get&apikey=legacy-key" {
		t.Fatal("current row purged or not hydrated")
	}
	if n, _ := m.PurgeLegacyCache(ctx); n != 0 {
		t.Fatal("purge not idempotent")
	}
}

func TestRedactLinkRoundTrip(t *testing.T) {
	c := New(config.Indexer{APIKey: "k+y/1", SecretSettings: map[string]string{"passkey": "pk", "cookie": "k+y/1"}}, nil, Options{})
	secrets := c.cacheSecrets()
	for _, link := range []string{"", "https://x/a?apikey=k+y/1&b=k%2By%2F1&p=k%2By%2F1", "magnet:?xt=urn:btih:00&tr=https://t/announce/pk", "no secret here", "pkpkk+y/1"} {
		red, ok := redactLink(link, secrets)
		if !ok || strings.Contains(red, "k+y/1") || strings.Contains(red, "k%2By%2F1") {
			t.Fatalf("redact %q -> %q", link, red)
		}
		back, ok := hydrateLink(red, secrets)
		if !ok || back != link {
			t.Fatalf("round trip %q -> %q -> %q", link, red, back)
		}
	}
	if _, ok := redactLink("bad\x00link", secrets); ok {
		t.Fatal("NUL accepted")
	}
	if _, ok := hydrateLink("x\x00unknown|raw\x00y", secrets); ok {
		t.Fatal("unknown token hydrated")
	}
}

func TestCachedAddonStreamLinksDropAddonSecrets(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const key = "rdkey-0123456789ABCDEF"
	cfg := config.Indexer{ID: "addon1", Kind: config.IndexerKindStremio, URL: "https://addon.example/realdebrid=" + key + "/manifest.json", Enabled: true}
	c := New(cfg, db, Options{})
	rel := core.Release{ID: "h", Protocol: core.ProtocolHTTP,
		StreamURL:     "https://addon.example/resolve/realdebrid/" + key + "/abc/0/Movie.mkv",
		StreamHeaders: map[string]string{"Authorization": "Bearer " + key},
		NZBURL:        "https://addon.example/nzb/" + key + "/x.nzb"}
	c.cachePut(ctx, "k", []core.Release{rel})
	var stored string
	if err := db.QueryRow(`SELECT results FROM newznab_cache WHERE cache_key='k'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, key) {
		t.Fatalf("cached row keeps the addon's key: %s", stored)
	}
	got, ok := c.cacheGet(ctx, "k")
	if !ok || got[0].StreamURL != rel.StreamURL || got[0].StreamHeaders["Authorization"] != "Bearer "+key || got[0].NZBURL != rel.NZBURL {
		t.Fatalf("hydrated %+v", got)
	}
	// Route words in the path are not secrets.
	for _, s := range c.cacheSecrets() {
		if s.text == "manifest.json" || s.text == "resolve" {
			t.Fatalf("route word %q treated as a secret", s.text)
		}
	}
}
