package newznab

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

const personaCaps = `<caps><searching>` +
	`<search available="yes" supportedParams="q"/>` +
	`<movie-search available="yes" supportedParams="q,imdbid,tmdbid"/>` +
	`<tv-search available="yes" supportedParams="q,tvdbid,imdbid,season,ep"/>` +
	`</searching></caps>`

func queryTestClient(t *testing.T, cfg config.Indexer) *Client {
	t.Helper()
	if cfg.URL == "" {
		cfg.URL = "https://ix.example"
	}
	if cfg.APIKey == "" {
		cfg.APIKey = "KEY"
	}
	cfg.Enabled = true
	return New(cfg, nil, Options{})
}

func attemptQuery(t *testing.T, c *Client, ident resolvedIdentity, params url.Values) string {
	t.Helper()
	u, err := c.apiURLWith(params, queryOrder(ident.profile))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	return parsed.RawQuery
}

func TestPersonaQueryStrings(t *testing.T) {
	c := queryTestClient(t, config.Indexer{})
	sonarr := presetIdentity(IdentitySonarr, "1.0", "", nil)
	radarr := presetIdentity(IdentityRadarr, "1.0", "", nil)

	tvReq := core.SearchRequest{Type: core.TypeEpisode, IDs: core.IDs{TVDB: 81189}, Title: "The Signal", Season: 1, Episode: 2}
	tv := c.personaSearchAttempts(tvReq, nil, sonarr)
	if len(tv) != 2 {
		t.Fatalf("TV attempts = %d", len(tv))
	}
	if got, want := attemptQuery(t, c, sonarr, tv[0]), "t=tvsearch&cat=5000&extended=1&apikey=KEY&offset=0&limit=100&tvdbid=81189&season=1&ep=2"; got != want {
		t.Errorf("TV ID query:\n got %s\nwant %s", got, want)
	}
	if got, want := attemptQuery(t, c, sonarr, tv[1]), "t=search&cat=5000&extended=1&apikey=KEY&offset=0&limit=100&q=The+Signal+S01E02"; got != want {
		t.Errorf("TV text query:\n got %s\nwant %s", got, want)
	}

	movieReq := core.SearchRequest{Type: core.TypeMovie, IDs: core.IDs{IMDB: "tt133093"}, Title: "The Signal", Year: 1999}
	movie := c.personaSearchAttempts(movieReq, nil, radarr)
	if len(movie) != 2 {
		t.Fatalf("movie attempts = %d", len(movie))
	}
	if got, want := attemptQuery(t, c, radarr, movie[0]), "t=movie&cat=2000&extended=1&apikey=KEY&offset=0&limit=100&imdbid=133093"; got != want {
		t.Errorf("movie ID query:\n got %s\nwant %s", got, want)
	}
	if got, want := attemptQuery(t, c, radarr, movie[1]), "t=search&cat=2000&extended=1&apikey=KEY&offset=0&limit=100&q=The+Signal+1999"; got != want {
		t.Errorf("movie text query:\n got %s\nwant %s", got, want)
	}

	// A season-pack request on a torrent indexer keeps the TV verb.
	torrent := queryTestClient(t, config.Indexer{Protocol: "torrent"})
	pack := torrent.personaSearchAttempts(tvReq, nil, sonarr)
	if len(pack) != 3 {
		t.Fatalf("season-pack attempts = %d", len(pack))
	}
	if got, want := attemptQuery(t, torrent, sonarr, pack[1]), "t=tvsearch&cat=5000&extended=1&apikey=KEY&offset=0&limit=100&tvdbid=81189&season=1"; got != want {
		t.Errorf("season-pack query:\n got %s\nwant %s", got, want)
	}

	// Prowlarr combines every supported identifier with the text query.
	prowlarr := presetIdentity(IdentityProwlarr, "1.0", "", nil)
	combined := c.personaSearchAttempts(tvReq, nil, prowlarr)
	if len(combined) != 1 {
		t.Fatalf("prowlarr attempts = %d", len(combined))
	}
	if got, want := attemptQuery(t, c, prowlarr, combined[0]), "t=tvsearch&cat=5000&extended=1&apikey=KEY&offset=0&limit=100&tvdbid=81189&season=1&ep=2&q=The+Signal"; got != want {
		t.Errorf("prowlarr query:\n got %s\nwant %s", got, want)
	}

	// Without any identifier a persona falls back to a text search.
	plain := c.personaSearchAttempts(core.SearchRequest{Type: core.TypeMovie, Title: "The Signal", Year: 1999}, nil, radarr)
	if len(plain) != 1 {
		t.Fatalf("text-only attempts = %d", len(plain))
	}
	if got := attemptQuery(t, c, radarr, plain[0]); !strings.HasPrefix(got, "t=search&") || !strings.Contains(got, "offset=0") {
		t.Errorf("text-only query = %s", got)
	}
}

