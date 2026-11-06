package livetv

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/mediaproc"
	"github.com/braedonsaunders/ether/internal/onair/signal"
	"github.com/braedonsaunders/ether/internal/transcode"
)

var crispDemo = flag.String("static-crisp-demo", "", "offline native-clock switcher/continuous encoder evidence directory")

// Offline frame stepping preserves every live generated frame under host load.
// It exercises the production switcher and continuous encoder, without claiming realtime performance.
func TestStaticCrispDemo(t *testing.T) {
	if *crispDemo == "" {
		t.Skip("opt-in evidence")
	}
	for _, h := range []int{1080, 720} {
		t.Run(fmt.Sprint(h), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
			defer cancel()
			ff, _ := exec.LookPath("ffmpeg")
			fp, _ := exec.LookPath("ffprobe")
			manager, err := transcode.New(config.Transcode{FFmpegPath: ff, FFprobePath: fp, TempDir: t.TempDir(), HWAccel: "none"}, slog.New(slog.NewTextHandler(io.Discard, nil)), transcode.WithoutHWDetection())
			if err != nil {
				t.Fatal(err)
			}
			defer manager.Close()
			f := transcode.RawFormat{Width: h * 16 / 9, Height: h, FPSNum: 30, FPSDen: 1, SampleRate: 48000, Channels: 2, PixelFormat: "yuv420p"}
			ceiling := 5000
			name := "static"
			if h == 720 {
				ceiling = 2000
				name += "-720p"
			}
			enc, err := manager.StartEncoder(ctx, transcode.EncoderRequest{SessionID: "crisp", Source: f, Decision: core.Decision{Kind: core.DecisionTranscode, VideoCodec: "h264", AudioCodec: "aac", MaxHeight: h, BitrateKbps: ceiling, AudioBitrateKbps: 192, AudioChannels: 2, AudioSampleRate: 48000, H264Level: 40}, SegmentDuration: 4 * time.Second, DVRWindow: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			defer enc.Close()
			plan := broadcastPlan{id: "crisp"}
			b, err := newBroadcast(ctx, manager, liveSignalPlanner{plan}, f)
			if err != nil {
				t.Fatal(err)
			}
			defer b.cancel()
			d := &signalInputDecoder{engine: signal.NewStatic(f.Width, f.Height, 35), format: f, audio: make([]int16, 3200), closed: make(chan struct{})}
			b.current = &broadcastInput{ctx: ctx, cancel: func() {}, plan: plan, decoder: d, frames: make(chan transcode.RawFrame, 1), errs: make(chan error, 1)}
			q := newBroadcastFrameQueue(30)
			b.renditions["evidence"] = &broadcastRendition{queue: q, cancel: func() {}}
			start := time.Now()
			for n := 0; n < 660; n++ {
				raw, e := d.Next(ctx)
				if e != nil {
					t.Fatal(e)
				}
				b.current.frames <- raw
				b.tick = int64(n)
				b.step(b.origin.Add(time.Duration(n) * time.Second / 30))
				frame, ok := q.pop(ctx)
				if !ok {
					t.Fatal("evidence queue closed")
				}
				if e = enc.Write(ctx, frame); e != nil {
					t.Fatal(e)
				}
			}
			index := enc.Segments()
			for {
				last, ok := index.Latest()
				if ok && last.Seq >= 6 {
					break
				}
				if _, err = index.Wait(ctx, func() int64 {
					if ok {
						return last.Seq
					}
					return -1
				}()); err != nil {
					t.Fatal(err)
				}
			}
			file, err := os.Create(filepath.Join(*crispDemo, name+".ts"))
			if err != nil {
				t.Fatal(err)
			}
			segmentRates := []float64{}
			for _, seg := range index.Since(-1, 7) {
				rec := httptest.NewRecorder()
				index.ServeTS(rec, httptest.NewRequest("GET", "/", nil), seg.Seq)
				if rec.Code != 200 {
					t.Fatal(rec.Code)
				}
				rate := float64(rec.Body.Len()*8) / seg.Duration.Seconds() / 1000
				segmentRates = append(segmentRates, rate)
				if rate > float64(ceiling) {
					t.Fatalf("TS segment exceeds ceiling: %.1fk", rate)
				}
				file.Write(rec.Body.Bytes())
			}
			file.Close()
			report, _ := json.MarshalIndent(map[string]any{"mode": "offline deterministic frame stepping", "media_seconds": 22, "switcher_encoder_elapsed_s": time.Since(start).Seconds(), "segment_ts_kbps": segmentRates, "ceiling_kbps": ceiling}, "", "  ")
			if err := os.WriteFile(filepath.Join(*crispDemo, name+"-switcher.json"), report, 0644); err != nil {
				t.Fatal(err)
			}
			enc.Close()
			manager.Close()
			t.Logf("offline switcher+encoder %s: 22s frames in %.3fs (%.3fx realtime), ceiling %dk", name, time.Since(start).Seconds(), 22/time.Since(start).Seconds(), ceiling)
			out := filepath.Join(*crispDemo, name+".mp4")
			if bytes, e := mediaproc.CommandContext(ctx, 2*time.Minute, ff, "-v", "error", "-y", "-i", filepath.Join(*crispDemo, name+".ts"), "-t", "20", "-c", "copy", "-movflags", "+faststart", out).CombinedOutput(); e != nil {
				t.Fatalf("%v %s", e, bytes)
			}
			os.Remove(filepath.Join(*crispDemo, name+".ts"))
		})
	}
}
