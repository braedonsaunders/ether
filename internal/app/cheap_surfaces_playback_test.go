//go:build scorecard && !plexparity

package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/transcode"
)

// The scorecard file adapter replaces external indexers and the download
// transport only. The mounted listeners, resolver, playback manager, broker,
// policies, capability stores and FFmpeg are the application's implementations.
func TestCheapSurfacesMountedPlayback(t *testing.T) {
	stub := httptest.NewServer(http.NotFoundHandler())
	defer stub.Close()
	t.Setenv("ETHER_PARITY_TMDB_URL", stub.URL)
	t.Setenv("ETHER_PARITY_PLEXTV_URL", stub.URL)
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "cert.mp4")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, ff, "-v", "error", "-f", "lavfi", "-i", "color=size=320x180:rate=24", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-t", "120", "-c:v", "libx264", "-threads", "1", "-pix_fmt", "yuv420p", "-c:a", "aac", "-movflags", "+faststart", path).CombinedOutput()
	if err != nil {
		t.Fatalf("generate media: %v %s", err, output)
	}
	if err = os.Truncate(path, 6<<20); err != nil {
		t.Fatal(err)
	}
	media, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	probe, err := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-print_format", "json", "-show_format", "-show_streams", path).Output()
	if err != nil {
		t.Fatal(err)
	}
	info, err := transcode.ParseProbe(probe)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(media)
	manifest, err := json.Marshal([]ScorecardFile{{TMDB: 999, Path: path, Media: info, SHA256: hex.EncodeToString(sum[:]), Info: core.ReleaseInfo{VideoCodec: "h264", AudioCodec: "aac", Container: "mp4", Height: 720}}})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "scorecard-files.json"), manifest, 0600); err != nil {
		t.Fatal(err)
	}
	testCheapSurfacesMounted(t, dir, func(c *config.Config) {
		c.Catalog.TMDBKey = "fixture"
		c.Transcode.FFmpegPath = ff
		c.Transcode.HWAccel = "none"
		c.Server.MaxStreams = 30
		c.Server.DefaultStreamLimit = 10 // Four concurrent protocol sessions belong to this viewer.
		for i := range c.Quality.Profiles {
			c.Quality.Profiles[i].MinSizeMBPerMin = 0
			c.Quality.Profiles[i].MaxSizeMBPerMin = 0
		}
	}, media)
}
