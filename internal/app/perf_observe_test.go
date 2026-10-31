package app

import (
	"bufio"
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/store"
)

func TestRouteShapeHidesIdentifiersAndTokens(t *testing.T) {
	for path, want := range map[string]string{
		"/library/metadata/1000000000000603/children":       "/library/metadata/:id/children",
		"/stremio/eg1_c29tZWxvbmdzZWNyZXQ123/manifest.json": "/stremio/:id/manifest.json",
		"/Items/4e0f0a56014b4883889b7e9465f03462":           "/Items/:id",
		"/hubs/search": "/hubs/search",
	} {
		if got := routeShape(path); got != want {
			t.Errorf("routeShape(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestPerfObserverTracesAndLogsChattyRequests(t *testing.T) {
	trace := filepath.Join(t.TempDir(), "trace.jsonl")
	t.Setenv(PerfTraceEnv, trace)
	db, err := store.OpenPath(t.Context(), filepath.Join(t.TempDir(), "perf.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var logs bytes.Buffer
	p := newPerfObserver(slog.New(slog.NewTextHandler(&logs, nil)))
	defer store.CaptureCallSites(false)
	h := p.wrap("plex", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := 1
		if r.URL.Path == "/chatty" {
			n = perfChattyQueries + 1
		}
		for range n {
			var one int
			if err := db.QueryRowContext(r.Context(), `SELECT 1`).Scan(&one); err != nil {
				t.Error(err)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	for _, path := range []string{"/quiet", "/chatty"} {
		req := httptest.NewRequest(http.MethodGet, path+"?X-Plex-Token=secret", nil)
		req.Header.Set(PerfTagHeader, "tag"+path)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	p.close()

	f, err := os.Open(trace)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var recs []PerfTraceRecord
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var rec PerfTraceRecord
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatal(err)
		}
		recs = append(recs, rec)
	}
	if len(recs) != 2 || recs[0].Tag != "tag/quiet" || recs[0].Queries != 1 || recs[0].Rows != 1 || recs[0].Status != 200 || recs[1].Queries != perfChattyQueries+1 {
		t.Fatalf("trace = %+v", recs)
	}
	if len(recs[0].Statements) != 1 || !strings.Contains(recs[0].Statements[0].Site, "perf_observe_test.go") {
		t.Fatalf("statement breakdown = %+v", recs[0].Statements)
	}
	out := logs.String()
	if !strings.Contains(out, "request over performance budget") || !strings.Contains(out, "route=/chatty") || strings.Contains(out, "route=/quiet") {
		t.Fatalf("budget log = %s", out)
	}
	if strings.Contains(out, "secret") {
		t.Fatal("budget log carries the request's token")
	}
}
