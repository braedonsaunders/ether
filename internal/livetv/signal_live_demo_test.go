package livetv

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	_ "image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/mediaproc"
	"github.com/braedonsaunders/ether/internal/onair/families"
	"github.com/braedonsaunders/ether/internal/onair/raster"
	"github.com/braedonsaunders/ether/internal/onair/signal"
	"github.com/braedonsaunders/ether/internal/transcode"
)

var liveSignalDemo = flag.String("live-signal-demo", "", "capture owner demos from the real switcher and signed shared HLS")
var liveSignalPlates = flag.String("live-signal-plates", "", "plate directory override")
var liveSignalNotify = flag.String("live-signal-notify", "", "notify requesting coordinator after each file")

type liveSignalDecoder struct {
	engine   *signal.Engine
	overlay  *videoOverlay
	format   transcode.RawFormat
	tick     int64
	origin   time.Time
	closed   chan struct{}
	once     sync.Once
	renderNS atomic.Int64
}

func (d *liveSignalDecoder) Next(ctx context.Context) (transcode.RawFrame, error) {
	deadline := d.origin.Add(time.Duration(d.tick) * time.Second / 30)
	timer := time.NewTimer(max(0, time.Until(deadline)))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return transcode.RawFrame{}, ctx.Err()
	case <-d.closed:
		return transcode.RawFrame{}, io.EOF
	case <-timer.C:
	}
	frame := transcode.RawFrame{Video: make([]byte, d.format.Width*d.format.Height*3/2), PCM: make([]byte, 6400)}
	var pcm [3200]int16
	start := time.Now()
	d.engine.Render(frame.Video, pcm[:], d.tick)
	d.overlay.blend(frame.Video, d.format, 255)
	d.renderNS.Add(time.Since(start).Nanoseconds())
	for i, v := range pcm {
		binary.LittleEndian.PutUint16(frame.PCM[i*2:], uint16(v))
	}
	d.tick++
	return frame, nil
}
func (d *liveSignalDecoder) Close() error { d.once.Do(func() { close(d.closed) }); return nil }

type liveSignalPlanner struct{ plan broadcastPlan }

func (p liveSignalPlanner) plans(time.Time) (broadcastPlan, *broadcastPlan) { return p.plan, nil }
func (p liveSignalPlanner) screen(time.Time) broadcastPlan                  { return broadcastPlan{} }

type liveSignalTimedRunner struct{ report string }

func (r liveSignalTimedRunner) Start(c transcode.Command) (transcode.Process, error) {
	options := []string{"-l", "-o", r.report}
	if runtime.GOOS == "linux" {
		options = []string{"-v", "-o", r.report}
	}
	c.Args = append(append(options, c.Path), c.Args...)
	c.Path = "/usr/bin/time"
	p, err := (transcode.ExecRunner{}).Start(c)
	if err != nil {
		return nil, err
	}
	return liveSignalTimedProcess{Process: p}, nil
}

type liveSignalTimedProcess struct{ transcode.Process }

func (p liveSignalTimedProcess) Stop() {
	select {
	case <-p.Done():
	case <-time.After(2 * time.Second):
	}
	p.Process.Stop()
}
func (r liveSignalTimedRunner) Run(ctx context.Context, c transcode.Command) error {
	return (transcode.ExecRunner{}).Run(ctx, c)
}
func liveSignalCPU(b []byte) float64 {
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) >= 6 && f[1] == "real" {
			u, _ := strconv.ParseFloat(f[2], 64)
			s, _ := strconv.ParseFloat(f[4], 64)
			return (u + s) / 20
		}
	}
	return 0
}

