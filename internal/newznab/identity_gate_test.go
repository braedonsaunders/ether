package newznab

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

type capturedRequest struct {
	path   string
	query  url.Values
	ua     string
	header http.Header
}

type requestCapture struct {
	mu   sync.Mutex
	reqs []capturedRequest
}

func (c *requestCapture) add(r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reqs = append(c.reqs, capturedRequest{path: r.URL.Path, query: r.URL.Query(), ua: r.Header.Get("User-Agent"), header: r.Header.Clone()})
}

func (c *requestCapture) all() []capturedRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]capturedRequest(nil), c.reqs...)
}

func (c *requestCapture) searchRequests() []capturedRequest {
	var out []capturedRequest
	for _, r := range c.all() {
		switch r.query.Get("t") {
		case "movie", "tvsearch", "search":
			out = append(out, r)
		}
	}
	return out
}

// newIdentityServer serves caps, one search item per verb, an NZB and a
// .torrent, capturing every request.
func newIdentityServer(t *testing.T) (*requestCapture, *httptest.Server) {
	t.Helper()
	capture := &requestCapture{}
	torrent, _ := sampleTorrent()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capture.add(r)
		q := r.URL.Query()
		switch {
		case r.URL.Path == "/nzb":
			fmt.Fprint(w, fakeNZB)
		case r.URL.Path == "/torrent":
			w.Write(torrent)
		case q.Get("t") == "caps":
			fmt.Fprint(w, `<caps><searching>`+
				`<search available="yes" supportedParams="q"/>`+
				`<movie-search available="yes" supportedParams="q,imdbid"/>`+
				`<tv-search available="yes" supportedParams="q,tvdbid"/>`+
				`</searching></caps>`)
		case q.Get("t") == "movie", q.Get("t") == "tvsearch", q.Get("t") == "search":
			category := 2040
			if q.Get("t") == "tvsearch" {
				category = 5040
			}
			link := "http://" + r.Host + "/nzb?id=item"
			fmt.Fprintf(w, `<rss><channel><item><title>Item.2024.1080p</title><guid>item</guid><link>%s</link>`+
				`<attr name="category" value="%d"/></item></channel></rss>`, link, category)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return capture, srv
}

func servarrPolicy() config.ClientIdentity {
	return config.ClientIdentity{
		SearchTV:     IdentitySonarr,
		SearchMovies: IdentityRadarr,
		GrabTV:       config.IdentitySame,
		GrabMovies:   config.IdentitySame,
	}
}

func TestRequestCarriesResolvedIdentity(t *testing.T) {
	capture, srv := newIdentityServer(t)
	cfg := config.Indexer{ID: "ix", Name: "Fake", URL: srv.URL, APIKey: "k", Enabled: true}
	c := New(cfg, nil, Options{Identity: servarrPolicy(), Version: "1.2.3", SearchCacheTTL: -1})
	ctx := context.Background()

	movie, err := c.Search(ctx, core.SearchRequest{Type: core.TypeMovie, IDs: core.IDs{IMDB: "tt1"}, Title: "Item", Year: 2024})
	if err != nil || len(movie) == 0 {
		t.Fatalf("movie search: %v (%d)", err, len(movie))
	}
	if _, err := c.FetchNZB(ctx, movie[0]); err != nil {
		t.Fatal(err)
	}
	tv, err := c.Search(ctx, core.SearchRequest{Type: core.TypeEpisode, IDs: core.IDs{TVDB: 1}, Title: "Item", Season: 1, Episode: 1})
	if err != nil || len(tv) == 0 {
		t.Fatalf("TV search: %v (%d)", err, len(tv))
	}
	if _, err := c.FetchNZB(ctx, tv[0]); err != nil {
		t.Fatal(err)
	}

	all := capture.all()
	var capsUAs, searchUAs, nzbUAs []string
	for _, r := range all {
		switch {
		case r.query.Get("t") == "caps":
			capsUAs = append(capsUAs, r.ua)
		case r.path == "/nzb":
			nzbUAs = append(nzbUAs, r.ua)
		default:
			searchUAs = append(searchUAs, r.ua)
			if r.header.Get("Accept") != rssAccept["Accept"] {
				t.Errorf("%s Accept = %q", r.query.Get("t"), r.header.Get("Accept"))
			}
		}
	}
	want := []string{"Radarr/", "Sonarr/"}
	assertUAs := func(kind string, got []string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s requests = %v", kind, got)
		}
		for i, prefix := range want {
			if !strings.HasPrefix(got[i], prefix) {
				t.Errorf("%s request %d UA = %q, want %s*", kind, i, got[i], prefix)
			}
		}
	}
	assertUAs("caps", capsUAs)
	assertUAs("search", searchUAs)
	// Grabs follow the media recorded on the release.
	assertUAs("grab", nzbUAs)
}