func TestEtherQueryStaysCanonical(t *testing.T) {
	c := queryTestClient(t, config.Indexer{})
	req := core.SearchRequest{Type: core.TypeMovie, IDs: core.IDs{IMDB: "tt133093"}, Title: "The Signal", Year: 1999}
	attempts := c.searchAttempts(req, nil)
	if len(attempts) == 0 {
		t.Fatal("no attempts")
	}
	params := attempts[0]
	got, err := c.apiURLWith(params, queryOrder(profileCurrent))
	if err != nil {
		t.Fatal(err)
	}
	expected := url.Values{}
	for k, vs := range params {
		expected[k] = vs
	}
	expected.Set("apikey", "KEY")
	want := "https://ix.example/api?" + expected.Encode()
	if got != want {
		t.Fatalf("ether URL changed:\n got %s\nwant %s", got, want)
	}
	if queryOrder(profileCurrent) != nil {
		t.Fatal("ether must keep the canonical encoding")
	}
}

func TestPersonaRescueSkipsQueriesSearchSpent(t *testing.T) {
	c := queryTestClient(t, config.Indexer{})
	radarr := presetIdentity(IdentityRadarr, "1.0", "", nil)

	movie := core.SearchRequest{Type: core.TypeMovie, Title: "The Signal", Year: 1999}
	search := c.personaSearchAttempts(movie, nil, radarr)
	if len(search) != 1 || search[0].Get("q") != "The Signal 1999" {
		t.Fatalf("search = %v", search)
	}
	rescue := c.personaRescueAttempts(movie, nil, radarr)
	if len(rescue) != 1 || rescue[0].Get("q") != "The Signal" {
		t.Fatalf("rescue repeated the failed query: %v", rescue)
	}

	// Season-only TV: S02 was the search; rescue must broaden to Complete.
	tv := core.SearchRequest{Type: core.TypeEpisode, Title: "Show Name", Season: 2}
	search = c.personaSearchAttempts(tv, nil, radarr)
	if len(search) != 1 || search[0].Get("q") != "Show Name S02" {
		t.Fatalf("TV search = %v", search)
	}
	rescue = c.personaRescueAttempts(tv, nil, radarr)
	if len(rescue) != 1 || rescue[0].Get("q") != "Show Name Complete" {
		t.Fatalf("TV rescue repeated the failed query: %v", rescue)
	}
}

func TestPersonaSearchUsesPersonaURL(t *testing.T) {
	var rawQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("t") == "caps" {
			fmt.Fprint(w, personaCaps)
			return
		}
		rawQuery = r.URL.RawQuery
		fmt.Fprint(w, `<rss><channel><item><title>Item.2024.1080p</title><guid>g</guid></item></channel></rss>`)
	}))
	defer srv.Close()
	c := New(config.Indexer{ID: "ix", Name: "Fake", URL: srv.URL, APIKey: "k", Enabled: true}, nil, Options{
		Identity: servarrPolicy(), Version: "1.0", SearchCacheTTL: -1,
	})
	if _, err := c.Search(context.Background(), core.SearchRequest{Type: core.TypeMovie, IDs: core.IDs{IMDB: "tt1"}, Title: "Item", Year: 2024}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(rawQuery, "t=movie&cat=2000&extended=1&apikey=k&offset=0&limit=100&imdbid=1") {
		t.Fatalf("wire query = %s", rawQuery)
	}
}
