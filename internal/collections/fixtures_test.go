package collections_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/collections"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/tmdb"
)

// TMDB genre ids.
const (
	gAction, gAdventure, gAnimation, gComedy, gCrime, gDocumentary, gDrama = 28, 12, 16, 35, 80, 99, 18
	gFamily, gFantasy, gHorror, gMusic, gMystery, gRomance, gSciFi         = 10751, 14, 27, 10402, 9648, 10749, 878
	gThriller, gWar, gTVMovie                                              = 53, 10752, 10770
	gSciFiTV, gTalk, gReality                                              = 10765, 10767, 10764
)

var movieGenres = map[int]string{gAction: "Action", gAdventure: "Adventure", gAnimation: "Animation", gComedy: "Comedy",
	gCrime: "Crime", gDocumentary: "Documentary", gDrama: "Drama", gFamily: "Family", gFantasy: "Fantasy", gHorror: "Horror",
	gMusic: "Music", gMystery: "Mystery", gRomance: "Romance", gSciFi: "Science Fiction", gThriller: "Thriller", gWar: "War",
	gTVMovie: "TV Movie"}

var tvGenres = map[int]string{gAnimation: "Animation", gComedy: "Comedy", gCrime: "Crime", gDocumentary: "Documentary",
	gDrama: "Drama", gFamily: "Family", gMystery: "Mystery", gSciFiTV: "Sci-Fi & Fantasy", gTalk: "Talk", gReality: "Reality",
	10759: "Action & Adventure"}

// fx is one fixture title, served by the fake TMDB and (when inCatalog)
// seeded into the catalogue mirror.
type fx struct {
	id        int64
	tv        bool
	title     string
	date      string // YYYY-MM-DD; "" = unknown; "+30" = 30 days from now; "-10" = 10 days ago
	genres    []int
	lang      string
	votes     int
	rating    float64
	pop       float64
	runtime   int
	keywords  []int64
	companies []int64
	networks  []int64
	providers []int64
	cert      string
	countries []string
	tvType    int
	inCatalog bool
}

func (f fx) released(now time.Time) string {
	if f.date == "" {
		return ""
	}
	if f.date[0] == '+' || f.date[0] == '-' {
		n, _ := strconv.Atoi(f.date)
		return now.AddDate(0, 0, n).Format("2006-01-02")
	}
	return f.date
}

func (f fx) key() core.RatingKey {
	if f.tv {
		return core.ShowKey(f.id)
	}
	return core.MovieKey(f.id)
}

