package collections_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/braedonsaunders/ether/internal/collections"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/llm"
)

// letterboxdPage1 uses the classic poster markup; page 2 the newer
// LazyPoster components.
const letterboxdPage1 = `<html><body><ul class="poster-list">
<li class="poster-container"><div class="really-lazy-load poster film-poster film-poster-1" data-film-id="1" data-film-slug="time-car" data-target-link="/film/time-car/"><img alt="Time Car" src="x.jpg"/></div></li>
<li class="poster-container"><div class="really-lazy-load poster film-poster" data-film-id="2" data-film-slug="mystery-film-x" data-target-link="/film/mystery-film-x/"><img alt="Mystery Film X" /></div></li>
</ul><div class="pagination"><a class="next" href="/someone/list/favourites/page/2/">Older</a></div></body></html>`

const letterboxdPage2 = `<html><body>
<div class="react-component" data-component-class="LazyPoster" data-item-name="Dream Heist (2010)" data-item-slug="dream-heist" data-item-link="/film/dream-heist/"></div>
<div class="react-component" data-component-class="LazyPoster" data-item-name="Nowhere Film (1999)" data-item-slug="nowhere-film"></div>
</body></html>`

func TestExternalListSources(t *testing.T) {
	var mu sync.Mutex
	hits := map[string]int{}
	ext := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits[r.URL.Path]++
		mu.Unlock()
		if !strings.HasPrefix(r.Header.Get("User-Agent"), "Ether") {
			http.Error(w, "identify yourself", http.StatusForbidden)
			return
		}
		switch r.URL.Path {
		case "/users/alice/lists/greats/items":
			if r.Header.Get("trakt-api-key") != "client-123" || r.Header.Get("trakt-api-version") != "2" {
				http.Error(w, "no key", http.StatusUnauthorized)
				return
			}
			w.Header().Set("X-Pagination-Page-Count", "2")
			if r.URL.Query().Get("page") == "2" {
				fmt.Fprint(w, `[{"type":"show","show":{"title":"Office Life","year":2005,"ids":{"tmdb":502}}}]`)
				return
			}
			fmt.Fprint(w, `[{"type":"movie","movie":{"title":"Space Saga","year":1977,"ids":{"tmdb":101,"imdb":"tt0000101"}}},
				{"type":"movie","movie":{"title":"Prison Hope","year":1994,"ids":{"imdb":"tt0000105"}}},
				{"type":"episode","show":{"title":"Chemistry Teacher","ids":{"tmdb":501}}}]`)
		case "/someone/list/favourites/":
			fmt.Fprint(w, letterboxdPage1)
		case "/someone/list/favourites/page/2/":
			fmt.Fprint(w, letterboxdPage2)
		case "/film/mystery-film-x/":
			fmt.Fprint(w, `<html><body class="film" data-tmdb-type="movie" data-tmdb-id="111"></body></html>`)
		case "/film/nowhere-film/":
			fmt.Fprint(w, `<html><body>no ids here</body></html>`)
		case "/lists/bob/top-picks/json":
			fmt.Fprint(w, `[{"id":108,"title":"Knight Returns","imdb_id":"tt0000108","mediatype":"movie","release_year":2008,"rank":1},
				{"id":509,"title":"Space Show","mediatype":"show","release_year":2013,"rank":2}]`)
		case "/lists/bob/split/json":
			fmt.Fprint(w, `{"movies":[{"tmdb_id":104,"title":"Cyborg Hunter"}],"shows":[{"id":506,"title":"Nordic Bridge"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ext.Close()
	bases := collections.ExternalBases{TraktAPI: ext.URL, Letterboxd: ext.URL, MDBList: ext.URL}
	e := newEnv(t, withOpts(collections.WithExternalBases(bases), collections.WithUserAgent("Ether/test"),
		collections.WithTraktConfig(func() config.Trakt { return config.Trakt{ClientID: "client-123"} })))
	preview := func(src core.CollectionSource) *core.CollectionPreview {
		t.Helper()
		sp := spec("list", "mixed", []core.CollectionSource{src})
		sp.Sort, sp.SortDesc = "source", false
		p, err := e.svc.Preview(e.ctx, sp, 60)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}

	trakt := preview(core.CollectionSource{Kind: "traktList", URL: "https://trakt.tv/users/alice/lists/greats"})
	if got := strings.Join(titlesOf(trakt), ","); got != "Space Saga,Prison Hope,Chemistry Teacher,Office Life" {
		t.Fatalf("trakt: %s %v", got, trakt.Warnings)
	}

	lb := preview(core.CollectionSource{Kind: "letterboxdList", URL: "https://letterboxd.com/someone/list/favourites/"})
	got := titlesOf(lb)
	if strings.Join(got, ",") != "Time Car,Quiet Gem,Dream Heist" {
		t.Fatalf("letterboxd: %v %v", got, lb.Warnings)
	}
	if !strings.Contains(strings.Join(lb.Warnings, " "), "1 of 4") {
		t.Fatalf("letterboxd should report the unmatched film: %v", lb.Warnings)
	}
	// Matches are cached: a second evaluation does not refetch film pages.
	before := hits["/film/mystery-film-x/"]
	preview(core.CollectionSource{Kind: "letterboxdList", URL: "https://letterboxd.com/someone/list/favourites/"})
	if hits["/film/mystery-film-x/"] != before {
		t.Fatal("letterboxd film page refetched")
	}

	md := preview(core.CollectionSource{Kind: "mdblist", URL: "https://mdblist.com/lists/bob/top-picks"})
	if strings.Join(titlesOf(md), ",") != "Knight Returns,Space Show" {
		t.Fatalf("mdblist: %v %v", titlesOf(md), md.Warnings)
	}
	split := preview(core.CollectionSource{Kind: "mdblist", URL: "https://mdblist.com/lists/bob/split/"})
	if strings.Join(titlesOf(split), ",") != "Cyborg Hunter,Nordic Bridge" {
		t.Fatalf("mdblist object shape: %v %v", titlesOf(split), split.Warnings)
	}

	// Missing lists and missing credentials are warnings.
	missing := preview(core.CollectionSource{Kind: "mdblist", URL: "https://mdblist.com/lists/bob/gone"})
	if missing.Total != 0 || len(missing.Warnings) == 0 {
		t.Fatalf("missing list: %+v", missing)
	}
	e2 := newEnv(t, withOpts(collections.WithExternalBases(bases)))
	p, err := e2.svc.Preview(e2.ctx, spec("t", "mixed", []core.CollectionSource{{Kind: "traktList", URL: "https://trakt.tv/users/alice/lists/greats"}}), 60)
	if err != nil || !strings.Contains(strings.Join(p.Warnings, " "), "client ID") {
		t.Fatalf("trakt without a client id: %+v %v", p, err)
	}
}

// fakeLLM replays canned replies.
type fakeLLM struct {
	enabled bool
	replies []string
	mu      sync.Mutex
	calls   [][]llm.Message
}

func (f *fakeLLM) Enabled() bool { return f.enabled }

func (f *fakeLLM) Chat(ctx context.Context, msgs []llm.Message, jsonMode bool) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, msgs)
	if len(f.replies) == 0 {
		return "", errors.New("no more replies")
	}
	r := f.replies[0]
	f.replies = f.replies[1:]
	return r, nil
}

func TestGenerateDraftsResolvesAndValidates(t *testing.T) {
	reply := "Sure! Here you go:\n```json\n" + `{
	  "spec": {
	    "name": "80s Sci-Fi Critics Loved",
	    "description": "Science fiction from the 1980s under two hours.",
	    "mediaType": "movie",
	    "sources": [{"kind": "catalog"}, {"kind": "tmdbKeyword", "label": "time loop"}, {"kind": "letterboxdList", "url": "https://evil.example"}],
	    "match": "all",
	    "rules": [
	      {"field": "genre", "op": "is", "value": "Science Fiction"},
	      {"field": "decade", "op": "=", "value": 1980},
	      {"field": "runtime", "op": "<=", "value": 120},
	      {"field": "rating", "op": "gte", "value": 7.5},
	      {"field": "keyword", "op": "in", "value": ["time loop"]},
	      {"field": "vibes", "op": "is", "value": "rad"}
	    ],
	    "sort": "rating",
	    "limit": 50
	  },
	  "explanation": "Sci-fi genre, the 1980s, runtime at most 120 minutes and a high rating."
	}` + "\n```"
	f := &fakeLLM{enabled: true, replies: []string{reply}}
	e := newEnv(t, withOpts(collections.WithLLM(f)))
	sp, explanation, err := e.svc.Generate(e.ctx, "80s sci-fi under 2 hours that critics loved", "")
	if err != nil {
		t.Fatal(err)
	}
	if sp.Name != "80s Sci-Fi Critics Loved" || sp.MediaType != "movie" || sp.Sort != "rating" || sp.Limit != 50 || !sp.SortDesc {
		t.Fatalf("spec: %+v", sp)
	}
	if len(sp.Rules) != 5 {
		t.Fatalf("the invalid rule should be dropped, kept %d: %+v", len(sp.Rules), sp.Rules)
	}
	if sp.Rules[1].Op != "is" || sp.Rules[2].Op != "lte" {
		t.Fatalf("ops not normalised: %+v", sp.Rules)
	}
	if !strings.Contains(string(sp.Rules[4].Value), `"id":1002`) {
		t.Fatalf("keyword not resolved to a TMDB id: %s", sp.Rules[4].Value)
	}
	if len(sp.Sources) != 2 || sp.Sources[1].ID != 1002 {
		t.Fatalf("sources: %+v", sp.Sources)
	}
	if !strings.Contains(explanation, "runtime") || !strings.Contains(explanation, "Dropped") {
		t.Fatalf("explanation: %q", explanation)
	}
	if err := collections.Validate(sp); err != nil {
		t.Fatalf("generated spec invalid: %v", err)
	}
	if _, err := e.svc.Preview(e.ctx, *sp, 10); err != nil {
		t.Fatalf("generated spec does not preview: %v", err)
	}
	if !strings.Contains(f.calls[0][0].Content, "JSON") || !strings.Contains(f.calls[0][1].Content, "critics loved") {
		t.Fatalf("prompt: %+v", f.calls[0])
	}
}

func TestGenerateRetriesMalformedOutputAndReportsFailures(t *testing.T) {
	good, _ := json.Marshal(map[string]any{"spec": map[string]any{"name": "Cosy", "mediaType": "movie",
		"rules": []map[string]any{{"field": "genre", "op": "in", "value": []string{"Comedy", "Family"}}}}, "explanation": "comfy"})
	f := &fakeLLM{enabled: true, replies: []string{"I think you would enjoy some films!", string(good)}}
	e := newEnv(t, withOpts(collections.WithLLM(f)))
	sp, _, err := e.svc.Generate(e.ctx, "something cosy", "show")
	if err != nil {
		t.Fatal(err)
	}
	if sp.Name != "Cosy" || sp.MediaType != "show" || len(f.calls) != 2 {
		t.Fatalf("retry: %+v calls=%d", sp, len(f.calls))
	}
	if !strings.Contains(f.calls[1][len(f.calls[1])-1].Content, "could not be used") {
		t.Fatalf("retry prompt: %+v", f.calls[1])
	}

	bad := &fakeLLM{enabled: true, replies: []string{"nope", `{"spec": {"description": "no name"}}`}}
	e2 := newEnv(t, withOpts(collections.WithLLM(bad)))
	_, _, err = e2.svc.Generate(e2.ctx, "anything", "")
	var up interface{ Upstream() bool }
	if !errors.As(err, &up) {
		t.Fatalf("want an upstream error, got %v", err)
	}

	off := newEnv(t, withOpts(collections.WithLLM(&fakeLLM{enabled: false})))
	_, _, err = off.svc.Generate(off.ctx, "anything", "")
	var nc interface{ NotConfigured() bool }
	if !errors.As(err, &nc) || !errors.Is(err, collections.ErrLLMDisabled) {
		t.Fatalf("disabled: %v", err)
	}
	none := newEnv(t)
	if _, _, err := none.svc.Generate(none.ctx, "x", ""); !errors.Is(err, collections.ErrLLMDisabled) {
		t.Fatalf("no llm: %v", err)
	}
}

func TestLookupsAndFacets(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		kind, q string
		want    string
	}{
		{"keyword", "twist", "plot twist"},
		{"company", "pix", "Pixar"},
		{"person", "Christopher Nolan", "Christopher Nolan"},
		{"collection", "saga", "Space Saga Collection"},
		{"network", "netf", "Netflix"},
		{"network", "174", "AMC"},
		{"watchProvider", "disney", "Disney Plus"},
		{"keyword", "1005", "dream"},
	}
	for _, c := range cases {
		res, err := e.svc.Lookup(e.ctx, c.kind, c.q, "")
		if err != nil {
			t.Fatalf("%s %q: %v", c.kind, c.q, err)
		}
		if len(res) == 0 || res[0].Name != c.want {
			t.Fatalf("%s %q: %+v", c.kind, c.q, res)
		}
	}
	if res, _ := e.svc.Lookup(e.ctx, "watchProvider", "", "GB"); len(res) != len(providerList) || res[0].Subtitle != "GB" || res[0].ImagePath == "" {
		t.Fatalf("all providers: %+v", res)
	}
	if _, err := e.svc.Lookup(e.ctx, "studio", "x", ""); err == nil {
		t.Fatal("unknown lookup kind accepted")
	}
	langs, err := e.svc.Facets(e.ctx, core.TypeMovie, "language")
	if err != nil {
		t.Fatal(err)
	}
	foundKo := false
	for _, v := range langs {
		if v.Key == "ko" && v.Title == "Korean" && v.Count == 1 {
			foundKo = true
		}
	}
	if !foundKo {
		t.Fatalf("language facet: %+v", langs)
	}
	countries, err := e.svc.Facets(e.ctx, "", "country")
	if err != nil {
		t.Fatal(err)
	}
	se := 0
	for _, v := range countries {
		if v.Key == "SE" {
			se = v.Count
			if v.Title != "Sweden" {
				t.Fatalf("country title: %+v", v)
			}
		}
	}
	if se != 2 { // a film stored by name and a show stored by code merge
		t.Fatalf("country facet: %+v", countries)
	}
	if _, err := e.svc.Facets(e.ctx, core.TypeMovie, "mood"); err == nil {
		t.Fatal("unknown facet accepted")
	}
	genres, err := e.svc.Facets(e.ctx, core.TypeMovie, "genre")
	if err != nil || len(genres) == 0 {
		t.Fatalf("genre facet: %v %v", genres, err)
	}
	ratings, err := e.svc.Facets(e.ctx, core.TypeMovie, "contentRating")
	if err != nil {
		t.Fatalf("contentRating facet: %v", err)
	}
	// One value per age level, mildest first, then "Not rated"; each lists
	// its equivalent labels so film and TV ratings are one choice.
	var keys []string
	for _, v := range ratings {
		keys = append(keys, v.Key)
	}
	if strings.Join(keys, ",") != "G,TV-Y7,PG,PG-13,R,NC-17,NR" {
		t.Fatalf("contentRating levels %v", keys)
	}
	if ratings[2].Title != "PG · TV-PG" || !slices.Contains(ratings[2].Labels, "TV-PG") || !slices.Contains(ratings[2].Labels, "gb/PG") {
		t.Fatalf("PG level %+v", ratings[2])
	}
	items, err := e.svc.SearchTitles(e.ctx, "Dream", core.TypeMovie, 5)
	if err != nil || len(items) == 0 || items[0].Title != "Dream Heist" {
		t.Fatalf("search: %+v %v", items, err)
	}
	d, err := e.svc.TitleDetail(e.ctx, core.MovieKey(107))
	if err != nil || d.Title != "Dream Heist" {
		t.Fatalf("detail: %+v %v", d, err)
	}
}
