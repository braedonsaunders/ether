package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// subtitleFixture is a deliberately small offline OpenSubtitles source. Any
// unrecognized request is recorded as a hard fixture failure.
func subtitleFixture(tracker *fixtureTracker) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fail := func() {
			tracker.record(r.Method + " " + r.URL.RequestURI())
			http.Error(w, "missing subtitle fixture", http.StatusNotFound)
		}
		if !strings.HasPrefix(r.URL.Path, "/file/") && r.Header.Get("Api-Key") != "parity-fixture" {
			fail()
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/subtitles":
			id, err := strconv.Atoi(r.URL.Query().Get("tmdb_id"))
			lang := r.URL.Query().Get("languages")
			if err != nil || id < 10001 || id > 10000+fixtureMovieCount() || lang == "" {
				fail()
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"attributes": map[string]any{"language": lang, "download_count": 10, "release": "Fixture Movie", "files": []any{map[string]any{"file_id": id, "file_name": "fixture.srt"}}}}}})
		case r.Method == http.MethodPost && r.URL.Path == "/download":
			var v struct {
				FileID int `json:"file_id"`
			}
			if json.NewDecoder(r.Body).Decode(&v) != nil || v.FileID < 10001 || v.FileID > 10000+fixtureMovieCount() {
				fail()
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"link": fmt.Sprintf("http://%s/file/%d", r.Host, v.FileID), "file_name": "fixture.srt"})
		default:
			if r.Method == http.MethodGet && len(r.URL.Path) > 6 && r.URL.Path[:6] == "/file/" {
				id, err := strconv.Atoi(r.URL.Path[6:])
				if err == nil && id >= 10001 && id <= 10000+fixtureMovieCount() {
					w.Header().Set("Content-Type", "application/x-subrip")
					fmt.Fprint(w, "1\n00:00:01,000 --> 00:00:03,000\nFixture subtitle\n\n")
					return
				}
			}
			fail()
		}
	})
}