var fixtures = []fx{
	// Classics and decades.
	{id: 101, title: "Space Saga", date: "1977-05-25", genres: []int{gAdventure, gSciFi, gAction}, lang: "en", votes: 20000, rating: 8.2, pop: 90, runtime: 121, cert: "PG", inCatalog: true, companies: []int64{1}},
	{id: 102, title: "The Family Business", date: "1972-03-14", genres: []int{gDrama, gCrime}, lang: "en", votes: 21000, rating: 8.7, pop: 80, runtime: 175, cert: "R", inCatalog: true},
	{id: 103, title: "Time Car", date: "1985-07-03", genres: []int{gAdventure, gComedy, gSciFi}, lang: "en", votes: 19000, rating: 8.3, pop: 75, runtime: 116, cert: "PG", inCatalog: true, keywords: []int64{1002}},
	{id: 104, title: "Cyborg Hunter", date: "1984-10-26", genres: []int{gAction, gSciFi, gThriller}, lang: "en", votes: 13000, rating: 7.7, pop: 60, runtime: 107, cert: "R", inCatalog: true},
	{id: 105, title: "Prison Hope", date: "1994-09-23", genres: []int{gDrama, gCrime}, lang: "en", votes: 27000, rating: 8.7, pop: 85, runtime: 142, cert: "R", inCatalog: true, keywords: []int64{1006}},
	{id: 106, title: "Twisty Tale", date: "1994-10-14", genres: []int{gThriller, gCrime}, lang: "en", votes: 28000, rating: 8.5, pop: 70, runtime: 154, cert: "R", inCatalog: true, keywords: []int64{1001, 1003}},
	{id: 107, title: "Dream Heist", date: "2010-07-15", genres: []int{gAction, gSciFi, gAdventure}, lang: "en", votes: 36000, rating: 8.4, pop: 95, runtime: 148, cert: "PG-13", inCatalog: true, keywords: []int64{1001, 1005, 1004}},
	{id: 108, title: "Knight Returns", date: "2008-07-16", genres: []int{gAction, gCrime, gDrama}, lang: "en", votes: 32000, rating: 8.5, pop: 88, runtime: 152, cert: "PG-13", inCatalog: true},
	{id: 109, title: "Epic Kingdom", date: "2003-12-17", genres: []int{gAdventure, gFantasy, gAction}, lang: "en", votes: 25000, rating: 8.5, pop: 82, runtime: 201, cert: "PG-13", inCatalog: true, keywords: []int64{9672}},
	{id: 110, title: "Love Letters", date: "2004-06-25", genres: []int{gRomance, gDrama}, lang: "en", votes: 12000, rating: 7.9, pop: 50, runtime: 123, cert: "PG-13", inCatalog: true, keywords: []int64{160404, 1006}},
	{id: 111, title: "Quiet Gem", date: "2015-03-01", genres: []int{gDrama}, lang: "en", votes: 800, rating: 7.6, pop: 8, runtime: 98, cert: "PG-13", inCatalog: true},
	{id: 112, title: "Laugh Riot", date: "2017-08-01", genres: []int{gComedy, gFamily}, lang: "en", votes: 4000, rating: 7.3, pop: 40, runtime: 85, cert: "PG", inCatalog: true, keywords: []int64{3335}},
	{id: 113, title: "Animated Friends", date: "2019-06-21", genres: []int{gAnimation, gFamily, gComedy}, lang: "en", votes: 9000, rating: 7.8, pop: 66, runtime: 100, cert: "G", inCatalog: true, companies: []int64{3}},
	{id: 114, title: "Spirit Valley", date: "2001-07-20", genres: []int{gAnimation, gFamily, gFantasy}, lang: "ja", votes: 16000, rating: 8.5, pop: 64, runtime: 125, cert: "PG", inCatalog: true, companies: []int64{10342}},
	{id: 115, title: "Samurai Seven", date: "1954-04-26", genres: []int{gAction, gDrama}, lang: "ja", votes: 3500, rating: 8.5, pop: 20, runtime: 207, inCatalog: true},
	{id: 116, title: "Seoul Story", date: "2019-05-30", genres: []int{gThriller, gDrama, gComedy}, lang: "ko", votes: 18000, rating: 8.5, pop: 70, runtime: 133, cert: "R", inCatalog: true},
	{id: 117, title: "Paris Nights", date: "2001-04-25", genres: []int{gComedy, gRomance}, lang: "fr", votes: 11000, rating: 7.9, pop: 35, runtime: 122, cert: "R", inCatalog: true},
	{id: 118, title: "Madrid Mystery", date: "2016-09-01", genres: []int{gThriller, gMystery}, lang: "es", votes: 3000, rating: 7.6, pop: 25, runtime: 106, inCatalog: true},
	{id: 119, title: "Mumbai Dreams", date: "2009-12-25", genres: []int{gComedy, gDrama}, lang: "hi", votes: 2500, rating: 8.0, pop: 30, runtime: 170, inCatalog: true},
	{id: 120, title: "Stockholm Crime", date: "2011-01-01", genres: []int{gCrime, gThriller}, lang: "sv", votes: 900, rating: 7.1, pop: 12, runtime: 110, inCatalog: true, countries: []string{"Sweden"}},
	{id: 121, title: "Haunted House", date: "2013-07-18", genres: []int{gHorror, gThriller}, lang: "en", votes: 11000, rating: 7.5, pop: 60, runtime: 112, cert: "R", inCatalog: true, keywords: []int64{3335}},
	{id: 122, title: "Holiday Magic", date: "2011-11-23", genres: []int{gFamily, gComedy, gAnimation}, lang: "en", votes: 3000, rating: 7.2, pop: 30, runtime: 97, cert: "PG", inCatalog: true, keywords: []int64{207317}, companies: []int64{41077}},
	{id: 123, title: "Marvelous Hero", date: "2008-04-30", genres: []int{gAction, gSciFi}, lang: "en", votes: 26000, rating: 7.6, pop: 70, runtime: 126, cert: "PG-13", inCatalog: true, keywords: []int64{180547}},
	{id: 124, title: "Short and Sweet", date: "2014-02-01", genres: []int{gComedy}, lang: "en", votes: 900, rating: 7.0, pop: 15, runtime: 82, cert: "PG", inCatalog: false},
	{id: 125, title: "Brand New Film", date: "-10", genres: []int{gAction, gComedy}, lang: "en", votes: 300, rating: 7.0, pop: 99, runtime: 101, cert: "PG-13", inCatalog: true},
	{id: 126, title: "Future Blockbuster", date: "+30", genres: []int{gAction, gAdventure}, lang: "en", votes: 0, rating: 0, pop: 120, inCatalog: false},
	{id: 127, title: "Joined From TMDB", date: "2012-05-04", genres: []int{gAction, gAdventure, gSciFi}, lang: "en", votes: 30000, rating: 8.0, pop: 92, runtime: 143, cert: "PG-13", inCatalog: false, keywords: []int64{180547, 1004}, companies: []int64{420}, providers: []int64{8, 337}},
	{id: 128, title: "Indie Darling", date: "2018-04-06", genres: []int{gDrama, gHorror}, lang: "en", votes: 6000, rating: 7.4, pop: 30, runtime: 127, cert: "R", inCatalog: true, companies: []int64{41077}, providers: []int64{9}},
	// Shows.
	{id: 501, tv: true, title: "Chemistry Teacher", date: "2008-01-20", genres: []int{gDrama, gCrime}, lang: "en", votes: 15000, rating: 8.9, pop: 120, inCatalog: true, networks: []int64{174}, providers: []int64{8}},
	{id: 502, tv: true, title: "Office Life", date: "2005-03-24", genres: []int{gComedy}, lang: "en", votes: 4000, rating: 8.6, pop: 110, inCatalog: true, providers: []int64{1899, 15}},
	{id: 503, tv: true, title: "Meltdown", date: "2019-05-06", genres: []int{gDrama}, lang: "en", votes: 6000, rating: 8.7, pop: 60, inCatalog: true, tvType: 2, providers: []int64{350, 2303}},
	{id: 504, tv: true, title: "Wild Planet", date: "2016-11-06", genres: []int{gDocumentary}, lang: "en", votes: 1200, rating: 9.0, pop: 40, inCatalog: true, tvType: 2},
	{id: 505, tv: true, title: "Seoul Romance", date: "2020-12-09", genres: []int{gComedy, gDrama}, lang: "ko", votes: 900, rating: 8.3, pop: 45, inCatalog: true, countries: []string{"KR"}},
	{id: 506, tv: true, title: "Nordic Bridge", date: "2011-09-21", genres: []int{gCrime, gDrama, gMystery}, lang: "sv", votes: 800, rating: 8.0, pop: 25, inCatalog: true, countries: []string{"SE"}},
	{id: 507, tv: true, title: "Late Night Talk", date: "2014-02-17", genres: []int{gTalk, gComedy}, lang: "en", votes: 400, rating: 7.6, pop: 70, inCatalog: true, providers: []int64{8}},
	{id: 508, tv: true, title: "Fresh Series", date: "-20", genres: []int{gDrama}, lang: "en", votes: 50, rating: 7.4, pop: 90, inCatalog: true},
	{id: 509, tv: true, title: "Space Show", date: "2013-12-02", genres: []int{gAnimation, gComedy, gSciFiTV}, lang: "en", votes: 9000, rating: 8.7, pop: 100, inCatalog: true, providers: []int64{1899}},
}

