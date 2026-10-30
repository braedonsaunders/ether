package transcode

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

// Explicit lab opt-in. Ordinary go test remains offline and never provisions
// binaries or starts an RTX encode. The executable package is owner-installed.
func TestRTXNativeLifecycle(t *testing.T) {
	nv := os.Getenv("ETHER_RTX_TEST_EXECUTABLE")
	ff := os.Getenv("ETHER_RTX_TEST_FFMPEG")
	if runtime.GOOS != "windows" || nv == "" || ff == "" {
		t.Skip("native Windows RTX lab test requires explicit executable paths")
	}
	fp := filepath.Join(filepath.Dir(ff), "ffprobe.exe")
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	dir := t.TempDir()
	t.Cleanup(func() {
		if err := removeAllRetry(dir); err != nil {
			t.Errorf("lab cleanup: %v", err)
		}
	})
	input := filepath.Join(dir, "source.mp4")
	adaptive := os.Getenv("ETHER_RTX_TEST_ADAPTIVE") == "1"
	rate := "24"
	if os.Getenv("ETHER_RTX_TEST_FRACTIONAL") == "1" {
		rate = "24000/1001"
	}
	span := "20"
	if adaptive {
		span = "40"
	}
	args := []string{"-v", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=" + rate, "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-f", "lavfi", "-i", "sine=frequency=880:sample_rate=48000", "-map", "0:v", "-map", "1:a", "-map", "2:a", "-t", span, "-c:v", "h264_nvenc", "-g", "72", "-bf", "0", "-c:a", "aac", "-b:a", "128k", "-movflags", "+faststart", input}
	if out, err := exec.CommandContext(ctx, ff, args...).CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v %s", err, out)
	}
	srv := httptest.NewServer(http.FileServer(http.Dir(dir)))
	defer srv.Close()
	quality, _ := strconv.Atoi(os.Getenv("ETHER_RTX_TEST_QUALITY"))
	m, err := New(config.Transcode{ABRLadder: adaptive, ABRLadderUnderCopy: adaptive, FFmpegPath: ff, FFprobePath: fp, TempDir: filepath.Join(dir, "sessions"), RTXVideo: config.RTXVideo{Mode: "on", ExecutablePath: nv, Quality: quality}}, testLogger(t), WithoutHWDetection(), WithBuffering(24*time.Second, 0, 0), WithSegmentTimeout(60*time.Second), withPollInterval(20*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if support := m.EnhancementStatus(); !support.Available {
		t.Fatalf("detection: %+v", support)
	}
	info, err := m.Probe(ctx, srv.URL+"/source.mp4")
	if err != nil {
		t.Fatal(err)
	}
	// Fixture has exact three-second source GOPs; scan actual packets rather
	// than assuming the encoder honored that request.
	packetArgs := []string{"-v", "error", "-select_streams", "v:0", "-show_packets", "-show_entries", "packet=pts_time,flags", "-of", "json", input}
	out, err := exec.CommandContext(ctx, fp, packetArgs...).Output()
	if err != nil {
		t.Fatal(err)
	}
	var packets struct {
		Packets []struct {
			PTS   string `json:"pts_time"`
			Flags string `json:"flags"`
		}
	}
	if err := json.Unmarshal(out, &packets); err != nil {
		t.Fatal(err)
	}
	for _, p := range packets.Packets {
		if strings.Contains(p.Flags, "K") {
			f, _ := strconv.ParseFloat(p.PTS, 64)
			info.Keyframes = append(info.Keyframes, core.KeyframePoint{Time: time.Duration(f * float64(time.Second))})
		}
	}
	_, caps, opts := rtxFixture()
	opts.AudioStream = 2
	for _, proto := range []string{core.ProtocolHLS, "hls-fmp4", core.ProtocolDASH} {
		t.Run(proto, func(t *testing.T) {
			d := m.Decide(*info, caps, opts)
			if !enhancementSelected(d) {
				t.Fatalf("selection %+v", d)
			}
			transport := proto
			if proto == "hls-fmp4" {
				transport = core.ProtocolHLS
				d.Container = ContainerFMP4
			}
			req := core.HLSRequest{SessionID: proto, InputURL: srv.URL + "/source.mp4", Info: *info, Decision: d, Protocol: transport, SegmentDuration: 4 * time.Second}
			hs, err := m.StartHLS(ctx, req)
			if err != nil {
				t.Fatal(err)
			}
			s := hs.(*Session)
			defer s.Close()
			if proto == core.ProtocolDASH {
				rec := httptest.NewRecorder()
				s.ServeManifest(rec, httptest.NewRequest("GET", "/manifest.mpd", nil))
				if rec.Code != 200 {
					t.Fatalf("MPD %d %s", rec.Code, rec.Body.String())
				}
			}
			waitNative := func(seg int) {
				t.Helper()
				deadline := time.Now().Add(60 * time.Second)
				for time.Now().Before(deadline) {
					s.mu.Lock()
					done := s.done[0][seg]
					cur := s.cur
					confirmed := cur != nil && cur.rtx && cur.rtxConfirmed.Load()
					d := s.decision
					s.mu.Unlock()
					if d.Enhancement.State == "fallback" {
						t.Fatalf("native fallback: %+v", d)
					}
					if done && confirmed {
						return
					}
					time.Sleep(30 * time.Millisecond)
				}
				t.Fatal("native segment timed out")
			}
			waitNative(0)
			s.mu.Lock()
			first := s.trackSegPath(0, 0)
			init := s.initFile
			if s.dash != nil {
				init = ""
			}
			s.mu.Unlock()
			checkNativeSegment(t, ctx, fp, dir, s.dir, first, init, 0)
			if proto == core.ProtocolHLS {
				checkNativeAudio(t, ctx, ff, first)
			}
			if adaptive {
				for seg := 0; seg < 2; seg++ {
					rec := httptest.NewRecorder()
					s.ServeRungSegment(rec, httptest.NewRequest("GET", "/base-segment", nil), "base", seg)
					if rec.Code != 200 {
						t.Fatalf("base delivery %d: %d", seg, rec.Code)
					}
				}
				for i, id := range []string{"480p1500k", "360p800k", "base"} {
					seg := i + 2
					if id == "base" {
						// The fast native fixture may already have filled this
						// segment. Force a cache miss to prove a real recovery run.
						s.mu.Lock()
						delete(s.done[0], seg)
						_ = os.Remove(s.trackSegPath(0, seg))
						s.mu.Unlock()
					}
					rec := httptest.NewRecorder()
					started := time.Now()
					s.ServeRungSegment(rec, httptest.NewRequest("GET", "/segment", nil), id, seg)
					elapsed := time.Since(started)
					if rec.Code != 200 {
						t.Fatalf("switch %s: %d %s", id, rec.Code, rec.Body.String())
					}
					s.mu.Lock()
					track := s.rungTrack(id)
					path := s.trackSegPath(track, seg)
					init := ""
					if !s.isDASH() {
						init = s.initFileLocked(track)
					}
					rungIdx := track
					if s.isDASH() {
						rungIdx = rungForDashTrack(track)
					}
					rung := s.rungs[rungIdx]
					cur := s.cur
					audio := ""
					if s.isDASH() {
						audio = s.trackSegPath(s.audioTrackLocked(), seg)
					}
					s.mu.Unlock()
					if s.container == ContainerFMP4 && !s.isDASH() && init == "" {
						_ = filepath.WalkDir(s.dir, func(path string, _ os.DirEntry, err error) error {
							if err == nil {
								t.Logf("init diagnostic: %s", path)
							}
							return nil
						})
						t.Fatal("lower fMP4 init not published")
					}
					inspected := checkNativeSegmentSize(t, ctx, fp, dir, s.dir, path, init, float64(seg*4), rung.width, rung.height)
					if audio == "" {
						audio = inspected
					}
					checkNativeAudio(t, ctx, ff, audio)
					checkNativeAudioStart(t, ctx, fp, audio, float64(seg*4))
					if id != "base" {
						if cur.rtx || m.EnhancementStatus().Active != 0 || s.Decision().Enhancement.State != "fallback" {
							t.Fatalf("ordinary takeover: %+v", s.Decision())
						}
					} else if !cur.rtx || !cur.rtxConfirmed.Load() {
						t.Fatal("recovery was not genuine NGX")
					}
					t.Logf("native adaptive %s %s: %dx%d at %ds; IDR/GOP/audio aligned; request=%v", proto, id, rung.width, rung.height, seg*4, elapsed)
				}
				// An unsupported native seek must keep the established clock
				// and all ordinary representations while releasing VSR.
				noIndex := *info
				noIndex.Keyframes = nil
				if err := s.SwapInput(ctx, req.InputURL, noIndex, 32*time.Second); err != nil {
					t.Fatal(err)
				}
				checkOrdinary := func(s *Session, seg int, at float64) {
					t.Helper()
					rec := httptest.NewRecorder()
					s.ServeRungSegment(rec, httptest.NewRequest("GET", "/fallback", nil), "base", seg)
					if rec.Code != 200 {
						t.Fatalf("fallback media %d %s", rec.Code, rec.Body.String())
					}
					s.mu.Lock()
					path, init := s.trackSegPath(0, seg), ""
					if !s.isDASH() {
						init = s.initFileLocked(0)
					}
					audio := ""
					if s.isDASH() {
						audio = s.trackSegPath(s.audioTrackLocked(), seg)
					}
					for _, r := range s.rungs {
						if r.id == "480p1500k" && r.height != 480 {
							t.Errorf("fallback corrupted lower metadata: %+v", r)
						}
					}
					s.mu.Unlock()
					inspected := checkNativeSegmentSize(t, ctx, fp, dir, s.dir, path, init, at, 1280, 720)
					if audio == "" {
						audio = inspected
					}
					checkNativeAudio(t, ctx, ff, audio)
					checkNativeAudioStart(t, ctx, fp, audio, at)
					if s.Decision().Enhancement.State != "fallback" || m.EnhancementStatus().Active != 0 {
						t.Fatalf("ordinary fallback state %+v", s.Decision())
					}
				}
				checkOrdinary(s, 8, 32)
				checkOrdinary(s, 0, 0) // cached enhanced bytes must not survive the shape change
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				if m.EnhancementStatus().Active != 0 {
					t.Fatal("native permit leaked")
				}
				startupReq := req
				startupReq.SessionID += "-startup-fallback"
				startupReq.Info = noIndex
				startupReq.StartOffset = 4 * time.Second
				hs, err := m.StartHLS(ctx, startupReq)
				if err != nil {
					t.Fatal(err)
				}
				startup := hs.(*Session)
				defer startup.Close()
				seg := 0
				if startup.isDASH() {
					seg = 1
				}
				checkOrdinary(startup, seg, 4)
				if err := startup.Close(); err != nil {
					t.Fatal(err)
				}
				t.Logf("native adaptive %s: permanent32s and startup4s ordinary fallback clocks/audio/metadata verified; permits=0", proto)
				return
			}
			if rate != "24" {
				return
			}
			// 8s is not a source keyframe: this exercises seek to 6s + 48 frames
			// trimmed, and absolute mux timestamp restoration.
			if err := s.SwapInput(ctx, req.InputURL, *info, 8*time.Second); err != nil {
				t.Fatal(err)
			}
			waitNative(2)
			s.mu.Lock()
			second := s.trackSegPath(0, 2)
			init = s.initFile
			if s.dash != nil {
				init = ""
			}
			s.mu.Unlock()
			checkNativeSegment(t, ctx, fp, dir, s.dir, second, init, 8)
			if proto == core.ProtocolHLS {
				checkNativeAudio(t, ctx, ff, second)
			}
			t.Logf("native %s: genuine NGX output, 1920x1080, indexed seek/trim at 8s, exact GOP boundary", proto)
		})
	}
}

func checkNativeAudioStart(t *testing.T, ctx context.Context, probe, path string, start float64) {
	t.Helper()
	out, err := exec.CommandContext(ctx, probe, "-v", "error", "-select_streams", "a:0", "-show_packets", "-show_entries", "packet=pts_time", "-of", "json", path).Output()
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		Packets []struct {
			PTS string `json:"pts_time"`
		}
	}
	if err := json.Unmarshal(out, &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Packets) == 0 {
		t.Fatal("missing audio packets")
	}
	pts, err := strconv.ParseFloat(p.Packets[0].PTS, 64)
	if err != nil || math.Abs(pts-start) > 0.05 {
		t.Fatalf("audio starts %s, want %.3f", p.Packets[0].PTS, start)
	}
}
func checkNativeSegment(t *testing.T, ctx context.Context, probe, dir, sessionDir, path, init string, start float64) {
	checkNativeSegmentSize(t, ctx, probe, dir, sessionDir, path, init, start, 1920, 1080)
}

func checkNativeSegmentSize(t *testing.T, ctx context.Context, probe, dir, sessionDir, path, init string, start float64, width, height int) string {
	t.Helper()
	if init != "" {
		initPath := init
		if !filepath.IsAbs(initPath) {
			initPath = filepath.Join(sessionDir, init)
		}
		b, e := os.ReadFile(initPath)
		if e != nil {
			t.Fatal(e)
		}
		p, e := os.ReadFile(path)
		if e != nil {
			t.Fatal(e)
		}
		path = filepath.Join(dir, "inspect-"+strconv.FormatFloat(start, 'f', 0, 64)+".mp4")
		if e := os.WriteFile(path, append(b, p...), 0600); e != nil {
			t.Fatal(e)
		}
	}
	args := []string{"-v", "error", "-select_streams", "v:0", "-show_streams", "-show_packets", "-show_entries", "stream=width,height:packet=pts_time,flags", "-of", "json", path}
	out, err := exec.CommandContext(ctx, probe, args...).Output()
	if err != nil {
		t.Fatalf("probe %s: %v", path, err)
	}
	var p struct {
		Streams []struct{ Width, Height int }
		Packets []struct {
			PTS   string `json:"pts_time"`
			Flags string `json:"flags"`
		}
	}
	if err := json.Unmarshal(out, &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Streams) != 1 || p.Streams[0].Width != width || p.Streams[0].Height != height || len(p.Packets) == 0 {
		t.Fatalf("bad enhanced segment: %s", out)
	}
	pts, _ := strconv.ParseFloat(p.Packets[0].PTS, 64)
	if math.Abs(pts-start) > 1.0/24+0.002 || !strings.Contains(p.Packets[0].Flags, "K") {
		t.Fatalf("boundary pts=%f want=%f flags=%s", pts, start, p.Packets[0].Flags)
	}
	last := pts
	for _, packet := range p.Packets {
		v, err := strconv.ParseFloat(packet.PTS, 64)
		if err != nil {
			t.Fatal(err)
		}
		last = math.Max(last, v) // ordinary encoders may reorder B frames
	}
	if math.Abs(last+1.0/24-start-4) > 0.006 {
		t.Fatalf("GOP duration mismatch start=%f last=%f", pts, last)
	}
	return path
}

// Two known fixture tones establish selected-track preservation through a seek.
func checkNativeAudio(t *testing.T, ctx context.Context, ff, path string) {
	t.Helper()
	out, err := exec.CommandContext(ctx, ff, "-v", "error", "-i", path, "-map", "0:a:0", "-t", "0.5", "-ac", "1", "-ar", "8000", "-f", "s16le", "-").Output()
	if err != nil {
		t.Fatal(err)
	}
	crossings := 0
	prev := int16(0)
	for i := 0; i+1 < len(out); i += 2 {
		v := int16(binary.LittleEndian.Uint16(out[i : i+2]))
		if v > 0 && prev <= 0 {
			crossings++
		}
		prev = v
	}
	if crossings < 400 || crossings > 480 {
		t.Fatalf("wrong audio track: %d positive crossings, expected 880Hz selected track", crossings)
	}
}
