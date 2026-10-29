package newznab

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
)

func TestProwlarrDiscoversUsenetAndTorrentAndUsesBudgetedProxy(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch r.URL.Path {
		case "/base/api/v1/indexer":
			if r.Header.Get("X-Api-Key") != "test-key" {
				t.Error("missing auth")
			}
			fmt.Fprint(w, `[{"id":4,"name":"News","enable":true,"protocol":"usenet","priority":2},{"id":5,"name":"Torrent","enable":true,"protocol":"torrent"},{"id":6,"name":"Disabled","enable":false,"protocol":"usenet"}]`)
		case "/base/4/api":
			if r.URL.Query().Get("apikey") != "test-key" {
				t.Error("missing proxied auth")
			}
			fmt.Fprint(w, `<caps><searching><search available="yes" supportedParams="q"/></searching></caps>`)
		default:
			t.Errorf("bad proxy URL: %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	cfg := config.Prowlarr{URL: srv.URL + "/base", APIKey: "test-key", DailyLimit: 1, DailyGrabs: 2, Priority: 5}
	items, err := DiscoverProwlarr(context.Background(), cfg, Options{})
	if err != nil || len(items) != 2 {
		t.Fatalf("discover %+v %v", items, err)
	}
	if items[0].IsTorrent() || !items[1].IsTorrent() || !strings.Contains(items[1].Name, "Torrent") {
		t.Fatalf("protocols %+v", items)
	}
	item := items[0]
	if item.DailyLimit != 1 || item.DailyGrabs != 2 || item.Priority != 7 || !strings.HasPrefix(item.ID, "prowlarr-") {
		t.Fatalf("budget/identity %+v", item)
	}
	c := New(item, nil, Options{})
	if _, err := c.Caps(context.Background()); err != nil {
		t.Fatal(err)
	}
	remaining, err := c.Remaining(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("calls %d remaining %+v", calls, remaining)
	}
	if !c.Exhausted(context.Background()) {
		t.Fatal("discovered indexer bypassed daily budget")
	}
}

func TestProwlarrDiscoveryReportsSkippedUnknownProtocol(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[{"id":4,"name":"News","enable":true,"protocol":"usenet"},{"id":5,"name":"Odd","enable":true,"protocol":"rss"},{"id":5,"name":"Odd again","enable":true,"protocol":"rss"},{"id":6,"name":"Off","enable":false,"protocol":"rss"},{"id":7,"name":"Torrent","enable":true,"protocol":"torrent"}]`)
	}))
	defer srv.Close()
	report, err := DiscoverProwlarrReport(context.Background(), config.Prowlarr{URL: srv.URL, APIKey: "k"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Indexers) != 2 {
		t.Fatalf("indexers %+v", report.Indexers)
	}
	// Only the enabled unknown-protocol indexer counts — once, despite the
	// duplicated source entry; the disabled one does not count.
	if report.SkippedUnknownProtocol != 1 {
		t.Fatalf("skipped=%d want 1 (%+v)", report.SkippedUnknownProtocol, report)
	}
	// PresentIDs covers every enabled indexer (incl. the filtered id 5,
	// once), but not the disabled id 6: it records what this discovery
	// reported. It is diagnostic only: import is additive, so nothing
	// may use this list to remove stored indexers.
	prefix := ProwlarrIDPrefix(config.Prowlarr{URL: srv.URL})
	if prefix == "" {
		t.Fatal("empty prefix for test server URL")
	}
	if want := []string{prefix + "4", prefix + "5", prefix + "7"}; !slices.Equal(report.PresentIDs, want) {
		t.Fatalf("present=%v want %v", report.PresentIDs, want)
	}
	if len(report.Warnings) != 1 || !strings.Contains(report.Warnings[0], "unsupported protocol") || !strings.Contains(report.Warnings[0], "1") {
		t.Fatalf("warnings %+v", report.Warnings)
	}
	// The plain wrapper keeps working and drops nothing extra.
	items, err := DiscoverProwlarr(context.Background(), config.Prowlarr{URL: srv.URL, APIKey: "k"}, Options{})
	if err != nil || len(items) != 2 {
		t.Fatalf("wrapper %+v %v", items, err)
	}
}

func TestProwlarrDiscovery429PropagatesRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	_, err := DiscoverProwlarrReport(context.Background(), config.Prowlarr{URL: srv.URL, APIKey: "k"}, Options{})
	if err == nil {
		t.Fatal("expected error")
	}
	var rl *RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("err %T %v, want *RateLimitError", err, err)
	}
	if rl.RetryAfter != "120" {
		t.Fatalf("RetryAfter=%q want 120", rl.RetryAfter)
	}
	if !strings.Contains(err.Error(), "429") {
		t.Fatalf("err %v should name 429", err)
	}
	// Non-429 failures must not masquerade as rate limits.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer srv2.Close()
	_, err = DiscoverProwlarrReport(context.Background(), config.Prowlarr{URL: srv2.URL, APIKey: "k"}, Options{})
	if err == nil || errors.As(err, &rl) {
		t.Fatalf("500 err=%v should not be RateLimitError", err)
	}
}

// A malformed, past or absent Retry-After must not be propagated verbatim;
// a huge one is capped (24h, the Newznab client's bound).
func TestProwlarrDiscovery429ValidatesRetryAfter(t *testing.T) {
	past := time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)
	for _, tc := range []struct{ name, header, want string }{
		{"malformed", "soon", ""},
		{"past date", past, ""},
		{"huge capped", "999999999", "86400"},
		{"seconds kept", "45", "45"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", tc.header)
				w.WriteHeader(http.StatusTooManyRequests)
			}))
			defer srv.Close()
			_, err := DiscoverProwlarrReport(context.Background(), config.Prowlarr{URL: srv.URL, APIKey: "k"}, Options{})
			var rl *RateLimitError
			if !errors.As(err, &rl) {
				t.Fatalf("err %v, want *RateLimitError", err)
			}
			if rl.RetryAfter != tc.want {
				t.Fatalf("RetryAfter=%q want %q (from %q)", rl.RetryAfter, tc.want, tc.header)
			}
		})
	}
}

// hostRewriteTransport sends every request to target regardless of the
// host/port in the URL, so discovery can be exercised against URLs that
// name a default port or a differently-cased host without binding :80.
type hostRewriteTransport struct{ target string }

func (rt hostRewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.URL.Host = rt.target
	return http.DefaultTransport.RoundTrip(r)
}

// Tie test: ProwlarrIDPrefix(cfg) must be a prefix of every ID Discover
// generates for the same configured spelling (including host case and
// explicit default ports). Existing spellings retain their legacy scope.
func TestProwlarrPrefixIsPrefixOfDiscoveredIDsAcrossURLSpellings(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[{"id":4,"name":"News","enable":true,"protocol":"usenet"}]`)
	}))
	defer srv.Close()
	target := strings.TrimPrefix(srv.URL, "http://") // 127.0.0.1:<port>
	port := target[strings.LastIndex(target, ":")+1:]
	client := &http.Client{Transport: hostRewriteTransport{target: target}}
	for _, raw := range []string{
		srv.URL,
		srv.URL + "/",
		"http://LOCALHOST:" + port + "/base/", // host case + trailing slash (+path)
		"http://LOCALHOST/base",               // default port (80) implied, via rewrite
		"http://localhost:80/base/",           // default port (80) explicit
	} {
		cfg := config.Prowlarr{URL: raw, APIKey: "k"}
		prefix := ProwlarrIDPrefix(cfg)
		if prefix == "" {
			t.Fatalf("%q: empty prefix", raw)
		}
		report, err := DiscoverProwlarrReport(context.Background(), cfg, Options{HTTPClient: client})
		if err != nil {
			t.Fatalf("%q: %v", raw, err)
		}
		if len(report.Indexers) != 1 || !strings.HasPrefix(report.Indexers[0].ID, prefix) {
			t.Fatalf("%q: id %v not under prefix %q", raw, report.Indexers, prefix)
		}
		if len(report.PresentIDs) != 1 || !strings.HasPrefix(report.PresentIDs[0], prefix) {
			t.Fatalf("%q: present %v not under prefix %q", raw, report.PresentIDs, prefix)
		}
	}
}

