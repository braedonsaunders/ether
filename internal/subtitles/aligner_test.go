package subtitles

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

// fakeAlignSubtitles serves two releases of one movie: file 1 timed for
// the WEB-DL, file 2 the same lines 12s later (longer Blu-ray logo).
func fakeAlignSubtitles(t *testing.T, searches, downloads *atomic.Int32) *httptest.Server {
	t.Helper()
	base := syntheticDialogue(time.Minute, 60)
	shifted := remapCues(base, func(d time.Duration) time.Duration { return d + 12*time.Second })
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/subtitles", func(w http.ResponseWriter, r *http.Request) {
		searches.Add(1)
		json.NewEncoder(w).Encode(map[string]any{"data": []any{
			map[string]any{"attributes": map[string]any{
				"language": "en", "download_count": 900, "release": "My.Movie.2024.1080p.WEB-DL-GRP",
				"uploader": map[string]any{"name": "a", "rank": "Trusted"},
				"files":    []any{map[string]any{"file_id": 1, "file_name": "web.srt"}},
			}},
			map[string]any{"attributes": map[string]any{
				"language": "en", "download_count": 800, "release": "My.Movie.2024.1080p.BluRay-GRP",
				"uploader": map[string]any{"name": "b", "rank": "Trusted"},
				"files":    []any{map[string]any{"file_id": 2, "file_name": "blu.srt"}},
			}},
		}})
	})
	mux.HandleFunc("/download", func(w http.ResponseWriter, r *http.Request) {
		downloads.Add(1)
		var body struct {
			FileID int64 `json:"file_id"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		json.NewEncoder(w).Encode(map[string]any{"link": srv.URL + fmt.Sprintf("/file/%d", body.FileID)})
	})
	mux.HandleFunc("/file/1", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, RenderSRT(base)) })
	mux.HandleFunc("/file/2", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, RenderSRT(shifted)) })
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func alignReq() core.AlignRequest {
	return core.AlignRequest{
		Key:          core.MovieKey(42),
		From:         core.Release{ID: "rel-web", Name: "My.Movie.2024.1080p.WEB-DL-GRP"},
		To:           core.Release{ID: "rel-blu", Name: "My.Movie.2024.1080p.BluRay-GRP"},
		FromDuration: 25 * time.Minute,
		ToDuration:   25*time.Minute + 12*time.Second,
	}
}

func newAlignSvc(dir, url string) *Service {
	return New(Options{
		Config:   config.OpenSubtitles{Enabled: true, APIKey: "testkey"},
		CacheDir: dir, BaseURL: url,
	})
}

func TestAlignerEndToEndAndCache(t *testing.T) {
	var searches, downloads atomic.Int32
	srv := fakeAlignSubtitles(t, &searches, &downloads)
	dir := t.TempDir()
	svc := newAlignSvc(dir, srv.URL)
	title := &core.Title{Key: core.MovieKey(42), Type: core.TypeMovie}
	cands, err := svc.Search(context.Background(), title, "en", "")
	if err != nil || len(cands) != 2 {
		t.Fatalf("search: %+v %v", cands, err)
	}
	for _, c := range cands {
		id := cacheID(title, "en", c.FileID)
		if _, err := svc.fetchAndCache(context.Background(), id, c, title.Key); err != nil {
			t.Fatal(err)
		}
		rel := c.Release
		svc.saveSelection(title, "en", rel, selection{ID: id, FileID: c.FileID, Language: "en", Release: rel, At: svc.now()})
	}
	if downloads.Load() != 2 {
		t.Fatalf("explicit downloads = %d", downloads.Load())
	}
	a := svc.TimelineAligner()

	got, ok := a.AlignTime(context.Background(), alignReq(), 10*time.Minute)
	if !ok || got < 10*time.Minute+11*time.Second || got > 10*time.Minute+13*time.Second {
		t.Fatalf("AlignTime = %v,%v", got, ok)
	}
	s1, d1 := searches.Load(), downloads.Load()
	if d1 != 2 {
		t.Fatalf("alignment downloaded: downloads %d", d1)
	}
	// Second call: memory cache, no network at all.
	if _, ok := a.AlignTime(context.Background(), alignReq(), 5*time.Minute); !ok {
		t.Fatal("cached AlignTime failed")
	}
	// Same-release identity never needs the network either.
	same := alignReq()
	same.To = same.From
	if got, ok := a.AlignTime(context.Background(), same, 7*time.Minute); !ok || got != 7*time.Minute {
		t.Fatalf("identity = %v,%v", got, ok)
	}
	if searches.Load() != s1 || downloads.Load() != d1 {
		t.Fatalf("cache miss on network: searches %d->%d downloads %d->%d", s1, searches.Load(), d1, downloads.Load())
	}
	// A fresh service over the same cache dir answers within the failover
	// deadline from the on-disk alignment, with no network.
	fast, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	got, ok = newAlignSvc(dir, srv.URL).TimelineAligner().AlignTime(fast, alignReq(), 10*time.Minute)
	if !ok || got < 10*time.Minute+11*time.Second || got > 10*time.Minute+13*time.Second {
		t.Fatalf("disk-cached fast AlignTime = %v,%v", got, ok)
	}
	if searches.Load() != s1 || downloads.Load() != d1 {
		t.Fatal("disk cache hit still used the network")
	}
}

func TestAlignerFastPathDoesNotDownload(t *testing.T) {
	var searches, downloads atomic.Int32
	srv := fakeAlignSubtitles(t, &searches, &downloads)
	a := newAlignSvc(t.TempDir(), srv.URL).TimelineAligner()

	fast, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	start := time.Now()
	if _, ok := a.AlignTime(fast, alignReq(), 10*time.Minute); ok {
		t.Fatal("cold cache should miss on the fast path")
	}
	cancel()
	if elapsed := time.Since(start); elapsed > 300*time.Millisecond {
		t.Fatalf("fast path blocked for %v", elapsed)
	}
	time.Sleep(200 * time.Millisecond)
	if _, ok := a.AlignTime(context.Background(), alignReq(), 10*time.Minute); ok {
		t.Fatal("background warm downloaded a subtitle and aligned")
	}
	if downloads.Load() != 0 {
		t.Fatalf("aligner downloads = %d, want 0", downloads.Load())
	}
}

func TestAlignerDisabledAndUnknownTitle(t *testing.T) {
	off := New(Options{CacheDir: t.TempDir()}).TimelineAligner()
	if _, ok := off.AlignTime(context.Background(), alignReq(), time.Minute); ok {
		t.Fatal("disabled aligner aligned")
	}
	// A title with no subtitles anywhere: search 404s -> ok=false, and the
	// negative result is cached (second call must not search again).
	var searches atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		searches.Add(1)
		json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
	}))
	t.Cleanup(srv.Close)
	a := newAlignSvc(t.TempDir(), srv.URL).TimelineAligner()
	for i := 0; i < 2; i++ {
		if _, ok := a.AlignTime(context.Background(), alignReq(), time.Minute); ok {
			t.Fatal("empty search aligned")
		}
	}
	if searches.Load() != 1 {
		t.Fatalf("negative result not cached: %d searches", searches.Load())
	}
}
