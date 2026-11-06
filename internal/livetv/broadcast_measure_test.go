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
	"runtime"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/mediaproc"
	"github.com/braedonsaunders/ether/internal/transcode"
)

var broadcastMeasure = flag.Bool("broadcast-measure", false, "measure whole shared output serially for the release gate")

// Ballast attributes a process tree including the Go host, guardians and ffmpeg.
// Sampling occurs every five seconds; unrelated BB threads are excluded.
func broadcastResources(pid int) (float64, float64, error) {
	data, err := exec.Command("bb", "ballast", "top", "--limit", "200", "--json").Output()
	if err != nil {
		return 0, 0, err
	}
	var report struct {
		Consumers []struct {
			Root  int     `json:"rootPid"`
			PIDs  []int   `json:"pids"`
			Bytes int64   `json:"bytes"`
			CPU   float64 `json:"cpuPercent"`
		} `json:"consumers"`
	}
	if err := json.Unmarshal(data, &report); err != nil {
		return 0, 0, err
	}
	best := -1
	for i, c := range report.Consumers {
		for _, p := range c.PIDs {
			if p == pid && (best < 0 || len(c.PIDs) < len(report.Consumers[best].PIDs)) {
				best = i
			}
		}
	}
	if best < 0 {
		return 0, 0, fmt.Errorf("test process absent from Ballast attribution")
	}
	c := report.Consumers[best]
	return c.CPU / 100, float64(c.Bytes) / (1024 * 1024), nil
}

func TestBroadcastResourceMeasurements(t *testing.T) {
	if !*broadcastMeasure {
		t.Skip("opt-in serial whole-output measurement")
	}
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg absent")
	}
	fp, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe absent")
	}
	for _, mode := range []struct {
		name   string
		height int
		hw     string
	}{{"software720", 720, "none"}, {"videotoolbox1080", 1080, "videotoolbox"}} {
		for _, viewers := range []int{1, 2} {
			t.Run(fmt.Sprintf("%s/%dviewers", mode.name, viewers), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
				defer cancel()
				dir := t.TempDir()
				sample := filepath.Join(dir, "source.mp4")
				size := fmt.Sprintf("%dx%d", mode.height*16/9, mode.height)
				if out, err := mediaproc.CommandContext(ctx, 2*time.Minute, ff, "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size="+size+":rate=30", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-t", "4", "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-g", "120", "-c:a", "aac", "-ac", "2", sample).CombinedOutput(); err != nil {
					t.Fatalf("source: %v %s", err, out)
				}
				manager, err := transcode.New(config.Transcode{FFmpegPath: ff, FFprobePath: fp, TempDir: filepath.Join(dir, "out"), HWAccel: mode.hw}, slog.New(slog.NewTextHandler(io.Discard, nil)))
				if err != nil {
					t.Fatal(err)
				}
				defer manager.Close()
				if mode.hw != "none" && manager.HWAccel().Name != mode.hw {
					t.Fatalf("requested hardware unavailable: %+v", manager.HWAccel())
				}
				format := transcode.RawFormat{Width: mode.height * 16 / 9, Height: mode.height, FPSNum: 30, FPSDen: 1, SampleRate: 48000, Channels: 2, PixelFormat: "yuv420p"}
				plan := broadcastPlan{id: "source", open: func(ctx context.Context) (transcode.Decoder, func(), error) {
					d, err := manager.StartDecoder(ctx, transcode.DecoderRequest{SessionID: "source", InputURL: sample, Format: format, Loop: true})
					return d, nil, err
				}}
				b, err := newBroadcast(ctx, manager, broadcastFakePlanner{plan: plan}, format)
				if err != nil {
					t.Fatal(err)
				}
				defer b.close()
				rate := 3000
				if mode.height == 1080 {
					rate = 6000
				}
				decision := core.Decision{Kind: core.DecisionTranscode, VideoCodec: "h264", AudioCodec: "aac", MaxHeight: mode.height, BitrateKbps: rate, AudioChannels: 2, AudioSampleRate: 48000}
				for n := 0; n < viewers; n++ {
					if _, err := b.addRendition(ctx, "shared", decision); err != nil {
						t.Fatal(err)
					}
				}
				b.start()
				s := testService(t, Options{Transcoder: manager})
				channel := testChannel()
				channel.Items = []Item{{Key: 1, Title: "Measurement", DurationMS: 3600000}}
				if _, err := s.SaveChannel(ctx, channel); err != nil {
					t.Fatal(err)
				}
				var sessions []*session
				for n := 0; n < viewers; n++ {
					ssctx, sscancel := context.WithCancel(ctx)
					ss := &session{ctx: ssctx, cancel: sscancel, ready: true, user: core.User{ID: int64(n + 1), IsOwner: true}, info: SessionInfo{ID: fmt.Sprintf("viewer-%d", n), Channel: channel, UserID: int64(n + 1)}, broadcast: &broadcastLease{broadcast: b, rendition: "shared", height: mode.height}, last: time.Now()}
					s.mu.Lock()
					s.sessions[ss.info.ID] = ss
					s.mu.Unlock()
					sessions = append(sessions, ss)
				}
				runtime.GC()
				started := time.Now()
				samples := 0
				var cpuSum, peak float64
				for time.Since(started) < 60*time.Second {
					select {
					case <-time.After(5 * time.Second):
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
					cpu, memory, err := broadcastResources(os.Getpid())
					if err != nil {
						t.Fatal(err)
					}
					samples++
					cpuSum += cpu
					if memory > peak {
						peak = memory
					}
					for _, ss := range sessions {
						playlist := httptest.NewRecorder()
						s.ServePlaylist(playlist, httptest.NewRequest("GET", "/index.m3u8", nil), ss.info.ID, &ss.user)
						if playlist.Code != 200 {
							t.Fatalf("viewer playlist %d", playlist.Code)
						}
						index, err := ss.broadcast.output()
						if err != nil {
							t.Fatal(err)
						}
						latest, ok := index.Latest()
						if !ok {
							t.Fatal("no media")
						}
						media := httptest.NewRecorder()
						s.ServeRelaySegment(media, httptest.NewRequest("GET", "/", nil), ss.info.ID, int(latest.Seq), &ss.user)
						if media.Code != 200 {
							t.Fatalf("viewer media %d", media.Code)
						}
					}
				}
				t.Logf("WHOLE_OUTPUT mode=%s viewers=%d seconds=%.2f cpu_cores=%.3f peak_RSS_MiB=%.1f samples=%d Go_host_and_ffmpeg_children=true", mode.name, viewers, time.Since(started).Seconds(), cpuSum/float64(samples), peak, samples)
				b.close()
			})
		}
	}
}
