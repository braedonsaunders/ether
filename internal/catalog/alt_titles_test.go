package catalog_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

// altTMDB serves one movie and one show whose details carry alternative
// titles, as TMDB returns them under append_to_response.
func altTMDB(t *testing.T, queries *[]string) *httptest.Server {
	t.Helper()
	var alts []string
	for i := 0; i < 40; i++ {
		alts = append(alts, fmt.Sprintf(`{"iso_3166_1":"US","title":"Working Title %d","type":""}`, i))
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if queries != nil {
			*queries = append(*queries, r.URL.Path+"?"+r.URL.RawQuery)
		}
		switch r.URL.Path {
		case "/genre/movie/list", "/genre/tv/list":
			fmt.Fprint(w, `{"genres":[{"id":16,"name":"Animation"}]}`)
		case "/movie/7001":
			fmt.Fprint(w, `{"id":7001,"title":"The Grand Voyage","original_title":"Le Grand Voyage","release_date":"2001-04-01","runtime":100,"status":"Released","vote_count":900,"popularity":30,"original_language":"fr",
"alternative_titles":{"titles":[{"iso_3166_1":"FR","title":"Le Grand Voyage","type":""},{"iso_3166_1":"US","title":"The Long Way","type":""},{"iso_3166_1":"US","title":"Grand Voyage","type":""},{"iso_3166_1":"JP","title":"グランド・ボヤージュ","type":""},{"iso_3166_1":"GB","title":"the long way","type":""},`+strings.Join(alts, ",")+`]}}`)
		case "/tv/7002":
			fmt.Fprint(w, `{"id":7002,"name":"Frieren: Beyond Journey's End","original_name":"葬送のフリーレン","first_air_date":"2023-09-29","status":"Returning Series","episode_run_time":[24],"number_of_seasons":1,"vote_count":900,"popularity":50,"original_language":"ja","genres":[{"id":16,"name":"Animation"}],
"seasons":[{"id":1,"name":"Season 1","season_number":1,"episode_count":1,"air_date":"2023-09-29"}],
"alternative_titles":{"results":[{"iso_3166_1":"JP","title":"Sousou no Frieren","type":"Romaji"},{"iso_3166_1":"US","title":"Frieren","type":""}]}}`)
		case "/tv/7002/season/1":
			fmt.Fprint(w, `{"id":1,"name":"Season 1","season_number":1,"air_date":"2023-09-29","episodes":[{"id":9,"name":"The Journey's End","episode_number":1,"season_number":1,"air_date":"2023-09-29","runtime":24}]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"status_message":"not found"}`)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// Alternative titles arrive with the ordinary detail fetch, are kept in
// Latin script, deduplicated and bounded, survive a reload from storage,
// and reach a show's episodes for release matching.
func TestAlternativeTitlesFromDetailFetch(t *testing.T) {
	var queries []string
	c := newTestCatalog(t, altTMDB(t, &queries))
	ctx := context.Background()
	movie, err := c.Get(ctx, core.MovieKey(7001))
	if err != nil {
		t.Fatal(err)
	}
	alts := movie.AlternativeTitles
	if len(alts) != 24 {
		t.Fatalf("kept %d alternative titles, want the 24 bound: %v", len(alts), alts)
	}
	if alts[0] != "The Long Way" || slices.Contains(alts, "the long way") || slices.Contains(alts, "Grand Voyage") || slices.Contains(alts, "Le Grand Voyage") || slices.Contains(alts, "グランド・ボヤージュ") {
		t.Fatalf("alternative titles %v: want The Long Way once, nothing equal to the title or original, no non-Latin script", alts)
	}
	for _, q := range queries {
		if strings.HasPrefix(q, "/movie/7001?") && !strings.Contains(q, "alternative_titles") {
			t.Fatalf("detail fetch without alternative titles: %s", q)
		}
		if strings.Contains(q, "/alternative_titles") {
			t.Fatalf("separate alternative-titles request: %s", q)
		}
	}
	ep, err := c.Get(ctx, core.EpisodeKey(7002, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(ep.ShowAlternativeTitles, "Sousou no Frieren") {
		t.Fatalf("episode show alternative titles %v", ep.ShowAlternativeTitles)
	}
	if len(ep.AlternativeTitles) != 0 {
		t.Fatalf("episode has its own alternative titles %v", ep.AlternativeTitles)
	}
}
