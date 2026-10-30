package app

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/livetv"
	"github.com/braedonsaunders/ether/internal/store"
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

func TestLiveAdapterRewritesNestedResourcesToLazyCapability(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/master":
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1000\n/variant?secret=source-token\n")
		case "/variant":
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			_, _ = io.WriteString(w, "#EXTM3U\n#EXTINF:4,\n/segment?secret=source-token\n")
		case "/segment":
			_, _ = io.WriteString(w, "media-bytes")
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	db := store.OpenTest(t)
	service, err := livetv.New(livetv.Options{DB: db, HTTPClient: upstream.Client()})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	channel, err := service.SaveChannel(context.Background(), livetv.Channel{Name: "Fixture", Number: "1", Kind: "iptv", Enabled: true, StreamURL: upstream.URL + "/master?secret=source-token"})
	if err != nil {
		t.Fatal(err)
	}
	viewer := &core.Viewer{ID: 1}
	info, err := service.Tune(context.Background(), viewer, channel.ID, core.ClientCaps{ClientID: "test"})
	if err != nil {
		t.Fatal(err)
	}
	adapter := sessionLive{service: service}
	read := func(tail, base string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/?X-Plex-Token=account-token", nil)
		r.Header.Set("X-Plex-Token", "account-token")
		adapter.Serve(w, r, viewer, info.ID, tail, base)
		return w
	}
	firstURI := func(body, prefix string) string {
		for _, line := range strings.Split(body, "\n") {
			if strings.HasPrefix(line, prefix) {
				return strings.TrimPrefix(line, prefix)
			}
		}
		t.Fatalf("missing %q URI in %q", prefix, body)
		return ""
	}
	master := read("index.m3u8", "./")
	if master.Code != 200 || strings.Contains(master.Body.String(), "source-token") || strings.Contains(master.Body.String(), "account-token") || strings.Contains(master.Body.String(), "/sessions/media/") {
		t.Fatalf("master %d %q", master.Code, master.Body.String())
	}
	variantKey := firstURI(master.Body.String(), "./resource/")
	variant := read("resource/"+variantKey, "../")
	if variant.Code != 200 || strings.Contains(variant.Body.String(), "source-token") || strings.Contains(variant.Body.String(), "account-token") {
		t.Fatalf("variant %d %q", variant.Code, variant.Body.String())
	}
	segmentKey := firstURI(variant.Body.String(), "../resource/")
	segment := read("resource/"+segmentKey, "../")
	if segment.Code != 200 || segment.Body.String() != "media-bytes" {
		t.Fatalf("segment %d %q", segment.Code, segment.Body.String())
	}
}