func fixtureByID(id int64, tv bool) (fx, bool) {
	for _, f := range fixtures {
		if f.id == id && f.tv == tv {
			return f, true
		}
	}
	return fx{}, false
}

var keywordNames = map[int64]string{
	1001: "plot twist", 1002: "time loop", 1003: "nonlinear timeline", 1004: "alternate reality", 1005: "dream",
	1006: "tearjerker", 1007: "tragedy", 9672: "based on true story", 3335: "halloween", 207317: "christmas",
	160404: "valentine's day", 180547: "marvel cinematic universe (mcu)",
}

var companyNames = map[int64]string{1: "Lucasfilm", 3: "Pixar", 420: "Marvel Studios", 10342: "Studio Ghibli", 41077: "A24"}

var providerList = []map[string]any{
	{"provider_id": 8, "provider_name": "Netflix", "logo_path": "/netflix.jpg", "display_priority": 1},
	{"provider_id": 337, "provider_name": "Disney Plus", "logo_path": "/disney.jpg", "display_priority": 2},
	{"provider_id": 9, "provider_name": "Amazon Prime Video", "logo_path": "/prime.jpg", "display_priority": 3},
	{"provider_id": 1899, "provider_name": "HBO Max", "logo_path": "/max.jpg", "display_priority": 4},
	{"provider_id": 350, "provider_name": "Apple TV", "logo_path": "/apple.jpg", "display_priority": 5},
	{"provider_id": 15, "provider_name": "Hulu", "logo_path": "/hulu.jpg", "display_priority": 6},
	{"provider_id": 2303, "provider_name": "Paramount Plus Premium", "logo_path": "/pp.jpg", "display_priority": 7},
	{"provider_id": 2616, "provider_name": "Paramount Plus Essential", "logo_path": "/pe.jpg", "display_priority": 8},
}

