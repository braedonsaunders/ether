package livetv

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/mediaproc"
	"github.com/braedonsaunders/ether/internal/transcode"
)

type remoteSampleEncoder struct{ broadcastFakeEncoder }

func (e *remoteSampleEncoder) Write(ctx context.Context, f transcode.RawFrame) error {
	n := len(f.Video) * 2 / 3
	return e.broadcastFakeEncoder.Write(ctx, transcode.RawFrame{Video: []byte{f.Video[0], f.Video[1], f.Video[2], f.Video[3], f.Video[n], f.Video[n+n/4]}, PCM: f.PCM})
}

type slowRemoteBroadcastRunner struct {
	*transcode.Manager
	encoder  *remoteSampleEncoder
	attempts atomic.Int64
	offset   atomic.Int64
	origin   time.Time
}

func (r *slowRemoteBroadcastRunner) StartEncoder(context.Context, transcode.EncoderRequest) (transcode.ContinuousEncoder, error) {
	return r.encoder, nil
}
func (r *slowRemoteBroadcastRunner) StartDecoder(ctx context.Context, req transcode.DecoderRequest) (transcode.Decoder, error) {
	r.offset.Store(int64(req.Offset))
	if r.attempts.Add(1) == 1 {
		return &broadcastFakeDecoder{origin: r.origin, format: req.Format, closed: make(chan struct{}), stall: true}, nil
	}
	return r.Manager.StartDecoder(ctx, req)
}

// Use actual ffmpeg seeking into a range-served MKV at >1h40. Every range has
// 1.3s TTFB. A previous decoder deliberately stalls first, so the replacement
// must not inherit its stale freshness timestamp or the 1.5s running deadline.
func TestBroadcastSlowStartLargeOffsetRemote(t *testing.T) {
	if testing.Short() {
		t.Skip("real ffmpeg slow remote seek")
	}
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip(err)
	}
	fp, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	dir := t.TempDir()
	path := filepath.Join(dir, "long.mkv")
	if out, e := mediaproc.CommandContext(ctx, 2*time.Minute, ff, "-v", "error", "-y", "-f", "lavfi", "-i", "color=red:size=64x36:rate=1", "-f", "lavfi", "-i", "anullsrc=r=48000:cl=stereo", "-t", "6104", "-c:v", "libx264", "-threads", "1", "-preset", "ultrafast", "-tune", "zerolatency", "-g", "10", "-c:a", "aac", "-b:a", "16k", path).CombinedOutput(); e != nil {
		t.Fatalf("fixture:%v %s", e, out)
	}
	var ranges atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.Header.Get("Range"), "bytes=") {
			ranges.Add(1)
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(1300 * time.Millisecond):
		}
		http.ServeFile(w, r, path)
	}))
	defer server.Close()
	manager, err := transcode.New(config.Transcode{FFmpegPath: ff, FFprobePath: fp, TempDir: filepath.Join(dir, "out")}, slog.New(slog.NewTextHandler(io.Discard, nil)), transcode.WithoutHWDetection())
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	runner := &slowRemoteBroadcastRunner{Manager: manager, encoder: &remoteSampleEncoder{}, origin: time.Now()}
	s := testService(t, Options{Playback: &lookDemoPlayback{path: server.URL + "/long.mkv"}, Screens: func() config.LiveTVScreens { return config.LiveTVScreens{NoSignal: "static", ApplyTo: "all"} }})
	format := transcode.RawFormat{Width: 1920, Height: 1080, FPSNum: 30, FPSDen: 1, SampleRate: 48000, Channels: 2, PixelFormat: "yuv420p"}
	origin := time.Now()
	c := testChannel()
	c.Mode = "order"
	c.Items = []Item{{Key: 1, Title: "Slow remote programme", DurationMS: 24 * 60 * 60 * 1000}}
	c.Epoch = origin.Add(-6000 * time.Second)
	// Mode=order uses the weekly midnight anchor. Use the legacy epoch recipe so
	// the requested join is exactly the large offset under test.
	c.Mode = ""
	planner := &scheduleBroadcastPlanner{s: s, channel: c, user: core.User{ID: 1, IsOwner: true}, format: format, runner: runner}
	b, err := newBroadcast(ctx, runner, planner, format)
	if err != nil {
		t.Fatal(err)
	}
	defer b.close()
	if _, err = b.addRendition(ctx, "main", core.Decision{}); err != nil {
		t.Fatal(err)
	}
	runner.origin = time.Now()
	b.origin = runner.origin
	b.start()
	for {
		frames := runner.encoder.snapshot()
		red := 0
		for _, frame := range frames {
			if len(frame.Video) == 6 && frame.Video[0] >= 79 && frame.Video[0] <= 83 && frame.Video[4] >= 88 && frame.Video[4] <= 92 && frame.Video[5] >= 238 {
				red++
			}
		}
		if red >= 30 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("remote never played: %v attempts=%d offset=%s frames=%s", ctx.Err(), runner.attempts.Load(), time.Duration(runner.offset.Load()), strconv.Itoa(len(frames)))
		case <-time.After(100 * time.Millisecond):
		}
	}
	b.close()
	if runner.offset.Load() < int64(6000*time.Second) {
		t.Fatal("large seek lost", time.Duration(runner.offset.Load()))
	}
	if runner.attempts.Load() != 2 {
		t.Fatalf("slow decoder repeatedly abandoned: %d", runner.attempts.Load())
	}
	if ranges.Load() < 1 {
		t.Fatal("fixture was not range-seeked")
	}
	t.Logf("remote recovered after %s, offset=%s, range requests=%d", time.Since(runner.origin), time.Duration(runner.offset.Load()), ranges.Load())
}
