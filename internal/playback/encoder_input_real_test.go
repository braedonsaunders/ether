package playback

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/transcode"
)

// Real manager-issued scope, loopback producers and actual FFmpeg guardian
// exercise the complete optional proof without activating a broker caller.
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
	h := newHarness(t, nil, []string{"track-source"}, func(h *harness, _ *Options) { h.testEngine, h.testTranscode = &realTrackEngine{path: path}, tc })
	caps := defaultCaps("real-input")
	ps, source, err := h.m.PrepareSurfaceSource(ctx, owner, movieKey, caps, "", false)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := h.m.ReserveEncoderInput(ctx, ps.ID, source)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lease.SealInput)
	d := tc.Decide(ps.Info, caps, core.DecideOptions{DirectStream: true, AudioStream: -1, SubtitleStream: -1})
	hs, encoder, err := tc.StartHLSOwned(ctx, core.HLSRequest{SessionID: "actual-encoder", InputURL: h.m.InputURL(ps.ID), InputLease: lease, Info: ps.Info, Decision: d})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := hs.Close(); err != nil {
			t.Error(err)
		}
	})
	w := httptest.NewRecorder()
	hs.ServeSegment(w, httptest.NewRequest(http.MethodGet, "/segment0", nil).WithContext(ctx), 0)
	if w.Code != http.StatusOK || w.Body.Len() == 0 {
		t.Fatalf("real owned media=%d bytes%d", w.Code, w.Body.Len())
	}
	if err := hs.Close(); err != nil {
		t.Fatal(err)
	}
	waitRetirementTest(t, encoder.EncoderDone(), "actual encoder retirement")
	if err := encoder.EncoderErr(); err != nil {
		t.Fatalf("verified encoder proof=%v", err)
	}
	waitRetirementTest(t, lease.InputDone(), "actual loopback input drain")
	if err := lease.InputErr(); err != nil {
		t.Fatal(err)
	}
	if err := h.m.RetireSource(ps.ID, source); err != nil {
		t.Fatal(err)
	}
	waitRetirementTest(t, source.Done(), "independent actual source retirement")
	if err := source.Err(); err != nil {
		t.Fatal(err)
	}
}
