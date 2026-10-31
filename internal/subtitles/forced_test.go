package subtitles

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

type forcedCand struct {
	id        int64
	downloads int
	hi        bool
	foreign   bool
}

// forcedFake serves configurable hearing_impaired / foreign_parts_only
// candidates; downloaded file ids are recorded.
type forcedFake struct {
	mu         sync.Mutex
	downloaded []int64
	srv        *httptest.Server
}

func newForcedFake(t *testing.T, cands []forcedCand) *forcedFake {
	t.Helper()
	f := &forcedFake{}
	mux := http.NewServeMux()
	mux.HandleFunc("/subtitles", func(w http.ResponseWriter, r *http.Request) {
		lang := r.URL.Query().Get("languages")
		data := []any{}
		for _, c := range cands {
			data = append(data, map[string]any{"attributes": map[string]any{
				"language": lang, "download_count": c.downloads,
				"release":          fmt.Sprintf("Movie.2024.%d", c.id),
				"hearing_impaired": c.hi, "foreign_parts_only": c.foreign,
				"files": []any{map[string]any{"file_id": c.id, "file_name": fmt.Sprintf("f%d.srt", c.id)}},
			}})
		}
		json.NewEncoder(w).Encode(map[string]any{"data": data})
	})
	mux.HandleFunc("/download", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			FileID int64 `json:"file_id"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.downloaded = append(f.downloaded, body.FileID)
		f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"link": f.srv.URL + fmt.Sprintf("/file/%d", body.FileID)})
	})
	mux.HandleFunc("/file/", func(w http.ResponseWriter, r *http.Request) {
		var id int64
		fmt.Sscanf(strings.TrimPrefix(r.URL.Path, "/file/"), "%d", &id)
		fmt.Fprintf(w, "1\n00:00:01,000 --> 00:00:02,000\ntrack-%d\n\n", id)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *forcedFake) downloads() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64{}, f.downloaded...)
}

func forcedTitle(key int64) *core.Title {
	return &core.Title{Key: core.MovieKey(key), Type: core.TypeMovie, IDs: core.IDs{IMDB: "tt0000123", TMDB: key}}
}

func openText(t *testing.T, svc *Service, st core.StreamInfo) string {
	t.Helper()
	rc, _, err := svc.Open(st.ExternalURL)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	return string(b)
}

// Mutation: HI+foreign_parts must NOT parse as forced; real forced must.
func TestForcedDefinitionHIPlusForeignIsNotForced(t *testing.T) {
	ctx := context.Background()
	f := newForcedFake(t, []forcedCand{
		{id: 21, downloads: 9000},                          // full
		{id: 22, downloads: 5000, hi: true, foreign: true}, // HI + foreign: SDH, not forced
		{id: 23, downloads: 100, foreign: true},            // real forced
	})
	svc := New(Options{Config: config.OpenSubtitles{Enabled: true, APIKey: "k"}, CacheDir: t.TempDir(), BaseURL: f.srv.URL})
	cands, err := svc.Search(ctx, forcedTitle(42), "en", "")
	if err != nil || len(cands) != 3 {
		t.Fatalf("search: %+v %v", cands, err)
	}
	byID := map[int64]Candidate{}
	for _, c := range cands {
		byID[c.FileID] = c
	}
	if byID[22].Forced || !byID[22].HearingImp {
		t.Fatalf("HI+foreign must be HearingImp, not Forced: %+v", byID[22])
	}
	if !byID[23].Forced || byID[23].HearingImp {
		t.Fatalf("foreign non-HI must be Forced: %+v", byID[23])
	}
	if byID[21].Forced || byID[21].HearingImp {
		t.Fatalf("full track: %+v", byID[21])
	}
}

// Preferences used to download a matching subtitle by themselves. They
// list only what is already cached.
func TestPreferencesDoNotDownload(t *testing.T) {
	ctx := context.Background()
	f := newForcedFake(t, []forcedCand{
		{id: 21, downloads: 9000},
		{id: 22, downloads: 5000, hi: true, foreign: true},
		{id: 23, downloads: 100, foreign: true},
		{id: 24, downloads: 50, hi: true},
	})
	svc := New(Options{Config: config.OpenSubtitles{Enabled: true, APIKey: "k"}, CacheDir: t.TempDir(), BaseURL: f.srv.URL})
	user := &core.User{SubtitleLanguages: []string{"en"}}
	title := forcedTitle(43)
	for _, prefs := range []Preferences{{}, {Mode: "forced-only"}, {Mode: "always", Style: "sdh"}, {Mode: "always", Style: "normal"}} {
		out, err := svc.SidecarsForRelease(ctx, title, user, nil, "Movie.2024.1080p.WEB-DL", prefs)
		if err != nil || len(out) != 0 {
			t.Fatalf("%+v: %+v %v", prefs, out, err)
		}
	}
	if d := f.downloads(); len(d) != 0 {
		t.Fatalf("preferences downloaded %v", d)
	}
	if _, err := svc.SearchOnDemand(ctx, title, "en", ""); err != nil {
		t.Fatal(err)
	}
	st, err := svc.DownloadOnDemand(ctx, title, 23)
	if err != nil || !st.Forced {
		t.Fatalf("explicit forced pick: %+v %v", st, err)
	}
	if got := openText(t, svc, st); !strings.Contains(got, "track-23") {
		t.Fatalf("pick body: %q", got)
	}
	if d := f.downloads(); len(d) != 1 || d[0] != 23 {
		t.Fatalf("downloads = %v, want [23]", d)
	}
}