// fakeTMDB is an httptest TMDB that serves the fixtures and filters
// discover queries the way TMDB does for the parameters the engine uses.
type fakeTMDB struct {
	srv *httptest.Server
	now time.Time

	mu       sync.Mutex
	requests []string
}

func newFakeTMDB(t *testing.T) *fakeTMDB {
	t.Helper()
	f := &fakeTMDB{now: time.Now()}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeTMDB) seen(prefix string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, r := range f.requests {
		if strings.HasPrefix(r, prefix) {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeTMDB) item(x fx) map[string]any {
	m := map[string]any{"id": x.id, "overview": x.title + " overview", "poster_path": fmt.Sprintf("/p%d.jpg", x.id),
		"backdrop_path": fmt.Sprintf("/b%d.jpg", x.id), "genre_ids": x.genres, "original_language": x.lang,
		"popularity": x.pop, "vote_average": x.rating, "vote_count": x.votes}
	if x.tv {
		m["name"] = x.title
		m["first_air_date"] = x.released(f.now)
		m["origin_country"] = x.countries
	} else {
		m["title"] = x.title
		m["release_date"] = x.released(f.now)
	}
	return m
}

func page(items []map[string]any, pageNo int) map[string]any {
	const per = 20
	start := (pageNo - 1) * per
	end := start + per
	if start > len(items) {
		start = len(items)
	}
	if end > len(items) {
		end = len(items)
	}
	pages := (len(items) + per - 1) / per
	if pages == 0 {
		pages = 1
	}
	return map[string]any{"page": pageNo, "results": items[start:end], "total_pages": pages, "total_results": len(items)}
}

func writeJSONResp(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeTMDB) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.URL.Path+"?"+r.URL.RawQuery)
	f.mu.Unlock()
	q := r.URL.Query()
	pageNo, _ := strconv.Atoi(q.Get("page"))
	if pageNo < 1 {
		pageNo = 1
	}
	p := r.URL.Path
	parts := strings.Split(strings.Trim(p, "/"), "/")
	idAt := func(i int) int64 {
		if i < len(parts) {
			n, _ := strconv.ParseInt(parts[i], 10, 64)
			return n
		}
		return 0
	}
	switch {
	case p == "/genre/movie/list" || p == "/genre/tv/list":
		gm := movieGenres
		if strings.Contains(p, "/tv/") {
			gm = tvGenres
		}
		var gs []map[string]any
		for id, n := range gm {
			gs = append(gs, map[string]any{"id": id, "name": n})
		}
		writeJSONResp(w, map[string]any{"genres": gs})
	case p == "/discover/movie" || p == "/discover/tv":
		writeJSONResp(w, page(f.discover(strings.HasSuffix(p, "/tv"), q), pageNo))
	case p == "/search/keyword":
		var out []map[string]any
		for id, n := range keywordNames {
			if strings.Contains(n, strings.ToLower(q.Get("query"))) {
				out = append(out, map[string]any{"id": id, "name": n})
			}
		}
		sort.Slice(out, func(i, j int) bool { return len(out[i]["name"].(string)) < len(out[j]["name"].(string)) })
		writeJSONResp(w, map[string]any{"results": out})
	case p == "/search/company":
		var out []map[string]any
		for id, n := range companyNames {
			if strings.Contains(strings.ToLower(n), strings.ToLower(q.Get("query"))) {
				out = append(out, map[string]any{"id": id, "name": n, "logo_path": "/logo.png", "origin_country": "US"})
			}
		}
		writeJSONResp(w, map[string]any{"results": out})
	case p == "/search/person":
		writeJSONResp(w, map[string]any{"results": []map[string]any{
			{"id": 525, "name": q.Get("query"), "profile_path": "/person.jpg", "known_for_department": "Directing",
				"known_for": []map[string]any{{"title": "Dream Heist"}}},
		}})
	case p == "/search/collection":
		writeJSONResp(w, map[string]any{"results": []map[string]any{{"id": 10, "name": "Space Saga Collection", "poster_path": "/c10.jpg"}}})
	case p == "/search/movie" || p == "/search/tv":
		tv := strings.HasSuffix(p, "/tv")
		var out []map[string]any
		for _, x := range fixtures {
			if x.tv == tv && strings.EqualFold(x.title, q.Get("query")) {
				out = append(out, f.item(x))
			}
		}
		writeJSONResp(w, page(out, 1))
	case len(parts) == 2 && parts[0] == "keyword":
		if n, ok := keywordNames[idAt(1)]; ok {
			writeJSONResp(w, map[string]any{"id": idAt(1), "name": n})
			return
		}
		http.NotFound(w, r)
	case len(parts) == 2 && parts[0] == "company":
		if n, ok := companyNames[idAt(1)]; ok {
			writeJSONResp(w, map[string]any{"id": idAt(1), "name": n})
			return
		}
		http.NotFound(w, r)
	case len(parts) == 2 && parts[0] == "network":
		writeJSONResp(w, map[string]any{"id": idAt(1), "name": fmt.Sprintf("Network %d", idAt(1))})
	case len(parts) == 2 && parts[0] == "person":
		writeJSONResp(w, map[string]any{"id": idAt(1), "name": fmt.Sprintf("Person %d", idAt(1))})
	case len(parts) == 3 && parts[0] == "person" && parts[2] == "combined_credits":
		cast := []map[string]any{}
		for _, id := range []int64{107, 108, 103} {
			x, _ := fixtureByID(id, false)
			m := f.item(x)
			m["media_type"], m["character"] = "movie", "Lead"
			cast = append(cast, m)
		}
		talk, _ := fixtureByID(507, true)
		tm := f.item(talk)
		tm["media_type"], tm["character"] = "tv", "Self - Guest"
		cast = append(cast, tm)
		crew := []map[string]any{}
		for _, id := range []int64{107, 114, 106} {
			x, _ := fixtureByID(id, false)
			m := f.item(x)
			m["media_type"], m["job"], m["department"] = "movie", "Director", "Directing"
			crew = append(crew, m)
		}
		writeJSONResp(w, map[string]any{"cast": cast, "crew": crew})
	case len(parts) == 2 && parts[0] == "collection":
		id := idAt(1)
		var partsOut []map[string]any
		for _, x := range []int64{101, 103} {
			fx, _ := fixtureByID(x, false)
			partsOut = append(partsOut, f.item(fx))
		}
		writeJSONResp(w, map[string]any{"id": id, "name": fmt.Sprintf("Collection %d", id), "overview": "A saga.",
			"poster_path": "/c.jpg", "backdrop_path": "/cb.jpg", "parts": partsOut})
	case p == "/watch/providers/movie" || p == "/watch/providers/tv":
		writeJSONResp(w, map[string]any{"results": providerList})
	case len(parts) == 2 && (parts[0] == "movie" || parts[0] == "tv") && idAt(1) == 0:
		writeJSONResp(w, page(f.list(parts[0] == "tv", parts[1]), pageNo))
	case len(parts) == 3 && parts[0] == "trending":
		var out []map[string]any
		for _, x := range f.list(parts[1] == "tv", "popular") {
			out = append(out, x)
		}
		if parts[1] == "all" {
			out = nil
			for _, x := range fixtures {
				if x.inCatalog && x.votes > 5000 {
					m := f.item(x)
					m["media_type"] = map[bool]string{true: "tv", false: "movie"}[x.tv]
					out = append(out, m)
				}
			}
		}
		writeJSONResp(w, page(out, pageNo))
	case len(parts) == 2 && (parts[0] == "movie" || parts[0] == "tv"):
		x, ok := fixtureByID(idAt(1), parts[0] == "tv")
		if !ok {
			http.NotFound(w, r)
			return
		}
		m := f.item(x)
		m["runtime"] = x.runtime
		m["genres"] = []map[string]any{}
		gm := movieGenres
		if x.tv {
			gm = tvGenres
		}
		for _, g := range x.genres {
			m["genres"] = append(m["genres"].([]map[string]any), map[string]any{"id": g, "name": gm[g]})
		}
		m["external_ids"] = map[string]any{"imdb_id": fmt.Sprintf("tt%07d", x.id)}
		writeJSONResp(w, m)
	case len(parts) == 2 && parts[0] == "find":
		id := strings.TrimPrefix(parts[1], "tt")
		n, _ := strconv.ParseInt(id, 10, 64)
		out := map[string]any{"movie_results": []any{}, "tv_results": []any{}}
		if x, ok := fixtureByID(n, false); ok {
			out["movie_results"] = []any{f.item(x)}
		} else if x, ok := fixtureByID(n, true); ok {
			out["tv_results"] = []any{f.item(x)}
		}
		writeJSONResp(w, out)
	default:
		http.NotFound(w, r)
	}
}