// Golden vector (C3): the expected prefix is hard-coded on purpose.
// The scope is sum[:6] in Go — 6 bytes, i.e. 12 hex chars when
// hex-encoded (python3 hashlib hexdigest()[:12]) — computed
// independently as 1cf1d5715817; any change to the
// canonicalisation or scope derivation breaks this test deliberately,
// because changing scope orphans every previously imported indexer.
func TestProwlarrIDPrefixGoldenVector(t *testing.T) {
	const want = "prowlarr-1cf1d5715817-"
	for _, raw := range []string{
		"http://example.com:9696/base",
	} {
		if got := ProwlarrIDPrefix(config.Prowlarr{URL: raw}); got != want {
			t.Fatalf("prefix(%q)=%q want golden %q", raw, got, want)
		}
	}
}

// Local-edit detection vs the generated entry: only API-key rotation
// (copied from the current settings on every import) is ignored.
func TestProwlarrDiffersFromGenerated(t *testing.T) {
	generated := config.Indexer{ID: "prowlarr-x-4", Name: "Prowlarr / News [x:4]", URL: "http://h/4", APIKey: "k1", DailyLimit: 1, DailyGrabs: 2, Priority: 3, Enabled: true, MovieCategories: []int{2000}}
	if ProwlarrDiffersFromGenerated(generated, generated) {
		t.Fatal("identical entry reported as edited")
	}
	rotated := generated
	rotated.APIKey = "k2"
	if ProwlarrDiffersFromGenerated(rotated, generated) {
		t.Fatal("API-key rotation alone must not mark an entry as edited")
	}
	for _, tc := range []struct {
		name   string
		mutate func(*config.Indexer)
	}{
		{"name", func(x *config.Indexer) { x.Name = "Mine" }},
		{"url", func(x *config.Indexer) { x.URL = "http://elsewhere/4" }},
		{"dailyLimit", func(x *config.Indexer) { x.DailyLimit = 99 }},
		{"dailyGrabs", func(x *config.Indexer) { x.DailyGrabs = 99 }},
		{"priority", func(x *config.Indexer) { x.Priority = 99 }},
		{"enabled", func(x *config.Indexer) { x.Enabled = false }},
		{"protocol", func(x *config.Indexer) { x.Protocol = "torrent" }},
		{"categories", func(x *config.Indexer) { x.MovieCategories = []int{2000, 2010} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stored := generated
			// Deep-copy slices so mutations don't alias generated.
			stored.MovieCategories = slices.Clone(generated.MovieCategories)
			stored.TVCategories = slices.Clone(generated.TVCategories)
			tc.mutate(&stored)
			if !ProwlarrDiffersFromGenerated(stored, generated) {
				t.Fatalf("edit to %s not detected", tc.name)
			}
		})
	}
}

