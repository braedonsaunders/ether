package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/plextv"
)

// TestSpikePlexMetadataUsesSpikeCatalog reproduces the in-app spike wiring:
// buildPlex serves the spike (local-file) catalog, so a KindLocal rating key
// obtained from the section listing must open on /library/metadata/<key>.
// Regression: buildPlex unconditionally wired the TMDB-backed episode-order
// service over the spike catalog, and metadataTitle prefers EpisodeOrder, so
// the real catalog answered ErrNotFound for KindLocal and Plex Web showed
// "Content is unavailable" (404) while hubs/play queues — which read the
// spike catalog directly — worked.
func TestSpikePlexMetadataUsesSpikeCatalog(t *testing.T) {
	dir := t.TempDir()
	cfg, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Update(func(c *config.Config) error {
		c.Transcode.FFmpegPath = "/not-installed/ffmpeg"
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	mediaDir := t.TempDir()
	file := filepath.Join(mediaDir, "Big.Buck.Bunny.2008.720p.mp4")
	if err := os.WriteFile(file, []byte(strings.Repeat("Big Buck Bunny ", 500)), 0o644); err != nil {
		t.Fatal(err)
	}

	a, err := New(context.Background(), Options{
		DataDir:    dir,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Spike:      true,
		SpikeFiles: []string{file},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	// Register the owner token directly on the auth service built by New
	// (no plex.tv token in config, so startup made no network call).
	if err := a.users.SetOwner(context.Background(), plextv.Account{ID: 42, Username: "owner"}, "owner-token"); err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(a.Handler())
	defer ts.Close()
	do := func(method, path string) (int, map[string]any) {
		t.Helper()
		req, err := http.NewRequest(method, ts.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Plex-Token", "owner-token")
		req.Header.Set("X-Plex-Client-Identifier", "spike-test")
		req.Header.Set("Accept", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			return resp.StatusCode, nil
		}
		var v struct {
			MediaContainer map[string]any `json:"MediaContainer"`
		}
		if err := json.Unmarshal(body, &v); err != nil {
			t.Fatalf("%s %s: decode: %v (%s)", method, path, err, body)
		}
		return resp.StatusCode, v.MediaContainer
	}

	code, all := do(http.MethodGet, "/library/sections/1/all")
	if code != http.StatusOK {
		t.Fatalf("section listing: status %d", code)
	}
	metadata, ok := all["Metadata"].([]any)
	if !ok || len(metadata) == 0 {
		t.Fatalf("section listing has no items: %v", all)
	}
	key, ok := metadata[0].(map[string]any)["ratingKey"].(string)
	if !ok || key == "" {
		t.Fatalf("section listing item has no ratingKey: %v", metadata[0])
	}

	if code, _ := do(http.MethodGet, "/library/metadata/"+key); code != http.StatusOK {
		t.Fatalf("GET /library/metadata/%s: status %d, want 200 (spike catalog key must not be routed to the TMDB catalog)", key, code)
	}

	// Contrast: play queues always read the spike catalog directly and
	// must keep working for the same key.
	uri := url.QueryEscape(fmt.Sprintf("server://spike/com.plexapp.plugins.library/library/metadata/%s", key))
	if code, _ := do(http.MethodPost, "/playQueues?type=video&uri="+uri); code != http.StatusOK {
		t.Fatalf("POST /playQueues for %s: status %d, want 200", key, code)
	}

	item := metadata[0].(map[string]any)
	media, _ := item["Media"].([]any)
	if len(media) == 0 {
		t.Fatal("spike item has no media")
	}
	parts, _ := media[0].(map[string]any)["Part"].([]any)
	if len(parts) == 0 {
		t.Fatal("spike item has no part")
	}
	part, _ := parts[0].(map[string]any)["key"].(string)
	if part == "" {
		t.Fatal("spike part has no key")
	}
	req, err := http.NewRequest(http.MethodGet, ts.URL+part, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Plex-Token", "owner-token")
	req.Header.Set("X-Plex-Client-Identifier", "spike-test")
	req.Header.Set("X-Plex-Product", "Plex Web")
	req.Header.Set("Range", "bytes=0-31")
	started := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("GET %s immediately after ready: status %d, body %s", part, resp.StatusCode, body)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("play immediately after ready took %s", elapsed)
	} else {
		t.Logf("play immediately after ready took %s", elapsed)
	}
	if len(body) == 0 {
		t.Fatal("play returned no bytes")
	}
}