func TestRequestOverrideAppliesToBothMedia(t *testing.T) {
	capture, srv := newIdentityServer(t)
	cfg := config.Indexer{ID: "ix", Name: "Fake", URL: srv.URL, APIKey: "k", Enabled: true, SearchIdentity: IdentityProwlarr}
	c := New(cfg, nil, Options{Identity: servarrPolicy(), Version: "1.2.3", SearchCacheTTL: -1})
	ctx := context.Background()
	if _, err := c.Search(ctx, core.SearchRequest{Type: core.TypeMovie, Title: "Item", Year: 2024}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Search(ctx, core.SearchRequest{Type: core.TypeEpisode, Title: "Item", Season: 1, Episode: 1}); err != nil {
		t.Fatal(err)
	}
	for _, r := range capture.all() {
		if !strings.HasPrefix(r.ua, "Prowlarr/") {
			t.Fatalf("%s UA = %q, want the indexer override", r.query.Get("t"), r.ua)
		}
	}
}

func TestPersonaTorrentFetchAsksForATorrent(t *testing.T) {
	capture, srv := newIdentityServer(t)
	cfg := config.Indexer{ID: "tz", Name: "TZ", URL: srv.URL, Enabled: true, Protocol: "torrent"}
	c := New(cfg, nil, Options{Identity: servarrPolicy(), Version: "1.2.3"})
	out, err := c.ResolveTorrent(context.Background(), core.Release{
		Name: "Show.S01", Indexer: "TZ", Protocol: core.ProtocolTorrent,
		NZBURL: srv.URL + "/torrent", MediaType: core.TypeEpisode,
	})
	if err != nil || out.InfoHash == "" {
		t.Fatalf("resolve: %+v %v", out, err)
	}
	reqs := capture.all()
	last := reqs[len(reqs)-1]
	if last.header.Get("Accept") != "application/x-bittorrent" || !strings.HasPrefix(last.ua, "Sonarr/") {
		t.Fatalf("persona torrent request: UA %q Accept %q", last.ua, last.header.Get("Accept"))
	}

	etherCapture, etherSrv := newIdentityServer(t)
	etherCfg := config.Indexer{ID: "tz", Name: "TZ", URL: etherSrv.URL, Enabled: true, Protocol: "torrent"}
	ether := New(etherCfg, nil, Options{Version: "1.2.3"})
	if _, err := ether.ResolveTorrent(context.Background(), core.Release{
		Name: "Show.S01", Indexer: "TZ", Protocol: core.ProtocolTorrent,
		NZBURL: etherSrv.URL + "/torrent", MediaType: core.TypeEpisode,
	}); err != nil {
		t.Fatal(err)
	}
	etherReqs := etherCapture.all()
	etherLast := etherReqs[len(etherReqs)-1]
	if etherLast.header.Get("Accept") != "" || etherLast.ua != "Ether/1.2.3" {
		t.Fatalf("ether torrent request: UA %q Accept %q", etherLast.ua, etherLast.header.Get("Accept"))
	}
}

func TestNoGenericIdentityOnIndexerTraffic(t *testing.T) {
	capture, srv := newIdentityServer(t)
	cfg := config.Indexer{ID: "ix", Name: "Fake", URL: srv.URL, APIKey: "k", Enabled: true}
	c := New(cfg, nil, Options{Version: "1.2.3", SearchCacheTTL: -1})
	ctx := context.Background()
	if _, err := c.Caps(ctx); err != nil {
		t.Fatal(err)
	}
	rels, err := c.Search(ctx, core.SearchRequest{Type: core.TypeMovie, Title: "Item", Year: 2024})
	if err != nil || len(rels) == 0 {
		t.Fatalf("search: %v (%d)", err, len(rels))
	}
	if _, err := c.FetchNZB(ctx, rels[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ResolveTorrent(ctx, core.Release{
		Name: "Item.2024", Indexer: "Fake", Protocol: core.ProtocolTorrent,
		NZBURL: srv.URL + "/torrent", MediaType: core.TypeMovie,
	}); err != nil {
		t.Fatal(err)
	}
	for _, r := range capture.all() {
		if r.ua == "" || strings.Contains(r.ua, "Go-http-client") || r.ua == "Ether/1.0" {
			t.Fatalf("%s request presented a generic identity: %q", r.query.Get("t"), r.ua)
		}
		if !strings.HasPrefix(r.ua, "Ether/1.2.3") {
			t.Fatalf("%s request UA = %q", r.query.Get("t"), r.ua)
		}
	}
}

func TestRequestLogNamesIdentityWithoutSecrets(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	capture, srv := newIdentityServer(t)
	cfg := config.Indexer{ID: "ix", Name: "Fake", URL: srv.URL, APIKey: "k", Enabled: true}
	policy := config.ClientIdentity{SearchMovies: IdentityCustom, UserAgent: "Custom/1.0", Headers: map[string]string{"X-Api-Key": "header-secret-value"}}
	c := New(cfg, nil, Options{Identity: policy, Version: "1.2.3", Logger: logger, SearchCacheTTL: -1})
	if _, err := c.Search(context.Background(), core.SearchRequest{Type: core.TypeMovie, Title: "Item", Year: 2024}); err != nil {
		t.Fatal(err)
	}
	out := logs.String()
	if !strings.Contains(out, "identity=custom") {
		t.Fatalf("resolved identity not logged: %s", out)
	}
	if strings.Contains(out, "header-secret-value") {
		t.Fatalf("custom header value leaked into logs: %s", out)
	}
	for _, r := range capture.all() {
		if r.header.Get("X-Api-Key") != "header-secret-value" {
			t.Fatalf("custom header not sent: %v", r.header)
		}
	}
}

func TestSearchCacheFollowsGlobalCustomIdentity(t *testing.T) {
	capture, srv := newIdentityServer(t)
	cfg := config.Indexer{ID: "ix", Name: "Fake", URL: srv.URL, APIKey: "k", Enabled: true}
	policy := config.ClientIdentity{
		SearchMovies: IdentityCustom,
		GrabTV:       config.IdentitySame,
		GrabMovies:   config.IdentitySame,
		UserAgent:    "One/1.0",
		Headers:      map[string]string{"X-Api-Key": "one"},
	}
	c := New(cfg, nil, Options{Identity: policy, Version: "1.2.3", SearchCacheTTL: time.Hour})
	req := core.SearchRequest{Type: core.TypeMovie, Title: "Item", Year: 2024}
	if _, err := c.Search(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if got := len(capture.searchRequests()); got != 1 {
		t.Fatalf("first search requests = %d", got)
	}
	if _, err := c.Search(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if got := len(capture.searchRequests()); got != 1 {
		t.Fatalf("cached search made another request: %d", got)
	}
	c.SetIdentityPolicy(config.ClientIdentity{
		SearchMovies: IdentityCustom,
		GrabTV:       config.IdentitySame,
		GrabMovies:   config.IdentitySame,
		UserAgent:    "Two/2.0",
		Headers:      map[string]string{"X-Api-Key": "two"},
	})
	if _, err := c.Search(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if got := len(capture.searchRequests()); got != 2 {
		t.Fatalf("new custom headers reused cached results: %d requests", got)
	}
}

func TestConnectionTestPresentsTheTVPersonaShape(t *testing.T) {
	capture, srv := newIdentityServer(t)
	policy := config.ClientIdentity{
		SearchTV:   IdentitySonarr,
		GrabTV:     config.IdentitySame,
		GrabMovies: config.IdentitySame,
	}
	err := TestWithOptions(context.Background(), config.Indexer{
		ID: "ix", Name: "Fake", URL: srv.URL, APIKey: "k", Enabled: true,
	}, Options{Identity: policy, Version: "1.2.3"})
	if err != nil {
		t.Fatal(err)
	}
	var probe *capturedRequest
	for _, r := range capture.all() {
		if r.query.Get("t") == "search" {
			probe = &r
			break
		}
	}
	if probe == nil {
		t.Fatal("connection test made no search request")
	}
	if !strings.HasPrefix(probe.ua, "Sonarr/") {
		t.Errorf("probe UA = %q", probe.ua)
	}
	if probe.query.Get("cat") != "5000" || probe.query.Get("offset") != "0" {
		t.Errorf("probe query = %v, want the TV persona shape", probe.query.Encode())
	}
}