// list serves TMDB's curated lists from the fixtures.
func (f *fakeTMDB) list(tv bool, name string) []map[string]any {
	var xs []fx
	for _, x := range fixtures {
		if x.tv != tv {
			continue
		}
		rel := x.released(f.now)
		future := rel != "" && rel > f.now.Format("2006-01-02")
		if name == "upcoming" || name == "on_the_air" {
			if !future && name == "upcoming" {
				continue
			}
		} else if future {
			continue
		}
		xs = append(xs, x)
	}
	sort.SliceStable(xs, func(i, j int) bool {
		if name == "top_rated" {
			return xs[i].rating > xs[j].rating
		}
		return xs[i].pop > xs[j].pop
	})
	out := make([]map[string]any, 0, len(xs))
	for _, x := range xs {
		out = append(out, f.item(x))
	}
	return out
}

func idSet(v string) (ids []int64, all bool) {
	all = !strings.Contains(v, "|")
	for _, p := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == '|' }) {
		n, _ := strconv.ParseInt(strings.TrimSpace(p), 10, 64)
		ids = append(ids, n)
	}
	return ids, all
}

func hasIDs(have []int64, want []int64, all bool) bool {
	in := func(x int64) bool {
		for _, h := range have {
			if h == x {
				return true
			}
		}
		return false
	}
	for _, w := range want {
		if in(w) && !all {
			return true
		}
		if !in(w) && all {
			return false
		}
	}
	return all
}

