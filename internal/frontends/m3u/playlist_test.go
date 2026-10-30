package m3u

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
)

func TestPlaylistEscapesChannelMetadataAndRejectsUnsafeURLs(t *testing.T) {
	var out bytes.Buffer
	err := writePlaylist(&out, []playlistEntry{{ID: "channel\"\n#EXTINF:bad", Name: "News\r\nInjected", StreamURL: "https://ether.example/m3u/scoped"}})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 3 || lines[2] != "https://ether.example/m3u/scoped" || strings.Contains(lines[1], "\r") {
		t.Fatalf("unexpected playlist: %q", out.String())
	}
	for _, bad := range []string{"https://ether.example/a\nhttp://evil.example", "https://user:pass@ether.example/a", "file:///tmp/media", "/relative"} {
		out.Reset()
		if err := writePlaylist(&out, []playlistEntry{{StreamURL: bad}}); err == nil {
			t.Fatalf("accepted unsafe URL %q", bad)
		}
		if out.Len() != 0 {
			t.Fatalf("wrote partial playlist for %q", bad)
		}
	}
}

func TestPlaylistContainsOnlyViewerChannelsAndStableSurfaceURLs(t *testing.T) {
	host := frontend.Host{
		Identity: frontendtest.Identity{ResolveFunc: func(_ context.Context, credential string) (*frontend.Viewer, error) {
			if credential != "valid" {
				return nil, errors.New("bad grant")
			}
			return &frontend.Viewer{ID: 7}, nil
		}},
		Sessions: frontendtest.Sessions{},
		LiveTV: frontendtest.LiveTV{ChannelsFunc: func(_ context.Context, viewer *frontend.Viewer) ([]frontend.LiveChannel, error) {
			if viewer.ID != 7 {
				t.Fatal("wrong viewer")
			}
			return []frontend.LiveChannel{{ID: "live & one", Name: "News"}}, nil
		}},
		SurfaceURL: func(context.Context, *http.Request) (string, error) { return "https://ether.example/m3u", nil },
	}
	inst, err := (Frontend{}).Mount(context.Background(), host)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		credential string
		status     int
	}{{"valid", 200}, {"invalid", 401}} {
		w := httptest.NewRecorder()
		inst.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/m3u/"+tc.credential+"/playlist.m3u", nil))
		if w.Code != tc.status {
			t.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
		if tc.status == 200 && (!strings.Contains(w.Body.String(), `tvg-id="live & one"`) || !strings.Contains(w.Body.String(), "https://ether.example/m3u/valid/channel?id=live+%26+one")) {
			t.Fatalf("playlist %q", w.Body.String())
		}
	}
}
