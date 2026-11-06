package playback

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/mediaproc"
	"github.com/braedonsaunders/ether/internal/transcode"
)

// A failed candidate and a gated alternate exercise the real playback and
// encoder managers while keeping source availability deterministic.
type continuityEngine struct {
	*realTrackEngine
	alternate string
	entered   chan struct{}
	release   chan struct{}
	once      sync.Once
}

type continuityPacedRunner struct{ transcode.ExecRunner }

func (r continuityPacedRunner) Start(cmd transcode.Command) (transcode.Process, error) {
	for i, arg := range cmd.Args {
		if arg == "-i" {
			args := append([]string(nil), cmd.Args[:i]...)
			args = append(args, "-readrate", "4")
			cmd.Args = append(args, cmd.Args[i:]...)
			break
		}
	}
	return r.ExecRunner.Start(cmd)
}

func (e *continuityEngine) Open(ctx context.Context, id string, nzb []byte) (core.MediaSource, error) {
	switch id {
	case "bad":
		return nil, core.ErrSourceUnavailable
	case "alternate":
		e.once.Do(func() { close(e.entered) })
		select {
		case <-e.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return (&realTrackEngine{path: e.alternate}).Open(ctx, id, nzb)
	}
	return e.realTrackEngine.Open(ctx, id, nzb)
}

func TestOfflineBufferedSwapKeepsDecodableContiguousOutput(t *testing.T) {
	if testing.Short() {
		t.Skip("requires ffmpeg")
	}
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	fp, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe unavailable")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	dir := t.TempDir()
	fixture := filepath.Join(dir, "source.mkv")
	args := []string{"-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=24", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-t", "24", "-c:v", "libx264", "-preset", "ultrafast", "-threads", "1", "-g", "48", "-c:a", "aac", fixture}
	if out, err := mediaproc.CommandContext(ctx, 2*time.Minute, ff, args...).CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v: %s", err, out)
	}
	alternate := filepath.Join(dir, "alternate.mkv")
	if out, err := mediaproc.CommandContext(ctx, 2*time.Minute, ff, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=24", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-t", "24", "-vf", "hue=s=0", "-c:v", "libx264", "-preset", "ultrafast", "-threads", "1", "-g", "48", "-c:a", "aac", alternate).CombinedOutput(); err != nil {
		t.Fatalf("alternate fixture: %v: %s", err, out)
	}
	tc, err := transcode.New(config.Transcode{FFmpegPath: ff, FFprobePath: fp, HWAccel: "none", TempDir: filepath.Join(dir, "hls")}, slog.New(slog.NewTextHandler(io.Discard, nil)), transcode.WithoutHWDetection(), transcode.WithRunner(continuityPacedRunner{}), transcode.WithBuffering(8*time.Second, 8*time.Second, 20*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tc.Close() })
	engine := &continuityEngine{realTrackEngine: &realTrackEngine{path: fixture}, alternate: alternate, entered: make(chan struct{}), release: make(chan struct{})}
	h := newHarness(t, nil, []string{"track-source", "bad", "alternate"}, func(h *harness, _ *Options) { h.testEngine, h.testTranscode = engine, tc })
	ps := h.prepare(owner, movieKey, "continuity")
	decision := tc.Decide(ps.Info, defaultCaps("continuity"), core.DecideOptions{DirectStream: true, AudioStream: -1, SubtitleStream: -1})
	hs, err := tc.StartHLS(ctx, core.HLSRequest{SessionID: ps.ID, InputURL: h.m.InputURL(ps.ID), Info: ps.Info, Decision: decision, SegmentDuration: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	fetch := func(n int) []byte {
		t.Helper()
		w := httptest.NewRecorder()
		hs.ServeSegment(w, httptest.NewRequest(http.MethodGet, "/segment", nil).WithContext(ctx), n)
		if w.Code != http.StatusOK || w.Body.Len() == 0 {
			t.Fatalf("segment %d status %d bytes %d", n, w.Code, w.Body.Len())
		}
		return append([]byte(nil), w.Body.Bytes()...)
	}
	first := fetch(0)
	second := fetch(1)
	s := h.m.lookup(ps.ID)
	old := s.current()
	done := make(chan error, 1)
	go func() {
		_, e := h.m.failoverWith(ctx, s, old, 2*time.Second, failoverOpts{preserveBuffer: true})
		done <- e
	}()
	select {
	case <-engine.entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if s.current() != old || !old.acquire() {
		t.Fatal("original retired while candidate open was delayed")
	}
	old.release()
	if got := fetch(1); string(got) != string(second) {
		t.Fatal("published old segment changed during candidate open")
	}
	close(engine.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if s.current() == old {
		t.Fatal("replacement did not publish")
	}
	media := append(first, second...)
	for n := 2; n < 8; n++ {
		media = append(media, fetch(n)...)
	}
	path := filepath.Join(dir, "handoff.ts")
	if err := os.WriteFile(path, media, 0600); err != nil {
		t.Fatal(err)
	}
	probe := mediaproc.CommandContext(ctx, 2*time.Minute, fp, "-v", "error", "-select_streams", "v:0", "-show_entries", "packet=pts_time", "-of", "json", path)
	out, err := probe.Output()
	if err != nil {
		t.Fatal(err)
	}
	var packets struct {
		Packets []struct {
			PTS string `json:"pts_time"`
		} `json:"packets"`
	}
	if err := json.Unmarshal(out, &packets); err != nil {
		t.Fatal(err)
	}
	if len(packets.Packets) != 8*2*24 {
		t.Fatalf("got %d video packets, want 384", len(packets.Packets))
	}
	var previous float64
	for i, packet := range packets.Packets {
		pts, err := strconv.ParseFloat(packet.PTS, 64)
		if err != nil {
			t.Fatal(err)
		}
		if i > 0 && (pts <= previous || pts-previous > 1.0/24+0.0015) {
			t.Fatalf("packet %d has a playback gap: %.6f", i, pts-previous)
		}
		previous = pts
	}
	if out, err := mediaproc.CommandContext(ctx, 2*time.Minute, ff, "-v", "error", "-xerror", "-i", path, "-fps_mode", "passthrough", "-enc_time_base:v", "1:90000", "-f", "null", "-").CombinedOutput(); err != nil {
		t.Fatalf("decode: %v: %s", err, out)
	}
	frame, err := mediaproc.CommandContext(ctx, 2*time.Minute, ff, "-v", "error", "-i", path, "-vf", "select=gte(n\\,360),scale=32:32", "-frames:v", "1", "-pix_fmt", "rgb24", "-f", "rawvideo", "-").Output()
	if err != nil || len(frame) != 32*32*3 {
		t.Fatalf("replacement frame: %d bytes, %v", len(frame), err)
	}
	var colorDelta int
	for i := 0; i < len(frame); i += 3 {
		colorDelta += absColor(int(frame[i])-int(frame[i+1])) + absColor(int(frame[i+1])-int(frame[i+2]))
	}
	if colorDelta > 4*32*32 {
		t.Fatalf("suffix did not contain the grayscale replacement: color delta %d", colorDelta)
	}
}

func absColor(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