func TestLiveSignalDemo(t *testing.T) {
	if *liveSignalDemo == "" {
		t.Skip("opt-in owner demo")
	}
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip(err)
	}
	fp, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip(err)
	}
	os.MkdirAll(*liveSignalDemo, 0755)
	type spec struct{ name, style, overlay string }
	specs := []spec{{"static", "crisp", ""}, {"nocturne", "crisp", ""}, {"bars", "bars", ""}, {"card-back-at-6pm", "card", "back-at-6pm"}, {"card-reconnecting", "card", "reconnecting"}, {"card-waiting", "card", "waiting"}, {"brb", "brb", ""}, {"testcard", "testcard", ""}}
	for _, s := range specs {
		for _, h := range []int{1080, 720} {
			t.Run(fmt.Sprintf("%s-%d", s.name, h), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 70*time.Second)
				defer cancel()
				w := h * 16 / 9
				suffix := ""
				ceiling := 5000
				if h == 720 {
					suffix = "-720p"
					ceiling = 2000
				}
				name := s.name + suffix
				load := func(base string) image.Image {
					plateDir := *liveSignalPlates
					if plateDir == "" {
						plateDir = filepath.Join(*liveSignalDemo, "plates")
					}
					path := filepath.Join(plateDir, fmt.Sprintf("%s-%dx%d.png", base, w, h))
					file, err := os.Open(path)
					if err != nil {
						t.Fatal(err)
					}
					defer file.Close()
					im, _, err := image.Decode(file)
					if err != nil {
						t.Fatal(err)
					}
					return im
				}
				var engine *signal.Engine
				var foreground *videoOverlay
				if strings.HasPrefix(s.name, "static") || s.name == "nocturne" {
					engine = signal.NewStatic(w, h, 20261105)
					if s.name == "nocturne" {
						doc, _ := families.Get("late-night", "offair")
						plate, _ := doc.StaticForeground()
						foreground = compactOverlay(raster.Render(plate, raster.Bindings{Bindings: families.Defaults("late-night")}, doc.Poster, w, h), transcode.RawFormat{Width: w, Height: h})
					}
				} else {
					overlay := "screen-" + s.style + "-text"
					if s.style == "card" {
						overlay = "screen-card-" + s.overlay + "-text"
					}
					engine = signal.NewPlate(w, h, s.style, load("screen-"+s.style+"-plate"), load(overlay), 20261105)
				}
				report := filepath.Join(*liveSignalDemo, name+"-encoder-cpu.txt")
				manager, err := transcode.New(config.Transcode{FFmpegPath: ff, FFprobePath: fp, TempDir: filepath.Join(t.TempDir(), "out"), HWAccel: "none"}, slog.New(slog.NewTextHandler(os.Stderr, nil)), transcode.WithoutHWDetection(), transcode.WithRunner(liveSignalTimedRunner{report: report}))
				if err != nil {
					t.Fatal(err)
				}
				defer manager.Close()
				format := transcode.RawFormat{Width: w, Height: h, FPSNum: 30, FPSDen: 1, SampleRate: 48000, Channels: 2, PixelFormat: "yuv420p"}
				decoder := &liveSignalDecoder{engine: engine, overlay: foreground, format: format, origin: time.Now(), closed: make(chan struct{})}
				plan := broadcastPlan{id: "live-" + name, open: func(context.Context) (transcode.Decoder, func(), error) { return decoder, nil, nil }}
				b, err := newBroadcast(ctx, manager, liveSignalPlanner{plan: plan}, format)
				if err != nil {
					t.Fatal(err)
				}
				b.logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
				defer b.close()
				if _, err := b.addRendition(ctx, "signal", core.Decision{Kind: core.DecisionTranscode, VideoCodec: "h264", AudioCodec: "aac", MaxHeight: h, BitrateKbps: ceiling, AudioBitrateKbps: 192, AudioChannels: 2, AudioSampleRate: 48000, H264Level: 40}); err != nil {
					t.Fatal(err)
				}
				service := testService(t, Options{Transcoder: manager})
				channel := testChannel()
				channel.Name = "Nocturne 7"
				channel.Items = []Item{{Key: 1, Title: name, DurationMS: 3600000}}
				if _, err := service.SaveChannel(ctx, channel); err != nil {
					t.Fatal(err)
				}
				user := core.User{ID: 1, IsOwner: true}
				ssctx, sscancel := context.WithCancel(ctx)
				ss := &session{ctx: ssctx, cancel: sscancel, ready: true, user: user, last: time.Now(), streamGrant: "signal-demo-stream-grant", info: SessionInfo{ID: "signal-demo", Channel: channel, UserID: 1}, broadcast: &broadcastLease{broadcast: b, rendition: "signal", height: h}}
				service.mu.Lock()
				service.sessions[ss.info.ID] = ss
				service.mu.Unlock()
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if strings.HasSuffix(r.URL.Path, "index.m3u8") {
						service.ServePlaylist(w, r, ss.info.ID, &user)
						return
					}
					seq, err := strconv.Atoi(strings.TrimSuffix(filepath.Base(r.URL.Path), ".ts"))
					if err != nil {
						http.NotFound(w, r)
						return
					}
					service.ServeRelaySegment(w, r, ss.info.ID, seq, &user)
				}))
				defer server.Close()
				b.origin = time.Now()
				decoder.origin = b.origin
				b.start()
				capture := filepath.Join(*liveSignalDemo, "."+name+".ts")
				file, err := os.Create(capture)
				if err != nil {
					t.Fatal(err)
				}
				defer file.Close()
				next := int64(0)
				firstSegment := 0.0
				started := time.Now()
				for next < 7 {
					req, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/livetv/sessions/signal-demo/0/index.m3u8", nil)
					res, err := server.Client().Do(req)
					if err != nil {
						t.Fatal(err)
					}
					body, _ := io.ReadAll(res.Body)
					res.Body.Close()
					if res.StatusCode != 200 {
						if next == 0 && time.Since(started) < 15*time.Second {
							select {
							case <-ctx.Done():
								t.Fatal(ctx.Err())
							case <-time.After(100 * time.Millisecond):
							}
							continue
						}
						t.Fatalf("playlist%d %s", res.StatusCode, body)
					}
					if strings.Contains(string(body), "DISCONTINUITY") {
						t.Fatal("discontinuity")
					}
					for _, line := range strings.Split(string(body), "\n") {
						line = strings.TrimSpace(line)
						if line == "" || strings.HasPrefix(line, "#") {
							continue
						}
						path, _, _ := strings.Cut(line, "?")
						seq, err := strconv.ParseInt(strings.TrimSuffix(filepath.Base(path), ".ts"), 10, 64)
						if err != nil {
							t.Fatal(err)
						}
						if seq < next {
							continue
						}
						if seq != next {
							t.Fatal("lost shared segment")
						}
						ref, _ := url.Parse(line)
						mediaReq, _ := http.NewRequestWithContext(ctx, "GET", req.URL.ResolveReference(ref).String(), nil)
						media, err := server.Client().Do(mediaReq)
						if err != nil {
							t.Fatal(err)
						}
						if media.StatusCode != 200 {
							media.Body.Close()
							t.Fatalf("segment%d status%d", seq, media.StatusCode)
						}
						_, err = io.Copy(file, media.Body)
						media.Body.Close()
						if err != nil {
							t.Fatal(err)
						}
						next++
						if firstSegment == 0 {
							firstSegment = time.Since(started).Seconds()
						}
						if next == 7 {
							break
						}
					}
					if next < 7 {
						select {
						case <-ctx.Done():
							t.Fatal(ctx.Err())
						case <-time.After(100 * time.Millisecond):
						}
					}
				}
				file.Close()
				b.close()
				manager.Close()
				output := filepath.Join(*liveSignalDemo, name+".mp4")
				if out, err := mediaproc.CommandContext(ctx, 2*time.Minute, ff, "-v", "error", "-y", "-i", capture, "-t", "20", "-c", "copy", "-movflags", "+faststart", output).CombinedOutput(); err != nil {
					t.Fatalf("remux: %v %s", err, out)
				}
				if out, err := mediaproc.CommandContext(ctx, 2*time.Minute, ff, "-v", "error", "-xerror", "-i", output, "-f", "null", "-").CombinedOutput(); err != nil {
					t.Fatalf("decode: %v %s", err, out)
				}
				raw, err := mediaproc.CommandContext(ctx, 2*time.Minute, fp, "-v", "error", "-show_packets", "-show_format", "-show_entries", "packet=pts_time,size:format=duration,bit_rate", "-of", "json", output).Output()
				if err != nil {
					t.Fatal(err)
				}
				var data struct {
					Packets []struct {
						PTS  string `json:"pts_time"`
						Size string
					}
					Format struct {
						Duration string
						BitRate  string `json:"bit_rate"`
					}
				}
				json.Unmarshal(raw, &data)
				type timedPacket struct {
					at   float64
					bits int
				}
				packets := make([]timedPacket, 0, len(data.Packets))
				for _, p := range data.Packets {
					at, _ := strconv.ParseFloat(p.PTS, 64)
					size, _ := strconv.Atoi(p.Size)
					packets = append(packets, timedPacket{at: at, bits: size * 8})
				}
				sort.Slice(packets, func(i, j int) bool { return packets[i].at < packets[j].at })
				peak, sum, left := 0, 0, 0
				for right, p := range packets {
					sum += p.bits
					for left < right && p.at-packets[left].at >= 1 {
						sum -= packets[left].bits
						left++
					}
					peak = max(peak, sum)
				}

				duration, _ := strconv.ParseFloat(data.Format.Duration, 64)
				bitrate, _ := strconv.ParseFloat(data.Format.BitRate, 64)
				means := inspectSignalSample(t, ctx, ff, output, *liveSignalDemo, name)
				stats, _ := os.ReadFile(report)
				encodeCPU := liveSignalCPU(stats)
				generationCPU := float64(decoder.renderNS.Load()) / float64(time.Second) / 20
				metadata := map[string]any{"caption": "Live " + name + ": Signal engine -> real Go switcher -> shared signed HLS -> copy-remux MP4", "variant": s.style, "resolution": fmt.Sprintf("%dx%d", w, h), "bitrate_kbps": bitrate / 1000, "peak_1s_bitrate_kbps": float64(peak) / 1000, "session_ceiling_kbps": ceiling, "duration_s": duration, "generation_plus_encode_cpu_cores": generationCPU + encodeCPU, "generation_walltime_cores_upper_bound": generationCPU, "encoder_cpu_cores": encodeCPU, "cold_first_segment_s": firstSegment, "mean_luma_2_8_14s": means}
				if peak > ceiling*1000 {
					t.Fatalf("ceiling exceeded %d", peak)
				}
				bytes, _ := json.MarshalIndent(metadata, "", "  ")
				os.WriteFile(filepath.Join(*liveSignalDemo, name+".json"), append(bytes, '\n'), 0644)
				crop := filepath.Join(*liveSignalDemo, name+"-crop.jpg")
				mediaproc.CommandContext(ctx, 2*time.Minute, ff, "-v", "error", "-y", "-ss", "5", "-i", output, "-vf", fmt.Sprintf("crop=%d:%d:%d:%d", w/2, h/2, w/4, h/4), "-frames:v", "1", crop).Run()
				os.Remove(capture)
				if false && *liveSignalNotify != "" {
					message := fmt.Sprintf("LANDED LIVE %s.mp4 +json+crop in %s. Real switcher/shared HLS, avg%.1f peak1s%.1f kb/s ceiling%d; generation+encode %.3f cores; -xerror decode passed.", name, *liveSignalDemo, bitrate/1000, float64(peak)/1000, ceiling, generationCPU+encodeCPU)
					exec.CommandContext(ctx, "bb", "thread", "tell", *liveSignalNotify, message).Run()
				}
				t.Logf("LANDED %s %s", output, bytes)
			})
		}
	}
}

