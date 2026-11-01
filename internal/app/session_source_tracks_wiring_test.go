//go:build scorecard && !plexparity

package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/plextv"
	"github.com/braedonsaunders/ether/internal/trackprefs"
)

// Exercises the broker installed by App.New, including its real scoped track
// preference callback. The scorecard source is a local generated fixture.
func TestSourceTracksThroughAppBrokerWiring(t *testing.T) {
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	stub := httptest.NewServer(http.NotFoundHandler())
	defer stub.Close()
	t.Setenv("ETHER_PARITY_TMDB_URL", stub.URL)
	t.Setenv("ETHER_PARITY_PLEXTV_URL", stub.URL)
	dir := t.TempDir()
	cfg, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Update(func(c *config.Config) error {
		c.SetupComplete = true
		c.Plex.Token = "fixture-owner"
		c.Catalog.TMDBKey = "fixture"
		c.Transcode.FFmpegPath = ff
		c.Transcode.HWAccel = "none"
		for i := range c.Quality.Profiles {
			c.Quality.Profiles[i].MinSizeMBPerMin = 0
			c.Quality.Profiles[i].MaxSizeMBPerMin = 0
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	a, err := New(context.Background(), Options{DataDir: dir, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := a.users.SetOwner(ctx, plextv.Account{ID: 99, Username: "owner"}, "owner-token"); err != nil {
		t.Fatal(err)
	}
	viewer, err := a.users.Owner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	title := core.Title{Key: core.MovieKey(44), Type: core.TypeMovie, IDs: core.IDs{TMDB: 44}, Title: "Track Fixture", Released: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)}
	if err := a.catalog.UpsertTitle(ctx, title, true); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.ExecContext(ctx, `UPDATE catalog_titles SET detail=1,detail_at=? WHERE key=?`, time.Now().Unix(), int64(title.Key)); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "fixture.srt")
	if err := os.WriteFile(sub, []byte("1\n00:00:00,000 --> 00:00:01,500\nfixture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "fixture.mkv")
	args := []string{"-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "color=size=160x90:rate=24", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-i", sub, "-f", "lavfi", "-i", "sine=frequency=880:sample_rate=48000", "-map", "0:v:0", "-map", "1:a:0", "-map", "2:s:0", "-map", "3:a:0", "-t", "2", "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-c:a", "aac", "-c:s", "srt", "-metadata:s:a:0", "language=eng", "-metadata:s:a:1", "language=fra", "-metadata:s:s:0", "language=fra", path}
	if output, err := exec.CommandContext(ctx, ff, args...).CombinedOutput(); err != nil {
		t.Fatalf("generate fixture: %v: %s", err, output)
	}
	// Resolver rejects releases below its absolute 5 MiB sanity floor. Pad
	// the local generated fixture without changing its encoded tracks.
	if err := os.Truncate(path, 6<<20); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < 5<<20 {
		t.Fatalf("fixture below resolver minimum: %d bytes", len(data))
	}
	digest := sha256.Sum256(data)
	files := []ScorecardFile{{TMDB: 44, Path: path, SHA256: hex.EncodeToString(digest[:]), Info: core.ReleaseInfo{VideoCodec: "h264", AudioCodec: "aac", Container: "mkv", Height: 720}}}
	manifest, err := json.Marshal(files)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scorecard-files.json"), manifest, 0600); err != nil {
		t.Fatal(err)
	}
	rules := trackprefs.Default()
	rules.AudioLanguages = []string{"fr"}
	rules.SubtitleLanguages = []string{"fr"}
	rules.SubtitleMode = "always"
	if err := a.trackprefs.PutRules(ctx, viewer.ID, rules); err != nil {
		t.Fatal(err)
	}
	if a.frontends == nil || a.frontends.broker == nil {
		t.Fatal("production app broker unavailable")
	}
	audio, subtitle, off := 0, 2, -1
	req := frontend.PlanRequest{Viewer: viewer, Device: core.Device{ID: "track-wiring"}, Protocol: "silo", Item: title.Key, Caps: core.ClientCaps{ClientID: "track-wiring", Protocol: "silo", Containers: []string{"mkv"}, VideoCodecs: []string{"h264"}, AudioCodecs: []string{"aac"}, SubtitleCodecs: []string{"srt"}}, Intent: frontend.PlayIntent{AudioOrdinal: &audio, SubtitleStreamIndex: &subtitle, Options: core.DecideOptions{DirectPlay: true, DirectStream: true, AudioStream: -1, SubtitleStream: -1}}}
	cases := []struct {
		name               string
		audio, sub         *int
		wantAudio, wantSub int
	}{{"explicit", &audio, &subtitle, 1, 2}, {"off", nil, &off, 3, -1}, {"saved", nil, nil, 3, 2}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req.Intent.AudioOrdinal, req.Intent.SubtitleStreamIndex = tc.audio, tc.sub
			plan, err := a.frontends.broker.Plan(ctx, req)
			if err != nil {
				t.Fatalf("plan error=%v resolver=%v", err, scorecardResolverTraces(a.resolver.Traces()))
			}
			if plan.Media.Estimated || plan.Decision.AudioStream != tc.wantAudio || plan.Decision.SubtitleStream != tc.wantSub {
				t.Fatalf("plan decision=%+v estimated=%v", plan.Decision, plan.Media.Estimated)
			}
		})
	}
}
