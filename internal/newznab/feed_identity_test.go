package newznab

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/braedonsaunders/ether/internal/config"
)

type feedCapture struct {
	mu   sync.Mutex
	reqs []capturedRequest
}

func (c *feedCapture) add(r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reqs = append(c.reqs, capturedRequest{path: r.URL.Path, query: r.URL.Query(), ua: r.Header.Get("User-Agent"), header: r.Header.Clone()})
}

func (c *feedCapture) all() []capturedRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]capturedRequest(nil), c.reqs...)
}

func newFeedServer(t *testing.T) (*feedCapture, *httptest.Server) {
	t.Helper()
	capture := &feedCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capture.add(r)
		fmt.Fprint(w, `<rss><channel></channel></rss>`)
	}))
	t.Cleanup(srv.Close)
	return capture, srv
}

func feedIndexer(url string) config.Indexer {
	return config.Indexer{ID: "ix", Name: "Fake", URL: url, APIKey: "k", Enabled: true}
}

func TestFeedSourcesStayMixedForEther(t *testing.T) {
	m := NewManager(nil, []config.Indexer{{ID: "ix", Name: "Fake", URL: "https://ix.example", Enabled: true}}, Options{})
	feeds := m.FeedSources()
	if len(feeds) != 1 || feeds[0].ID != "ix" {
		t.Fatalf("feeds = %+v", feeds)
	}
	if len(feeds[0].Categories) != 2 || feeds[0].Categories[0] != 2000 || feeds[0].Categories[1] != 5000 {
		t.Fatalf("mixed categories = %v", feeds[0].Categories)
	}
}

func TestFeedSourcesSplitForPersonas(t *testing.T) {
	policy := servarrPolicy()
	m := NewManager(nil, []config.Indexer{{ID: "ix", Name: "Fake", URL: "https://ix.example", Enabled: true}}, Options{Identity: policy})
	feeds := m.FeedSources()
	if len(feeds) != 2 {
		t.Fatalf("feeds = %+v", feeds)
	}
	byID := map[string]bool{}
	for _, f := range feeds {
		byID[f.ID] = true
	}
	if !byID["ix#tv"] || !byID["ix#movie"] {
		t.Fatalf("feed ids = %v", byID)
	}
	if feeds[0].Categories[0] != 5000 || feeds[1].Categories[0] != 2000 {
		t.Fatalf("side categories = %v / %v", feeds[0].Categories, feeds[1].Categories)
	}
	if feeds[0].Generation == feeds[1].Generation {
		t.Fatal("sides share a generation, so one cursor would reset the other")
	}
}

func TestFeedSourcesSplitForIndexerOverride(t *testing.T) {
	m := NewManager(nil, []config.Indexer{{
		ID: "ix", Name: "Fake", URL: "https://ix.example", Enabled: true, SearchIdentity: IdentityProwlarr,
	}}, Options{})
	feeds := m.FeedSources()
	if len(feeds) != 2 || feeds[0].ID != "ix#tv" || feeds[1].ID != "ix#movie" {
		t.Fatalf("override did not split the feed: %+v", feeds)
	}
}

func TestRecentUsesSideIdentityVerbAndOffset(t *testing.T) {
	capture, srv := newFeedServer(t)
	m := NewManager(nil, []config.Indexer{feedIndexer(srv.URL)}, Options{Identity: servarrPolicy(), Version: "1.0", SearchCacheTTL: -1})
	ctx := context.Background()

	if _, err := m.Recent(ctx, "ix#tv", 100, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Recent(ctx, "ix#movie", 200, 100); err != nil {
		t.Fatal(err)
	}
	reqs := capture.all()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d", len(reqs))
	}
	tv, movie := reqs[0], reqs[1]
	if !strings.HasPrefix(tv.ua, "Sonarr/") || tv.query.Get("t") != "tvsearch" || tv.query.Get("cat") != "5000" || tv.query.Get("offset") != "100" {
		t.Errorf("TV feed: UA %q query %v", tv.ua, tv.query)
	}
	if !strings.HasPrefix(movie.ua, "Radarr/") || movie.query.Get("t") != "movie" || movie.query.Get("cat") != "2000" || movie.query.Get("offset") != "200" {
		t.Errorf("movie feed: UA %q query %v", movie.ua, movie.query)
	}
	if tv.query.Get("limit") != "100" || tv.query.Get("extended") != "1" {
		t.Errorf("TV feed shape: %v", tv.query)
	}

	// A split indexer has no mixed feed left to resume.
	if _, err := m.Recent(ctx, "ix", 0, 100); err == nil {
		t.Fatal("mixed feed served for a split indexer")
	}
}

func TestRecentKeepsMixedFeedForEther(t *testing.T) {
	capture, srv := newFeedServer(t)
	m := NewManager(nil, []config.Indexer{feedIndexer(srv.URL)}, Options{SearchCacheTTL: -1})
	if _, err := m.Recent(context.Background(), "ix", 50, 100); err != nil {
		t.Fatal(err)
	}
	reqs := capture.all()
	if len(reqs) != 1 {
		t.Fatalf("requests = %d", len(reqs))
	}
	q := reqs[0].query
	if q.Get("t") != "search" || q.Get("cat") != "2000,5000" || q.Get("offset") != "50" || !strings.HasPrefix(reqs[0].ua, "Ether/") {
		t.Fatalf("mixed feed: UA %q query %v", reqs[0].ua, q)
	}
}