func ints(xs []int) []int64 {
	out := make([]int64, len(xs))
	for i, x := range xs {
		out[i] = int64(x)
	}
	return out
}

// discover filters fixtures by the discover parameters the engine sends.
func (f *fakeTMDB) discover(tv bool, q url.Values) []map[string]any {
	var xs []fx
	certRank := map[string]int{"G": 1, "PG": 2, "PG-13": 3, "R": 4, "NC-17": 5}
	for _, x := range fixtures {
		if x.tv != tv {
			continue
		}
		ok := true
		check := func(param string, have []int64) {
			if v := q.Get(param); v != "" && ok {
				want, all := idSet(v)
				if !hasIDs(have, want, all) {
					ok = false
				}
			}
		}
		check("with_genres", ints(x.genres))
		check("with_keywords", x.keywords)
		check("with_companies", x.companies)
		check("with_networks", x.networks)
		check("with_watch_providers", x.providers)
		if v := q.Get("without_genres"); v != "" {
			want, _ := idSet(v)
			if hasIDs(ints(x.genres), want, false) {
				ok = false
			}
		}
		num := func(k string) (float64, bool) {
			v := q.Get(k)
			if v == "" {
				return 0, false
			}
			n, err := strconv.ParseFloat(v, 64)
			return n, err == nil
		}
		if n, has := num("vote_count.gte"); has && float64(x.votes) < n {
			ok = false
		}
		if n, has := num("vote_count.lte"); has && float64(x.votes) > n {
			ok = false
		}
		if n, has := num("vote_average.gte"); has && x.rating < n {
			ok = false
		}
		if n, has := num("with_runtime.gte"); has && float64(x.runtime) < n {
			ok = false
		}
		if n, has := num("with_runtime.lte"); has && (x.runtime == 0 || float64(x.runtime) > n) {
			ok = false
		}
		if n, has := num("with_type"); has && float64(x.tvType) != n {
			ok = false
		}
		if v := q.Get("with_original_language"); v != "" && !strings.Contains("|"+v+"|", "|"+x.lang+"|") {
			ok = false
		}
		rel := x.released(f.now)
		for _, k := range []string{"primary_release_date.gte", "first_air_date.gte"} {
			if v := q.Get(k); v != "" && (rel == "" || rel < v) {
				ok = false
			}
		}
		for _, k := range []string{"primary_release_date.lte", "first_air_date.lte"} {
			if v := q.Get(k); v != "" && (rel == "" || rel > v) {
				ok = false
			}
		}
		if v := q.Get("certification.lte"); v != "" && (x.cert == "" || certRank[x.cert] > certRank[v]) {
			ok = false
		}
		if ok {
			xs = append(xs, x)
		}
	}
	sort.SliceStable(xs, func(i, j int) bool { return xs[i].pop > xs[j].pop })
	out := make([]map[string]any, 0, len(xs))
	for _, x := range xs {
		out = append(out, f.item(x))
	}
	return out
}