func inspectSignalSample(t *testing.T, ctx context.Context, ff, output, dir, name string) []float64 {
	t.Helper()
	var means []float64
	var hashes []uint64
	for _, second := range []string{"2", "8", "14"} {
		raw, err := mediaproc.CommandContext(ctx, 2*time.Minute, ff, "-v", "error", "-ss", second, "-i", output, "-frames:v", "1", "-pix_fmt", "gray", "-f", "rawvideo", "-").Output()
		if err != nil {
			t.Fatal(err)
		}
		var total uint64
		hash := uint64(1469598103934665603)
		for _, v := range raw {
			total += uint64(v)
			hash = (hash ^ uint64(v)) * 1099511628211
		}
		mean := float64(total) / float64(len(raw))
		if mean < 2 {
			t.Fatalf("black output at %ss: mean=%f", second, mean)
		}
		means = append(means, mean)
		hashes = append(hashes, hash)
	}
	if hashes[0] == hashes[1] && hashes[1] == hashes[2] {
		t.Fatal("frozen live output")
	}
	preview := filepath.Join(dir, name+"-inspection.jpg")
	if out, err := mediaproc.CommandContext(ctx, 2*time.Minute, ff, "-v", "error", "-y", "-i", output, "-vf", `select='eq(n,60)+eq(n,240)+eq(n,420)',scale=640:360,tile=3x1`, "-frames:v", "1", preview).CombinedOutput(); err != nil {
		t.Fatalf("inspection: %v %s", err, out)
	}
	return means
}
