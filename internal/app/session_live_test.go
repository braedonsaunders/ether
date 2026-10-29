package app

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLivePlaylistPathsUseScopedGrant(t *testing.T) {
	out := httptest.NewRecorder()
	w := &livePlaylistWriter{ResponseWriter: out, from: []byte("/livetv/sessions/internal/0/"), to: []byte("/sessions/media/id/grant/live/")}
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	_, _ = w.Write([]byte("#EXTM3U\n/livetv/sessions/internal/0/resource/"))
	_, _ = w.Write([]byte("abc\n"))
	w.finish()
	if got := out.Body.String(); !strings.Contains(got, "/sessions/media/id/grant/live/resource/abc") || strings.Contains(got, "/livetv/") {
		t.Fatalf("rewritten playlist %q", got)
	}
}
