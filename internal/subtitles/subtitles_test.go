package subtitles

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

func TestEncodingConversion(t *testing.T) {
	// CP1252: é = 0xE9, “ ” = 0x93 0x94.
	cp := []byte("1\r\n00:00:01,000 --> 00:00:02,000\r\nCaf\xe9 \x93oui\x94\r\n\r\n")
	srt, vtt, err := Normalize(cp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(srt, "Café “oui”") || !strings.Contains(vtt, "WEBVTT") || !strings.Contains(vtt, "00:00:01.000") {
		t.Fatalf("srt=%q vtt=%q", srt, vtt)
	}

	// UTF-16LE with BOM.
	text := "1\n00:00:01,000 --> 00:00:03,500\nGrüße!\n\n"
	u16 := utf16.Encode([]rune(text))
	raw := []byte{0xFF, 0xFE}
	for _, u := range u16 {
		raw = append(raw, byte(u), byte(u>>8))
	}
	srt, _, err = Normalize(raw)
	if err != nil || !strings.Contains(srt, "Grüße!") {
		t.Fatalf("utf16: %q err=%v", srt, err)
	}

	// VTT input -> SRT.
	srt, _, err = Normalize([]byte("WEBVTT\n\n00:01.000 --> 00:04.000\nHello\n"))
	if err != nil || !strings.Contains(srt, "00:00:01,000 --> 00:00:04,000") {
		t.Fatalf("vtt->srt: %q err=%v", srt, err)
	}
	if _, _, err := Normalize([]byte("not subtitles")); err == nil {
		t.Fatal("garbage should error")
	}
}

func fakeOpenSubtitles(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/subtitles", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Api-Key") != "testkey" {
			http.Error(w, "no key", 401)
			return
		}
		q := r.URL.Query()
		if q.Get("imdb_id") != "123" && q.Get("parent_imdb_id") != "555" {
			http.Error(w, fmt.Sprintf("bad query %v", q), 400)
			return
		}
		lang := q.Get("languages")
		json.NewEncoder(w).Encode(map[string]any{"data": []any{
			map[string]any{"attributes": map[string]any{
				"language": lang, "download_count": 10, "release": "Other.Release.720p",
				"uploader": map[string]any{"name": "anon", "rank": "Bronze member"},
				"files":    []any{map[string]any{"file_id": 1, "file_name": "a.srt"}},
			}},
			map[string]any{"attributes": map[string]any{
				"language": lang, "download_count": 5000, "ratings": 9.0, "release": "My.Movie.2024.1080p.WEB-DL-GRP",
				"uploader": map[string]any{"name": "pro", "rank": "Trusted"},
				"files":    []any{map[string]any{"file_id": 2, "file_name": "b.srt"}},
			}},
		}})
	})
	mux.HandleFunc("/download", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			FileID int64 `json:"file_id"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		json.NewEncoder(w).Encode(map[string]any{"link": srv.URL + fmt.Sprintf("/file/%d", body.FileID), "file_name": "x.srt"})
	})
	mux.HandleFunc("/file/", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "1\n00:00:01,000 --> 00:00:02,000\nBonjour\n\n")
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestSidecarsMissingOnly(t *testing.T) {
	srv := fakeOpenSubtitles(t)
	svc := New(Options{
		Config:   config.OpenSubtitles{Enabled: true, APIKey: "testkey"},
		CacheDir: t.TempDir(), BaseURL: srv.URL,
	})
	title := &core.Title{Key: core.MovieKey(42), Type: core.TypeMovie, IDs: core.IDs{IMDB: "tt0000123", TMDB: 42}, Title: "My Movie"}
	user := &core.User{ID: 1, SubtitleLanguages: []string{"en", "fr"}}
	embedded := []core.StreamInfo{{Index: 0, Type: "video", Codec: "h264"}, {Index: 1, Type: "subtitle", Language: "eng"}}

	out, err := svc.SidecarsForRelease(context.Background(), title, user, embedded, "My.Movie.2024.1080p.WEB-DL-GRP")
	if err != nil || len(out) != 0 {
		t.Fatalf("preferred languages must not download: %+v %v", out, err)
	}
	if _, err := svc.SearchOnDemand(context.Background(), title, "fr", "My.Movie.2024.1080p.WEB-DL-GRP"); err != nil {
		t.Fatal(err)
	}
	out = []core.StreamInfo{}
	picked, err := svc.DownloadOnDemand(context.Background(), title, 2)
	if err != nil {
		t.Fatal(err)
	}
	out = append(out, picked)
	if picked.Language != "fr" || !picked.External || !strings.HasPrefix(picked.ExternalURL, URLPrefix) {
		t.Fatalf("pick: %+v", picked)
	}
	if !strings.Contains(picked.Title, "1080p") {
		t.Fatalf("picked release %q", picked.Title)
	}
	rc, format, err := svc.Open(out[0].ExternalURL)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	if format != "srt" || !strings.Contains(string(b), "Bonjour") {
		t.Fatalf("open: format=%s body=%q", format, b)
	}
	vttRC, format, err := svc.OpenFormat(out[0].ExternalURL, "vtt")
	if err != nil {
		t.Fatal(err)
	}
	defer vttRC.Close()
	b, _ = io.ReadAll(vttRC)
	if format != "vtt" || !strings.HasPrefix(string(b), "WEBVTT") {
		t.Fatalf("vtt: %q", b)
	}
	if _, _, err := svc.Open("../etc/passwd"); err == nil {
		t.Fatal("path traversal must fail")
	}

	// Episode search params: parent ids + season/episode.
	ep := &core.Title{Key: core.EpisodeKey(9, 2, 3), Type: core.TypeEpisode, ShowIDs: core.IDs{IMDB: "tt555", TMDB: 9}, Season: 2, Episode: 3}
	q := searchQuery(ep, "en")
	if q.Get("parent_imdb_id") != "555" || q.Get("season_number") != "2" || q.Get("episode_number") != "3" {
		t.Fatalf("episode query: %v", q)
	}

	// Disabled service: no sidecars, no error.
	svc.cfg.Enabled = false
	out, err = svc.Sidecars(context.Background(), title, user, nil)
	if err != nil || out != nil {
		t.Fatalf("disabled: %v %v", out, err)
	}
}

func TestRankPenalties(t *testing.T) {
	cands := []Candidate{
		{FileID: 1, DownloadCount: 100000, AITranslated: true},
		{FileID: 2, DownloadCount: 100, UploaderRank: "Trusted"},
	}
	Rank(cands, "")
	// Log-scaled downloads: 50 vs 20, -20 AI penalty vs +15 trusted => trusted wins.
	if cands[0].FileID != 2 {
		t.Fatalf("rank: %+v", cands)
	}
}
