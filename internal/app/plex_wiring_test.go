package app

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/plextv"
)

// TestPlexRootAdvertisesTranscoderThroughAppWiring is the regression guard for
// #432. Production mounts the Plex server with NewServer(Deps{Host: host}), so
// the transcoder is only reachable through Host.Sessions; a handler that read
// Deps.Transcoder directly left `/` advertising transcoderVideo=0 and Plex Web
// disabled transcoding. This drives the real app wiring end to end: no Host
// fakes, the mounted registry, real auth, and the real frontend lifetime.
func TestPlexRootAdvertisesTranscoderThroughAppWiring(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cfg, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Update(func(c *config.Config) error {
		c.SetupComplete = true
		c.Transcode.FFmpegPath = "/not-installed/ffmpeg"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	a, err := New(ctx, Options{DataDir: dir, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), FixtureTranscoder: &operabilityTranscoder{max: 1}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	if err := a.users.SetOwner(ctx, plextv.Account{ID: 42, Username: "owner"}, "owner-token"); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(a.Handler())
	t.Cleanup(server.Close)
	a.frontends.start(ctx, server.Listener.Addr())
	t.Cleanup(a.frontends.stop)

	get := func(path string, json bool) string {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, server.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Plex-Token", "owner-token")
		req.Header.Set("X-Plex-Client-Identifier", "wiring-test")
		if json {
			req.Header.Set("Accept", "application/json")
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d: %s", path, res.StatusCode, body)
		}
		return string(body)
	}

	for _, path := range []string{"/", "/media/providers"} {
		if body := get(path, false); !strings.Contains(body, `transcoderVideo="1"`) {
			t.Errorf("%s advertised no transcoder through app wiring: %s", path, body)
		}
	}
	// The section listing and identity must also come from the real wiring.
	if body := get("/identity", false); !strings.Contains(body, `machineIdentifier=`) {
		t.Errorf("/identity missing machineIdentifier: %s", body)
	}
	if body := get("/library/sections", false); !strings.Contains(body, `type="movie" title="Movies"`) {
		t.Errorf("/library/sections missing the Movies section: %s", body)
	}
}