// env is a collection engine over a fixture catalogue and the fake TMDB.
type env struct {
	t    *testing.T
	ctx  context.Context
	tmdb *fakeTMDB
	db   interface{ Close() error }
	cat  *catalog.Catalog
	svc  *collections.Service
	cfg  config.Catalog
}

type envOpt func(*envOptions)

type envOptions struct {
	opts          []collections.Option
	noTMDB        bool
	catalogClient catalog.TMDB
}

func withCatalogClient(c catalog.TMDB) envOpt { return func(e *envOptions) { e.catalogClient = c } }

func withOpts(o ...collections.Option) envOpt {
	return func(e *envOptions) { e.opts = append(e.opts, o...) }
}

func newEnv(t *testing.T, eo ...envOpt) *env {
	t.Helper()
	var o envOptions
	for _, fn := range eo {
		fn(&o)
	}
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ft := newFakeTMDB(t)
	cfg := config.Catalog{Language: "en-US", Region: "US"}
	log := slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelWarn}))
	var client *tmdb.Client
	var catClient catalog.TMDB
	if !o.noTMDB {
		client = tmdb.New("test-key", log, tmdb.WithBaseURL(ft.srv.URL), tmdb.WithRateLimit(100000, 100000), tmdb.WithMaxRetries(0))
		t.Cleanup(client.Close)
		catClient = client
	}
	if o.catalogClient != nil {
		catClient = o.catalogClient
	}
	// Seed every fixture row at the fixture reference time so they share one
	// added_at (sort ties are part of the contract); the catalog follows the
	// wall clock again once seeding is done.
	var seeding atomic.Bool
	seeding.Store(true)
	clock := func() time.Time {
		if seeding.Load() {
			return ft.now
		}
		return time.Now()
	}
	cat, err := catalog.New(db, catClient, cfg, log, catalog.WithNow(clock))
	if err != nil {
		t.Fatal(err)
	}
	cat.SeedGenres("movie", movieGenres)
	cat.SeedGenres("tv", tvGenres)
	for _, x := range fixtures {
		if !x.inCatalog {
			continue
		}
		if err := cat.UpsertTitle(ctx, fixtureTitle(x, ft.now), true); err != nil {
			t.Fatal(err)
		}
	}
	seeding.Store(false)
	opts := []collections.Option{collections.WithIndexTTL(0), collections.WithLiveTTL(0), collections.WithoutDefaults(),
		collections.WithCatalogConfig(func() config.Catalog { return cfg })}
	if client != nil {
		opts = append(opts, collections.WithTMDB(client))
	}
	opts = append(opts, o.opts...)
	svc, err := collections.New(db, cat, log, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, ctx: ctx, tmdb: ft, db: db, cat: cat, svc: svc, cfg: cfg}
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimSpace(string(p)))
	return len(p), nil
}

