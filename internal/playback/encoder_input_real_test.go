package playback

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/handles/capability"
	"github.com/braedonsaunders/ether/internal/sessions"
	"github.com/braedonsaunders/ether/internal/transcode"
)

// The broker mints the input lease. Real FFmpeg either drains it, or a start
// without that claim leaves ErrEncoderInputUnverified and the transfer claim.
func TestEncoderInputRealManagersProveDrain(t *testing.T) {
	if testing.Short() {
		t.Skip("requires existing FFmpeg tools")
	}
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	fp, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	dir := t.TempDir()
	path := filepath.Join(dir, "source.mkv")
	args := []string{"-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "color=size=160x90:rate=24", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-t", "2", "-c:v", "libx264", "-preset", "ultrafast", "-threads", "1", "-pix_fmt", "yuv420p", "-c:a", "aac", path}
	if output, err := exec.CommandContext(ctx, ff, args...).CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v: %s", err, output)
	}
	tc, err := transcode.New(config.Transcode{FFmpegPath: ff, FFprobePath: fp, HWAccel: "none", TempDir: filepath.Join(dir, "hls")}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := tc.Close(); err != nil {
			t.Error(err)
		}
	})
	openBroker := func(t *testing.T) (*harness, *sessions.Broker, frontend.Session, frontend.SessionAccess) {
		t.Helper()
		h := newHarness(t, nil, []string{"track-source"}, func(h *harness, _ *Options) { h.testEngine, h.testTranscode = &realTrackEngine{path: path}, tc })
		active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
		events := sessions.NewEventBus(nil, active)
		t.Cleanup(events.Close)
		lookup := func(ctx context.Context, _ *core.Viewer, _ core.Device, key core.RatingKey) (*core.Title, error) {
			return h.catalog.Get(ctx, key)
		}
		b, err := sessions.NewBroker(sessions.BrokerDeps{DB: h.m.resume.db, Events: events, Playback: h.m, Transcoder: tc, Progress: h.state, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Lookup: lookup, MediaLookup: lookup})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { b.Close(context.Background()) })
		device := core.Device{ID: "real-input"}
		caps := defaultCaps(device.ID)
		caps.HLSContainers = []string{"mpegts"}
		req := frontend.PlanRequest{Viewer: friend, Device: device, Protocol: "jellyfin", Item: movieKey, Caps: caps, Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectStream: true, AudioStream: -1, SubtitleStream: -1}}}
		plan, err := b.Plan(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		session, err := b.Open(ctx, frontend.OpenRequest{Viewer: friend, Device: device, Protocol: "jellyfin", PlanID: plan.ID})
		if err != nil {
			t.Fatal(err)
		}
		return h, b, session, frontend.SessionAccess{Viewer: friend, Device: device, Protocol: "jellyfin"}
	}
	claims := func(t *testing.T, h *harness) int {
		t.Helper()
		var n int
		if err := h.m.resume.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM capability_claims WHERE purpose=? AND namespace='broker-transfer'`, capability.RetirementOwnerClaim).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	t.Run("broker-drain", func(t *testing.T) {
		h, b, session, access := openBroker(t)
		if claims(t, h) != 1 {
			t.Fatal("open did not keep a transfer claim")
		}
		master, err := b.StartStream(ctx, access, session.ID, frontend.StreamRequest{Format: frontend.StreamHLS})
		if err != nil {
			t.Fatal(err)
		}
		fetch := func(path string) *httptest.ResponseRecorder {
			t.Helper()
			w := httptest.NewRecorder()
			b.ServeMedia(w, httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx))
			return w
		}
		base := strings.TrimSuffix(master, "master.m3u8") + "base/"
		playlist := fetch(base + "index.m3u8")
		if playlist.Code != http.StatusOK {
			t.Fatalf("playlist status %d", playlist.Code)
		}
		segmentName := ""
		for _, line := range strings.Split(playlist.Body.String(), "\n") {
			if strings.HasSuffix(line, ".ts") && !strings.ContainsAny(line, "/\\") {
				segmentName = line
				break
			}
		}
		if segmentName == "" {
			t.Fatalf("no segment in %q", playlist.Body.String())
		}
		segment := fetch(base + segmentName)
		if segment.Code != http.StatusOK || segment.Body.Len() == 0 {
			t.Fatalf("segment status %d bytes %d", segment.Code, segment.Body.Len())
		}
		hs, ok := tc.Session(session.ID)
		if !ok || hs == nil {
			t.Fatal("broker start did not publish an encoder")
		}
		owned, ok := hs.(core.OwnedEncoderSession)
		if !ok {
			t.Fatal("encoder has no retirement outcome")
		}
		encoder := owned.EncoderRetirement()
		if err := b.Stop(ctx, access, session.ID); err != nil {
			t.Fatal(err)
		}
		waitRetirementTest(t, encoder.EncoderDone(), "broker encoder retirement")
		if err := encoder.EncoderErr(); err != nil {
			t.Fatalf("verified encoder proof=%v", err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for claims(t, h) != 0 {
			if time.Now().After(deadline) {
				t.Fatal("drained encoder kept the transfer claim")
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	t.Run("unverified-keeps-claim", func(t *testing.T) {
		h, b, session, access := openBroker(t)
		sources := h.m.Sessions()
		if len(sources) != 1 || sources[0].Source == nil {
			t.Fatalf("sources %+v", sources)
		}
		d := tc.Decide(sources[0].Info, defaultCaps("real-input"), core.DecideOptions{DirectStream: true, AudioStream: -1, SubtitleStream: -1})
		hs, err := tc.StartHLS(ctx, core.HLSRequest{SessionID: session.ID, InputURL: h.m.InputURL(sources[0].ID), Info: sources[0].Info, Decision: d})
		if err != nil {
			t.Fatal(err)
		}
		owned, ok := hs.(core.OwnedEncoderSession)
		if !ok {
			t.Fatal("encoder has no retirement outcome")
		}
		encoder := owned.EncoderRetirement()
		if err := b.Stop(ctx, access, session.ID); err != nil {
			t.Fatal(err)
		}
		waitRetirementTest(t, encoder.EncoderDone(), "unverified encoder retirement")
		if !errors.Is(encoder.EncoderErr(), core.ErrEncoderInputUnverified) {
			t.Fatalf("encoder proof=%v", encoder.EncoderErr())
		}
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if claims(t, h) != 1 {
				t.Fatalf("unverified input dropped the claim: %d", claims(t, h))
			}
			time.Sleep(20 * time.Millisecond)
		}
	})
}
