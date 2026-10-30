package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// tmdbFixture serves synthetic catalogue data. Unknown endpoints return 404 so
// missing fixture coverage cannot silently masquerade as a valid empty page.
type fixtureTracker struct {
	mu      sync.Mutex
	missing map[string]bool
}

func (f *fixtureTracker) record(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.missing == nil {
		f.missing = map[string]bool{}
	}
	f.missing[path] = true
}
func (f *fixtureTracker) failures() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.missing))
	for p := range f.missing {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
func tmdbFixture() http.Handler { return trackedTMDBFixture(nil) }
func trackedTMDBFixture(tracker *fixtureTracker) http.Handler {
	img := image.NewRGBA(image.Rect(0, 0, 320, 180))
	for y := 0; y < 180; y++ {
		for x := 0; x < 320; x++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), uint8(x + y), 255})
		}
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, img, &jpeg.Options{Quality: 90}); err != nil {
		panic(err)
	}
	movies := make([]map[string]any, 0, fixtureMovieCount())
	shows := make([]map[string]any, 0, 20)
	for i := 1; i <= fixtureMovieCount(); i++ {
		movies = append(movies, map[string]any{"id": 10000 + i, "title": fmt.Sprintf("Fixture Movie %03d", i), "release_date": "2020-01-01", "original_language": "en", "popularity": fixtureMoviePopularity(i), "vote_average": 7, "vote_count": 1000, "genre_ids": []int{18}, "genres": []map[string]any{{"id": 18, "name": "Drama"}}, "runtime": 90, "media_type": "movie"})
	}
	for i := range movies {
		movies[i]["poster_path"] = "/fixture.jpg"
		movies[i]["backdrop_path"] = "/fixture.jpg"
		rating := "PG"
		if (i+1)%3 == 0 {
			rating = "R"
		}
		movies[i]["release_dates"] = map[string]any{"results": []map[string]any{{"iso_3166_1": "US", "release_dates": []map[string]any{{"certification": rating, "release_date": "2020-01-01T00:00:00.000Z", "type": 4}}}}}
	}
	for i := 0; i < 12; i++ {
		movies[i]["belongs_to_collection"] = map[string]any{"id": 40001, "name": "Fixture Collection"}
	}
	for i := 1; i <= 20; i++ {
		shows = append(shows, map[string]any{"id": 20000 + i, "name": fmt.Sprintf("Fixture Show %03d", i), "first_air_date": "2020-01-01", "original_language": "en", "popularity": 100 - i, "vote_average": 7, "vote_count": 1000, "genre_ids": []int{18}, "genres": []map[string]any{{"id": 18, "name": "Drama"}}, "media_type": "tv", "number_of_seasons": 1, "number_of_episodes": 4, "seasons": []map[string]any{{"id": 30000 + i, "name": "Season 1", "season_number": 1, "episode_count": 4, "air_date": "2020-01-01"}}})
	}
	for i, show := range shows {
		show["poster_path"] = "/fixture.jpg"
		show["backdrop_path"] = "/fixture.jpg"
		rating := "TV-PG"
		if i == 0 {
			rating = "TV-MA"
		}
		show["content_ratings"] = map[string]any{"results": []map[string]any{{"iso_3166_1": "US", "rating": rating}}}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			if tracker != nil {
				tracker.record(r.Method + " " + r.URL.Path)
			}
			http.Error(w, "unsupported method", http.StatusMethodNotAllowed)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/3/images/") {
			parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/3/images/"), "/")
			if len(parts) == 2 && parts[1] == "fixture.jpg" && (parts[0] == "original" || parts[0] == "w92" || parts[0] == "w185" || parts[0] == "w300" || parts[0] == "w342" || parts[0] == "w500" || parts[0] == "w780" || parts[0] == "w1280") {
				w.Header().Set("Content-Type", "image/jpeg")
				_, _ = w.Write(encoded.Bytes())
				return
			}
			if tracker != nil {
				tracker.record(r.URL.Path)
			}
			http.NotFound(w, r)
			return
		}
		path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/3/"), "/")
		parts := strings.Split(path, "/")
		var result any
		switch {
		case path == "collection/40001":
			result = map[string]any{"id": 40001, "name": "Fixture Collection", "poster_path": "/fixture.jpg", "backdrop_path": "/fixture.jpg", "parts": movies[:12]}
		case path == "configuration":
			result = map[string]any{"images": map[string]any{"base_url": "http://127.0.0.1/fixture-images/", "secure_base_url": "http://127.0.0.1/fixture-images/"}}
		case path == "genre/movie/list" || path == "genre/tv/list":
			result = map[string]any{"genres": []map[string]any{{"id": 18, "name": "Drama"}}}
		case len(parts) == 7 && parts[0] == "tv" && parts[2] == "season" && parts[4] == "episode" && parts[6] == "external_ids":
			id, e := strconv.Atoi(parts[1])
			season, e2 := strconv.Atoi(parts[3])
			episode, e3 := strconv.Atoi(parts[5])
			if e == nil && e2 == nil && e3 == nil && id >= 20001 && id <= 20020 && season == 1 && episode >= 1 && episode <= 4 {
				result = map[string]any{"id": id*10 + episode}
			}
		case path == "search/multi":
			query := strings.ToLower(r.URL.Query().Get("query"))
			items := make([]map[string]any, 0, len(movies)+len(shows))
			for _, it := range append(append([]map[string]any{}, movies...), shows...) {
				name, _ := it["title"].(string)
				if name == "" {
					name, _ = it["name"].(string)
				}
				if query == "" || strings.Contains(strings.ToLower(name), query) {
					items = append(items, it)
				}
			}
			if page, _ := strconv.Atoi(r.URL.Query().Get("page")); page > 1 {
				items = nil
			}
			result = map[string]any{"page": 1, "total_pages": 1, "total_results": len(items), "results": items}
		case len(parts) == 2 && ((parts[0] == "movie" && (parts[1] == "now_playing" || parts[1] == "popular" || parts[1] == "top_rated" || parts[1] == "upcoming")) || (parts[0] == "tv" && (parts[1] == "airing_today" || parts[1] == "on_the_air" || parts[1] == "popular" || parts[1] == "top_rated"))):
			items := movies
			if parts[0] == "tv" {
				items = shows
			}
			if page, _ := strconv.Atoi(r.URL.Query().Get("page")); page > 1 {
				items = nil
			}
			result = map[string]any{"page": 1, "total_pages": 1, "total_results": len(items), "results": items}
		case len(parts) == 2 && (parts[0] == "movie" || parts[0] == "tv"):
			id, err := strconv.Atoi(parts[1])
			if err != nil {
				break
			}
			if parts[0] == "movie" && id >= 10001 && id <= 10000+fixtureMovieCount() {
				result = movies[id-10001]
			}
			if parts[0] == "tv" && id >= 20001 && id <= 20020 {
				result = shows[id-20001]
			}
		case len(parts) == 4 && parts[0] == "tv" && parts[2] == "season" && parts[3] == "1":
			id, err := strconv.Atoi(parts[1])
			if err != nil || id < 20001 || id > 20020 {
				break
			}
			eps := make([]map[string]any, 0, 4)
			for i := 1; i <= 4; i++ {
				eps = append(eps, map[string]any{"id": id*10 + i, "name": fmt.Sprintf("Episode %d", i), "show_id": id, "season_number": 1, "episode_number": i, "air_date": "2020-01-01", "runtime": 45})
			}
			result = map[string]any{"id": id + 10000, "name": "Season 1", "season_number": 1, "air_date": "2020-01-01", "episodes": eps}
		default:
			kind := ""
			if strings.Contains(path, "movie") {
				kind = "movie"
			}
			if strings.Contains(path, "tv") {
				kind = "tv"
			}
			allowed := path == "discover/movie" || path == "discover/tv" || path == "search/movie" || path == "search/tv" || path == "trending/movie/day" || path == "trending/movie/week" || path == "trending/tv/day" || path == "trending/tv/week"
			if len(parts) == 3 && (parts[2] == "similar" || parts[2] == "recommendations") {
				id, err := strconv.Atoi(parts[1])
				allowed = err == nil && ((parts[0] == "movie" && id >= 10001 && id <= 10000+fixtureMovieCount()) || (parts[0] == "tv" && id >= 20001 && id <= 20020))
			}
			if allowed && kind != "" {
				var items []map[string]any
				if kind == "movie" {
					items = movies
				} else {
					items = shows
				}
				q := strings.ToLower(r.URL.Query().Get("query"))
				filtered := make([]map[string]any, 0, len(items))
				if page, _ := strconv.Atoi(r.URL.Query().Get("page")); page <= 1 {
					for _, it := range items {
						name, _ := it["title"].(string)
						if name == "" {
							name, _ = it["name"].(string)
						}
						if q == "" || strings.Contains(strings.ToLower(name), q) {
							filtered = append(filtered, it)
						}
					}
				}
				result = map[string]any{"page": 1, "total_pages": 1, "total_results": len(filtered), "results": filtered}
			}
		}
		if result == nil {
			if tracker != nil {
				tracker.record(r.URL.Path)
			}
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(result); err != nil {
			return
		}
	})
}