// A valid future HTTP-date Retry-After is accepted and converted to a
// seconds value inside the remaining window (not dropped as past).
func TestProwlarrDiscovery429FutureHTTPDateRetryAfter(t *testing.T) {
	future := time.Now().Add(2 * time.Hour).UTC().Format(http.TimeFormat)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", future)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	_, err := DiscoverProwlarrReport(context.Background(), config.Prowlarr{URL: srv.URL, APIKey: "k"}, Options{})
	var rl *RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("err %v, want *RateLimitError", err)
	}
	secs, cerr := strconv.Atoi(rl.RetryAfter)
	if cerr != nil || secs <= 7000 || secs > 7200 {
		t.Fatalf("RetryAfter=%q want seconds in (7000,7200] for %q", rl.RetryAfter, future)
	}
}

// ProwlarrIDPrefix validates like discovery: unusable URLs yield "".
func TestProwlarrIDPrefixValidatesLikeDiscovery(t *testing.T) {
	for _, raw := range []string{
		"", "not a url", "ftp://example.com", "http://", "http://user@example.com",
		"http://example.com/?x=1", "http://example.com/#frag", "//example.com",
	} {
		if got := ProwlarrIDPrefix(config.Prowlarr{URL: raw}); got != "" {
			t.Errorf("prefix(%q)=%q want empty", raw, got)
		}
		if _, err := DiscoverProwlarrReport(context.Background(), config.Prowlarr{URL: raw, APIKey: "k"}, Options{}); err == nil {
			t.Errorf("discover(%q) unexpectedly succeeded", raw)
		}
	}
}
