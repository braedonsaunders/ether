package mediabrowser

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
)

func TestBitrateTestAuthenticatedBoundedBytes(t *testing.T) {
	identity := frontendtest.Identity{ResolveFunc: func(_ context.Context, token string) (*frontend.Viewer, error) {
		if token != "valid" {
			return nil, errors.New("denied")
		}
		return &frontend.Viewer{ID: 1}, nil
	}}
	for _, front := range []frontend.Frontend{Jellyfin{MachineID: "test"}, Emby{MachineID: "test"}} {
		inst, err := front.Mount(context.Background(), frontend.Host{Identity: identity})
		if err != nil {
			t.Fatal(err)
		}
		call := func(query, token string) *httptest.ResponseRecorder {
			r := httptest.NewRequest("GET", "/Playback/BitrateTest"+query, nil)
			if token != "" {
				r.Header.Set("X-Emby-Token", token)
			}
			w := httptest.NewRecorder()
			inst.Handler().ServeHTTP(w, r)
			return w
		}
		for _, tc := range []struct {
			q, token     string
			status, size int
		}{{"?size=5000000", "valid", 200, 5000000}, {"?size=1", "valid", 200, 1}, {"", "valid", 200, 102400}, {"?size=100000001", "valid", 400, 0}, {"?size=0", "valid", 400, 0}, {"?size=1&size=2", "valid", 400, 0}, {"?size=1", "bad", 401, 0}, {"?size=1", "", 401, 0}} {
			w := call(tc.q, tc.token)
			if w.Code != tc.status || (tc.status == 200 && (w.Body.Len() != tc.size || w.Header().Get("Content-Type") != "application/octet-stream")) {
				t.Fatal(tc, w.Code, w.Body.Len())
			}
		}
		w := call("?size=5000000", "valid")
		var compressed bytes.Buffer
		gz := gzip.NewWriter(&compressed)
		_, _ = io.Copy(gz, bytes.NewReader(w.Body.Bytes()))
		_ = gz.Close()
		if compressed.Len() < w.Body.Len()*98/100 {
			t.Fatal("bitrate data compresses materially", compressed.Len())
		}
	}
}
