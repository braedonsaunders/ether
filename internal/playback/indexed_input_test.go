package playback

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/mediaproc"
	"github.com/braedonsaunders/ether/internal/transcode"
)

// A real indexed Matroska projection must decode from the preceding keyframe
// and trim to the requested instant, with no original-file cue walk/seek.
// Blue becomes red between keyframes; checking red proves the forward trim.
func TestIndexedMatroskaInputDecodesAtTarget(t *testing.T) {
	if testing.Short() {
		t.Skip("real indexed ffmpeg input")
	}
	ff, e := exec.LookPath("ffmpeg")
	if e != nil {
		t.Skip(e)
	}
	fp, e := exec.LookPath("ffprobe")
	if e != nil {
		t.Skip(e)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	dir := t.TempDir()
	path := filepath.Join(dir, "source.mkv")
	args := []string{"-v", "error", "-y", "-f", "lavfi", "-i", "color=blue:size=64x36:rate=30:duration=6", "-f", "lavfi", "-i", "color=red:size=64x36:rate=30:duration=4", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-filter_complex", "[0:v][1:v]concat=n=2:v=1:a=0[v]", "-map", "[v]", "-map", "2:a", "-t", "10", "-c:v", "libx264", "-threads", "1", "-preset", "ultrafast", "-g", "120", "-sc_threshold", "0", "-pix_fmt", "yuv420p", "-c:a", "aac", "-ac", "2", path}
	if out, e := mediaproc.CommandContext(ctx, 2*time.Minute, ff, args...).CombinedOutput(); e != nil {
		t.Fatalf("fixture:%v %s", e, out)
	}
	tc, e := transcode.New(config.Transcode{FFmpegPath: ff, FFprobePath: fp, TempDir: filepath.Join(dir, "out")}, slog.New(slog.NewTextHandler(io.Discard, nil)), transcode.WithoutHWDetection())
	if e != nil {
		t.Fatal(e)
	}
	defer tc.Close()
	h := newHarness(t, nil, []string{"track-source"}, func(h *harness, _ *Options) { h.testEngine = &realTrackEngine{path: path}; h.testTranscode = tc })
	ps := h.prepare(owner, movieKey, "indexed-source")
	g := h.m.lookup(ps.ID).current()
	select {
	case <-g.indexReady:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	target := 6500 * time.Millisecond
	input, trim, ok := h.m.IndexedInput(ctx, h.m.InputURL(ps.ID), target)
	if !ok {
		t.Fatalf("index projection not ready: container=%s points=%v", ps.Info.Container, g.timeMapSnapshot().Points)
	}
	u, e := url.Parse(input)
	if e != nil {
		t.Fatal(e)
	}
	cluster, e := strconv.ParseInt(u.Query().Get("cluster"), 10, 64)
	if e != nil || cluster <= 0 || trim <= time.Second || trim >= 4*time.Second {
		t.Fatalf("wrong indexed seek: cluster%d trim%s", cluster, trim)
	}
	format := transcode.RawFormat{Width: 64, Height: 36, FPSNum: 30, FPSDen: 1, SampleRate: 48000, Channels: 2, PixelFormat: "yuv420p"}
	d, e := tc.StartDecoder(ctx, transcode.DecoderRequest{InputURL: input, Offset: trim, Seeked: true, Format: format})
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	first, e := d.Next(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if first.Video[0] < 79 || first.Video[0] > 83 || first.Video[64*36+64*36/4] < 238 {
		t.Fatalf("indexed projection did not trim blue lead-in: Y%d V%d", first.Video[0], first.Video[64*36+64*36/4])
	}
	q := u.Query()
	q.Set("cluster", "1")
	u.RawQuery = q.Encode()
	req, _ := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	res, e := http.DefaultClient.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatal("unindexed byte offset admitted", res.StatusCode)
	}
	t.Logf("indexed source cluster%d forwardTrim%s decoded correct target colour", cluster, trim)
}
