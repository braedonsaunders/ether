//go:build plexparity && !race

package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/app"
	"github.com/braedonsaunders/ether/internal/config"
)

// This normal-run performance diagnostic measures the real handler deadline.
// Race correctness remains covered by the focused selection/authority tests;
// instrumentation overhead is not the production latency gate.
// Exercise the actual parity catalogue, child policy, recommender and Plex
// handler together, including repeated requests after restricted backfill.
func TestParityChildMoviePostplayPerformance(t *testing.T) {
	// Setup may warm every seeded viewer under -race. Reserve time for the
	// measured requests and cleanup, without shortening their own deadlines.
	setupBudget := 2 * time.Minute
	if deadline, ok := t.Deadline(); ok {
		setupBudget = time.Until(deadline) - 30*time.Second
	}
	if setupBudget <= 0 {
		t.Fatal("no fixture setup budget remaining")
	}
	ctx, cancel := context.WithTimeout(context.Background(), setupBudget)
	defer cancel()
	tmdb := httptest.NewServer(tmdbFixture())
	defer tmdb.Close()
	plexTV := httptest.NewServer(plextvFixture())
	defer plexTV.Close()
	t.Setenv("ETHER_PARITY_TMDB_URL", tmdb.URL+"/3")
	t.Setenv("ETHER_PARITY_PLEXTV_URL", plexTV.URL)
	dir := t.TempDir()
	if err := seed(ctx, dir); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = cfg.Update(func(c *config.Config) error {
		c.SetupComplete = true
		c.Plex.Token, c.Plex.OwnerPlexID = "parity-owner", 1
		c.Cache.Dir, c.Transcode.TempDir = filepath.Join(dir, "cache"), filepath.Join(dir, "transcode")
		c.Transcode.FFmpegPath = "/not-installed/ffmpeg"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	server, err := app.New(ctx, app.Options{DataDir: dir, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	for attempt := 0; attempt < 3; attempt++ {
		requestCtx, done := context.WithTimeout(ctx, 5*time.Second)
		req := httptest.NewRequest("GET", "/hubs/metadata/1000000000010001/postplay", nil).WithContext(requestCtx)
		req.Header.Set("X-Plex-Token", "parity-child")
		req.Header.Set("X-Plex-Client-Identifier", "parity-child")
		req.Header.Set("Accept", "application/json")
		response := httptest.NewRecorder()
		start := time.Now()
		server.Handler().ServeHTTP(response, req)
		elapsed := time.Since(start)
		err = requestCtx.Err()
		done()
		if err != nil || response.Code != 200 {
			t.Fatalf("attempt=%d elapsed=%s status=%d err=%v body=%s", attempt, elapsed, response.Code, err, response.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		t.Logf("child movie postplay attempt=%d elapsed=%s", attempt, elapsed)
	}
}
