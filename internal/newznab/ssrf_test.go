package newznab

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/safefetch"
)

// A feed-supplied NZB URL pointing at a non-indexer private host (cloud
// metadata shape) must be refused before the internal service is hit.
func TestFetchNZBBlocksFeedURLToPrivateHost(t *testing.T) {
	var hits atomic.Int32
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, fakeNZB)
	}))
	defer internal.Close()

	c := New(testConfig("https://indexer.example"), nil, Options{})
	rel := core.Release{ID: "ssrf1", GUID: "g1", Name: "X", NZBURL: internal.URL + "/nzb?id=g1"}
	if _, err := c.FetchNZB(context.Background(), rel); !errors.Is(err, safefetch.ErrBlocked) {
		t.Fatalf("FetchNZB(private NZBURL) err = %v, want ErrBlocked", err)
	}
	if _, err := c.FetchNZB(context.Background(), core.Release{ID: "ssrf2", Name: "X", NZBURL: "http://169.254.169.254/latest/meta-data"}); !errors.Is(err, safefetch.ErrBlocked) {
		t.Fatalf("FetchNZB(metadata NZBURL) err = %v, want ErrBlocked", err)
	}
	// A network-path relative URL also leaves the indexer origin.
	if _, err := c.FetchNZB(context.Background(), core.Release{ID: "ssrf3", Name: "X", NZBURL: "//169.254.169.254/latest/meta-data"}); !errors.Is(err, safefetch.ErrBlocked) {
		t.Fatalf("FetchNZB(network-path NZBURL) err = %v, want ErrBlocked", err)
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("internal server hit %d times, want 0", got)
	}
}

// Regression (owner review): a proxy configured on the owner's HTTP
// client must not carry an untrusted feed URL to the metadata address
// — guarded fetches ignore proxies and dial (and validate) directly.
func TestFetchNZBWithConfiguredProxyDoesNotReachProxy(t *testing.T) {
	var proxyHits atomic.Int32
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHits.Add(1)
		fmt.Fprint(w, fakeNZB)
	}))
	defer proxySrv.Close()
	proxyURL, _ := url.Parse(proxySrv.URL)

	c := New(testConfig("https://indexer.example"), nil, Options{
		HTTPClient: &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}},
	})
	rel := core.Release{ID: "ssrf-proxy", Name: "X", NZBURL: "http://169.254.169.254/latest/meta-data"}
	if _, err := c.FetchNZB(context.Background(), rel); !errors.Is(err, safefetch.ErrBlocked) {
		t.Fatalf("FetchNZB(metadata, proxy configured) err = %v, want ErrBlocked", err)
	}
	if got := proxyHits.Load(); got != 0 {
		t.Fatalf("proxy hit %d times, want 0", got)
	}
}

// Regression (owner review): a connection opened to the private
// indexer while it was the owner-configured origin must not be reused
// by a later untrusted fetch to the same host:port. Once the config
// points elsewhere, that host is just another feed-supplied private
// destination and is blocked; with a shared pool the second fetch
// would reuse the open connection and skip the dial check.
func TestFetchNZBUntrustedSameHostAfterTrustedFetchBlocked(t *testing.T) {
	var hits atomic.Int32
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, fakeNZB)
	}))
	defer internal.Close()

	c := New(testConfig(internal.URL), nil, Options{})
	rel := core.Release{ID: "ssrf-pool1", GUID: "g1", Name: "X", NZBURL: internal.URL + "/nzb?id=g1"}
	if _, err := c.FetchNZB(context.Background(), rel); err != nil {
		t.Fatalf("trusted indexer FetchNZB: %v", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("hits = %d, want 1 after trusted fetch", got)
	}

	c.UpdateConfig(testConfig("https://indexer.example"))
	rel = core.Release{ID: "ssrf-pool2", GUID: "g2", Name: "X", NZBURL: internal.URL + "/nzb?id=g2"}
	if _, err := c.FetchNZB(context.Background(), rel); !errors.Is(err, safefetch.ErrBlocked) {
		t.Fatalf("untrusted FetchNZB to previously trusted host err = %v, want ErrBlocked", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("internal server hit %d times, want 1 (trusted fetch only)", got)
	}
}

// The indexer itself is owner-configured (private allowed), but a
// redirect off it to another private host must be refused.
func TestFetchNZBRedirectToPrivateHostBlocked(t *testing.T) {
	var hits atomic.Int32
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, fakeNZB)
	}))
	defer internal.Close()
	indexer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, internal.URL+"/nzb", http.StatusFound)
	}))
	defer indexer.Close()

	c := New(testConfig(indexer.URL), nil, Options{})
	rel := core.Release{ID: "ssrf4", GUID: "g1", Name: "X", NZBURL: indexer.URL + "/nzb?id=g1"}
	if _, err := c.FetchNZB(context.Background(), rel); !errors.Is(err, safefetch.ErrBlocked) {
		t.Fatalf("FetchNZB(redirect to private) err = %v, want ErrBlocked", err)
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("internal server hit %d times, want 0", got)
	}
}

// Same guard on the .torrent download path.
func TestResolveTorrentRedirectToPrivateHostBlocked(t *testing.T) {
	var hits atomic.Int32
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer internal.Close()
	indexer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, internal.URL+"/file.torrent", http.StatusFound)
	}))
	defer indexer.Close()

	cfg := testConfig(indexer.URL)
	cfg.Protocol = "torrent"
	c := New(cfg, nil, Options{})
	rel := core.Release{Name: "X", Protocol: core.ProtocolTorrent, NZBURL: indexer.URL + "/dl/file.torrent"}
	if _, err := c.ResolveTorrent(context.Background(), rel); !errors.Is(err, safefetch.ErrBlocked) {
		t.Fatalf("ResolveTorrent(redirect to private) err = %v, want ErrBlocked", err)
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("internal server hit %d times, want 0", got)
	}
}

// Search (caps + streamed feed) also revalidates redirects off the
// owner-configured indexer.
func TestSearchRedirectToPrivateHostBlocked(t *testing.T) {
	var hits atomic.Int32
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, `<rss><channel></channel></rss>`)
	}))
	defer internal.Close()
	indexer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, internal.URL+"/api", http.StatusFound)
	}))
	defer indexer.Close()

	c := New(testConfig(indexer.URL), nil, Options{SearchCacheTTL: -1})
	if _, err := c.Search(context.Background(), core.SearchRequest{Type: core.TypeMovie, Title: "The Signal", Year: 1999}); !errors.Is(err, safefetch.ErrBlocked) {
		t.Fatalf("Search(redirect to private) err = %v, want ErrBlocked", err)
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("internal server hit %d times, want 0", got)
	}
}
