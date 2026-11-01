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
	"github.com/braedonsaunders/ether/internal/core"
)

type playRequest struct {
	verb string
	ua   string
	raw  string
}

type playCapture struct {
	mu   sync.Mutex
	reqs []playRequest
}

func (c *playCapture) add(r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reqs = append(c.reqs, playRequest{
		verb: r.URL.Query().Get("t"), ua: r.Header.Get("User-Agent"), raw: r.URL.RawQuery,
	})
}

func (c *playCapture) all() []playRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]playRequest(nil), c.reqs...)
}

// newPlayIndexer serves caps, one item per search verb, an NZB and records
// every request.
func newPlayIndexer(t *testing.T) (*playCapture, *httptest.Server) {
	t.Helper()
	capture := &playCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capture.add(r)
		q := r.URL.Query()
		switch {
		case r.URL.Path == "/nzb":
			fmt.Fprint(w, fakeNZB)
		case q.Get("t") == "caps":
			fmt.Fprint(w, personaCaps)
		case q.Get("t") == "movie", q.Get("t") == "tvsearch", q.Get("t") == "search":
			category := 2040
			if q.Get("t") != "movie" {
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

func requestAt(requests []playRequest, verb string) []playRequest {
	var out []playRequest
	for _, r := range requests {
		if r.verb == verb {
			out = append(out, r)
		}
	}
	return out
}

func TestEndToEndIdentityPrecedence(t *testing.T) {
	aCapture, aSrv := newPlayIndexer(t)
	bCapture, bSrv := newPlayIndexer(t)
	policy := config.ClientIdentity{
		SearchTV: IdentitySonarr, SearchMovies: IdentityRadarr,
		GrabTV: config.IdentitySame, GrabMovies: config.IdentitySame,
	}
	m := NewManager(nil, []config.Indexer{
		{ID: "a", Name: "A", URL: aSrv.URL, APIKey: "ka", Enabled: true},
		{ID: "b", Name: "B", URL: bSrv.URL, APIKey: "kb", Enabled: true, SearchIdentity: IdentityProwlarr},
	}, Options{Identity: policy, Version: "1.2.3", SearchCacheTTL: -1})
	ctx := context.Background()

	movieReq := core.SearchRequest{Type: core.TypeMovie, IDs: core.IDs{IMDB: "tt1", TMDB: 2}, Title: "Item", Year: 2024}
	tvReq := core.SearchRequest{Type: core.TypeEpisode, IDs: core.IDs{TVDB: 1, IMDB: "tt9"}, Title: "Show", Season: 1, Episode: 2}

	movie, err := m.Search(ctx, movieReq)
	if err != nil || len(movie) == 0 {
		t.Fatalf("movie search: %v (%d)", err, len(movie))
	}
	tv, err := m.Search(ctx, tvReq)
	if err != nil || len(tv) == 0 {
		t.Fatalf("TV search: %v (%d)", err, len(tv))
	}
	for _, r := range movie {
		if _, err := m.FetchNZB(ctx, r); err != nil {
			t.Fatalf("movie grab %s: %v", r.Indexer, err)
		}
	}
	for _, r := range tv {
		if _, err := m.FetchNZB(ctx, r); err != nil {
			t.Fatalf("TV grab %s: %v", r.Indexer, err)
		}
	}

	a := aCapture.all()
	b := bCapture.all()

	// Identities follow the precedence order on every request kind.
	for _, r := range requestAt(a, "movie") {
		if !strings.HasPrefix(r.ua, "Radarr/") {
			t.Errorf("A movie search UA = %q", r.ua)
		}
	}
	for _, r := range requestAt(a, "tvsearch") {
		if !strings.HasPrefix(r.ua, "Sonarr/") {
			t.Errorf("A TV search UA = %q", r.ua)
		}
	}
	for _, r := range requestAt(b, "movie") {
		if !strings.HasPrefix(r.ua, "Prowlarr/") {
			t.Errorf("B movie search UA = %q", r.ua)
		}
	}
	for _, r := range requestAt(b, "tvsearch") {
		if !strings.HasPrefix(r.ua, "Prowlarr/") {
			t.Errorf("B TV search UA = %q", r.ua)
		}
	}

	// Persona query shape: verb, ordered keys, offset on the first page.
	wantMovieA := "t=movie&cat=2000&extended=1&apikey=ka&offset=0&limit=100&imdbid=1"
	if got := requestAt(a, "movie")[0].raw; !strings.HasPrefix(got, wantMovieA) {
		t.Errorf("A movie query = %s", got)
	}
	wantTVA := "t=tvsearch&cat=5000&extended=1&apikey=ka&offset=0&limit=100&tvdbid=1&season=1&ep=2"
	if got := requestAt(a, "tvsearch")[0].raw; !strings.HasPrefix(got, wantTVA) {
		t.Errorf("A TV query = %s", got)
	}
	if got := requestAt(b, "movie")[0].raw; !strings.Contains(got, "&imdbid=1&tmdbid=2&q=Item") {
		t.Errorf("B prowlarr movie query did not combine identifiers and text: %s", got)
	}

	// Grabs follow the media recorded on the release.
	var grabUAs []string
	for _, r := range a {
		if r.verb == "" {
			grabUAs = append(grabUAs, r.ua)
		}
	}
	if len(grabUAs) != 2 || !strings.HasPrefix(grabUAs[0], "Radarr/") || !strings.HasPrefix(grabUAs[1], "Sonarr/") {
		t.Errorf("A grab identities = %v", grabUAs)
	}
	for _, r := range b {
		if r.verb == "" && !strings.HasPrefix(r.ua, "Prowlarr/") {
			t.Errorf("B grab UA = %q", r.ua)
		}
	}

	// Feeds split per media side, with the side's identity and verb.
	feeds := m.FeedSources()
	if len(feeds) != 4 {
		t.Fatalf("feeds = %+v", feeds)
	}
	ids := map[string]bool{}
	for _, f := range feeds {
		ids[f.ID] = true
	}
	for _, want := range []string{"a#tv", "a#movie", "b#tv", "b#movie"} {
		if !ids[want] {
			t.Fatalf("missing feed %s", want)
		}
	}
	before := len(a)
	if _, err := m.Recent(ctx, "a#tv", 100, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Recent(ctx, "a#movie", 100, 100); err != nil {
		t.Fatal(err)
	}
	feedReqs := aCapture.all()[before:]
	if len(feedReqs) != 2 {
		t.Fatalf("feed requests = %d", len(feedReqs))
	}
	if feedReqs[0].verb != "tvsearch" || !strings.HasPrefix(feedReqs[0].ua, "Sonarr/") {
		t.Errorf("TV feed = %+v", feedReqs[0])
	}
	if feedReqs[1].verb != "movie" || !strings.HasPrefix(feedReqs[1].ua, "Radarr/") {
		t.Errorf("movie feed = %+v", feedReqs[1])
	}
}