func fixtureTitle(x fx, now time.Time) core.Title {
	t := core.Title{Key: x.key(), Type: core.TypeMovie, IDs: core.IDs{TMDB: x.id}, Title: x.title, Language: x.lang,
		VoteCount: x.votes, Rating: x.rating, Popularity: x.pop, ContentRating: x.cert,
		PosterPath: fmt.Sprintf("/p%d.jpg", x.id), BackdropPath: fmt.Sprintf("/b%d.jpg", x.id), Countries: x.countries}
	gm := movieGenres
	if x.tv {
		t.Type = core.TypeShow
		gm = tvGenres
	}
	for _, g := range x.genres {
		t.Genres = append(t.Genres, gm[g])
	}
	if rel := x.released(now); rel != "" {
		t.Released = tmdb.ParseDate(rel)
		t.Year = t.Released.Year()
	}
	if x.runtime > 0 {
		t.Runtime = time.Duration(x.runtime) * time.Minute
	}
	for _, k := range x.keywords {
		t.Keywords = append(t.Keywords, keywordNames[k])
	}
	for _, c := range x.companies {
		t.Studios = append(t.Studios, companyNames[c])
	}
	return t
}

// spec builds a minimal valid spec.
func spec(name, media string, sources []core.CollectionSource, rules ...core.CollectionRule) core.CollectionSpec {
	return core.CollectionSpec{Name: name, MediaType: media, Sources: sources, Match: "all", Rules: rules,
		Sort: "popularity", SortDesc: true, Refresh: "daily", Enabled: true,
		Targets: core.CollectionTargets{CollectionsTab: true, Users: core.AllUsers()}}
}

func rl(field, op string, v any) core.CollectionRule {
	b, _ := json.Marshal(v)
	return core.CollectionRule{Field: field, Op: op, Value: b}
}

func catalogSource() []core.CollectionSource { return []core.CollectionSource{{Kind: "catalog"}} }

func titlesOf(p *core.CollectionPreview) []string {
	out := make([]string, len(p.Items))
	for i, it := range p.Items {
		out[i] = it.Title
	}
	return out
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
