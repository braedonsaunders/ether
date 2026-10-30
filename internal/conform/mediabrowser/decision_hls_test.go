//go:build conformlab

package mediabrowser

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestDecisionHLSCapturesBoundedSegment(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/master.m3u8":
			_, _ = w.Write([]byte("#EXTM3U\nsegment.ts\n"))
		case "/segment.ts":
			_, _ = w.Write([]byte("test-segment"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer s.Close()
	p := filepath.Join(t.TempDir(), "segment")
	t.Setenv("SCORECARD_SEGMENT_EVIDENCE", p)
	target := decisionLabServer{base: s.URL, client: s.Client()}
	if err := target.readHLS("/master.m3u8"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(p)
	if err != nil || string(got) != "test-segment" {
		t.Fatalf("segment capture %q: %v", got, err)
	}
}
