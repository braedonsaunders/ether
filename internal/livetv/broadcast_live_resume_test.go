package livetv

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/mediaproc"
	"github.com/braedonsaunders/ether/internal/transcode"
)

type liveResumeRunner struct {
	*transcode.Manager
	encoder *broadcastFakeEncoder
}

func (r liveResumeRunner) StartEncoder(context.Context, transcode.EncoderRequest) (transcode.ContinuousEncoder, error) {
	return r.encoder, nil
}

// A provider may keep serving its last eight segments throughout an outage.
// Reopening that playlist must not replay old source PTS. The screen remains
// until the same live playlist publishes genuinely new media.
func TestBroadcastStaleLivePlaylistDoesNotReplay(t *testing.T) {
	if testing.Short() {
		t.Skip("real ffmpeg stale live HLS")
	}
	ff, e := exec.LookPath("ffmpeg")
	if e != nil {
		t.Skip(e)
	}
	fp, e := exec.LookPath("ffprobe")
	if e != nil {
		t.Skip(e)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	dir := t.TempDir()
	if out, e := mediaproc.CommandContext(ctx, 2*time.Minute, ff, "-v", "error", "-y", "-f", "lavfi", "-i", "color=red:size=64x36:rate=30", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-t", "40", "-c:v", "libx264", "-threads", "1", "-preset", "ultrafast", "-g", "120", "-sc_threshold", "0", "-pix_fmt", "yuv420p", "-c:a", "aac", "-ac", "2", "-f", "hls", "-hls_time", "4", "-hls_list_size", "0", "-hls_segment_filename", filepath.Join(dir, "seg-%03d.ts"), filepath.Join(dir, "full.m3u8")).CombinedOutput(); e != nil {
		t.Fatalf("fixture:%v %s", e, out)
	}
	var advancing atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".m3u8") {
			first := 0
			if advancing.Load() {
				first = 2
			}
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			fmt.Fprintf(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:%d\n", first)
			for n := first; n < first+8; n++ {
				fmt.Fprintf(w, "#EXTINF:4.000000,\nseg-%03d.ts\n", n)
			}
			return
		}
		http.ServeFile(w, r, filepath.Join(dir, filepath.Base(r.URL.Path)))
	}))
	defer server.Close()
	manager, e := transcode.New(config.Transcode{FFmpegPath: ff, FFprobePath: fp, TempDir: filepath.Join(dir, "out")}, slog.New(slog.NewTextHandler(io.Discard, nil)), transcode.WithoutHWDetection())
	if e != nil {
		t.Fatal(e)
	}
	defer manager.Close()
	encoder := &broadcastFakeEncoder{}
	runner := liveResumeRunner{manager, encoder}
	s := testService(t, Options{})
	watermark := &providerBroadcastPlanner{}
	format := smallBroadcastFormat()
	var attempts atomic.Int64
	plan := broadcastPlan{id: "live-provider", open: func(ctx context.Context) (transcode.Decoder, func(), error) {
		attempts.Add(1)
		d, e := runner.StartDecoder(ctx, transcode.DecoderRequest{InputURL: server.URL + "/index.m3u8", Format: format, Live: true})
		if e != nil {
			return nil, nil, e
		}
		return &providerBroadcastDecoder{Decoder: d, planner: watermark, sourceID: "provider", ready: func() {}}, nil, nil
	}}
	b, e := newBroadcast(ctx, runner, broadcastFakePlanner{plan: plan, screenPlan: s.signalPlan(Channel{ID: "live", Screens: &ChannelScreens{NoSignal: "static"}}, "reconnecting", "", format)}, format)
	if e != nil {
		t.Fatal(e)
	}
	defer b.close()
	if _, e = b.addRendition(ctx, "main", core.Decision{}); e != nil {
		t.Fatal(e)
	}
	start := time.Now()
	b.origin = start
	b.start()
	select {
	case <-time.After(24 * time.Second):
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	frames := encoder.snapshot()
	last := time.Duration(-1)
	screen := 0
	for i, frame := range frames {
		if frame.SourcePTSValid {
			if frame.SourcePTS < last {
				t.Fatalf("replayed source PTS at tick%d: %s after%s", i, frame.SourcePTS, last)
			}
			last = frame.SourcePTS
		} else if i >= 600 {
			screen++
		}
	}
	if last < time.Second || screen < 60 || attempts.Load() < 2 {
		t.Fatalf("stale playlist did not hold screen: last=%s screenTicks=%d attempts=%d", last, screen, attempts.Load())
	}
	advancing.Store(true)
	for {
		frames = encoder.snapshot()
		newMedia := false
		for _, frame := range frames {
			if frame.SourcePTSValid && frame.SourcePTS > last+time.Second {
				newMedia = true
				break
			}
		}
		if newMedia {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("new media never resumed:%v", ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
	b.close()
	last = -1
	for i, frame := range encoder.snapshot() {
		if frame.SourcePTSValid {
			if frame.SourcePTS < last {
				t.Fatalf("source PTS went backwards at tick%d: %s after%s", i, frame.SourcePTS, last)
			}
			last = frame.SourcePTS
		}
	}
	t.Logf("continuous live output resumed only on new PTS, attempts=%d elapsed=%s finalPTS=%s", attempts.Load(), time.Since(start), last)
}
