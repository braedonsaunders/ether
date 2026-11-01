package transcode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

// Integration tests run the system ffmpeg against a lavfi-generated clip
// served by an httptest file server (with range support, like Ether's
// loopback source server). They skip when ffmpeg is absent or with -short.

type realEnv struct {
	ffmpeg, ffprobe string
	dir             string
	srv             *httptest.Server
	m               *Manager
}

func newRealEnv(t *testing.T, opts ...Option) *realEnv {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test (-short)")
	}
	ff, err1 := exec.LookPath("ffmpeg")
	fp, err2 := exec.LookPath("ffprobe")
	if err1 != nil || err2 != nil {
		t.Skip("ffmpeg/ffprobe not installed")
	}
	e := &realEnv{ffmpeg: ff, ffprobe: fp, dir: t.TempDir()}
	e.srv = httptest.NewServer(http.FileServer(http.Dir(e.dir)))
	t.Cleanup(e.srv.Close)
	all := append([]Option{WithSegmentTimeout(90 * time.Second), withPollInterval(50 * time.Millisecond)}, opts...)
	m, err := New(config.Transcode{FFmpegPath: ff, FFprobePath: fp, HWAccel: "auto", TempDir: filepath.Join(e.dir, "hls")},
		testLogger(t), all...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	e.m = m
	return e
}

func (e *realEnv) url(name string) string { return e.srv.URL + "/" + name }

// gen makes a clip with ffmpeg; args go between the lavfi inputs and the
// output file.
func (e *realEnv) gen(t *testing.T, name string, seconds int, args ...string) {
	t.Helper()
	full := []string{"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=640x360:rate=24",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000"}
	full = append(full, args...)
	// Interleave strictly by timestamp: under load the default 10s
	// max_interleave_delta lets one encoder fall behind and the fixture's
	// packet layout (which seeks depend on) then varies between runs.
	full = append(full, "-max_interleave_delta", "0", "-t", strconv.Itoa(seconds), filepath.Join(e.dir, name))
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute) // bounded under load
	defer cancel()
	if out, err := exec.CommandContext(ctx, e.ffmpeg, full...).CombinedOutput(); err != nil {
		t.Skipf("cannot generate %s (encoder missing?): %v: %s", name, err, out)
	}
}

type probedStream struct {
	CodecName      string `json:"codec_name"`
	CodecType      string `json:"codec_type"`
	CodecTag       string `json:"codec_tag_string"`
	Height         int    `json:"height"`
	StartTime      string `json:"start_time"`
	ColorPrimaries string `json:"color_primaries"`
	ColorTransfer  string `json:"color_transfer"`
	ColorSpace     string `json:"color_space"`
}

func (e *realEnv) probeFile(t *testing.T, path string) []probedStream {
	t.Helper()
	out, err := exec.Command(e.ffprobe, "-v", "error", "-print_format", "json", "-show_streams", path).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", path, err)
	}
	var v struct {
		Streams []probedStream `json:"streams"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatal(err)
	}
	return v.Streams
}

func fetch(t *testing.T, s core.HLSSession, n int) []byte {
	t.Helper()
	rec := httptest.NewRecorder()
	s.ServeSegment(rec, httptest.NewRequest(http.MethodGet, "/", nil), n)
	if rec.Code != 200 {
		if session, ok := s.(*Session); ok {
			session.mu.Lock()
			t.Logf("segment state: end=%d done=%v", session.endSeg, session.done)
			if run := session.cur; run != nil {
				t.Logf("run=%d start=%d maxDone=%d finished=%v stopped=%v stderr=%s", run.seq, run.startSeg, run.maxDone, run.finished, run.stopped, run.stderr.String())
			}
			session.mu.Unlock()
		}
		t.Fatalf("segment %d: HTTP %d %s", n, rec.Code, rec.Body.String())
	}
	return rec.Body.Bytes()
}

func (e *realEnv) writeTemp(t *testing.T, name string, b []byte) string {
	t.Helper()
	p := filepath.Join(e.dir, name)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func startTime(t *testing.T, ps []probedStream, typ string) float64 {
	t.Helper()
	for _, s := range ps {
		if s.CodecType == typ {
			f, err := strconv.ParseFloat(s.StartTime, 64)
			if err != nil {
				// "N/A": the stream has no packets in the probed data.
				t.Fatalf("%s stream has no start time (%q): no packets", typ, s.StartTime)
			}
			return f
		}
	}
	t.Fatalf("no %s stream", typ)
	return 0
}

func TestIntegrationRemuxSeekAndSwap(t *testing.T) {
	e := newRealEnv(t)
	e.gen(t, "a.mkv", 24, "-c:v", "libx264", "-preset", "ultrafast", "-g", "24", "-c:a", "aac", "-ac", "2")
	e.gen(t, "b.mkv", 24, "-c:v", "libx264", "-preset", "ultrafast", "-g", "24", "-c:a", "aac", "-ac", "2")
	ctx := context.Background()

	start := time.Now()
	info, err := e.m.Probe(ctx, e.url("a.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("probe took %v", time.Since(start))
	if info.Container != "mkv" || info.Duration < 23*time.Second || info.Video().Codec != "h264" || pickAudio(*info, -1).Codec != "aac" {
		t.Fatalf("probe %+v", info)
	}
	d := e.m.Decide(*info, plexWeb, allow())
	if d.Kind != core.DecisionRemux || !d.VideoCopy || d.Container != ContainerMPEGTS {
		t.Fatalf("decision %+v", d)
	}
	hs, err := e.m.StartHLS(ctx, core.HLSRequest{SessionID: "it-remux", InputURL: e.url("a.mkv"), Info: *info, Decision: d})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	hs.ServePlaylist(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if n := strings.Count(rec.Body.String(), "#EXTINF"); n != 6 {
		t.Errorf("playlist segments = %d:\n%s", n, rec.Body.String())
	}
	seg0 := fetch(t, hs, 0)
	if len(seg0) < 188 || seg0[0] != 0x47 {
		t.Fatalf("segment 0 is not MPEG-TS (%d bytes)", len(seg0))
	}
	// Seek: segment 4 must carry media time ~16s (+ the TS muxer's 1.4s).
	seg4 := fetch(t, hs, 4)
	ps := e.probeFile(t, e.writeTemp(t, "seg4.ts", seg4))
	if st := startTime(t, ps, "video"); math.Abs(st-17.4) > 1.1 {
		t.Errorf("segment 4 video start %.3f, want ~17.4", st)
	}
	// Failover to b.mkv at 8s: segment 2 comes from the new input.
	binfo, err := e.m.Probe(ctx, e.url("b.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	if err := hs.SwapInput(ctx, e.url("b.mkv"), *binfo, 8*time.Second); err != nil {
		t.Fatal(err)
	}
	seg2 := fetch(t, hs, 2)
	ps = e.probeFile(t, e.writeTemp(t, "seg2.ts", seg2))
	if st := startTime(t, ps, "video"); math.Abs(st-9.4) > 1.1 {
		t.Errorf("segment 2 after swap starts %.3f, want ~9.4", st)
	}
	e.m.Stop("it-remux")
	if _, ok := e.m.Session("it-remux"); ok {
		t.Error("session still live")
	}
}

// Plex tvOS resume starts the session at 0, then jumps to the resume
// segment. That first media GET must return under 5s on a local source
// (the cached-debrid bar).
func TestIntegrationRemuxResumeFirstFrame(t *testing.T) {
	e := newRealEnv(t, WithBuffering(24*time.Second, 2*time.Minute, 3*time.Minute))
	e.gen(t, "resume.mkv", 24, "-c:v", "libx264", "-preset", "ultrafast", "-g", "24", "-c:a", "aac", "-ac", "2")
	ctx := context.Background()
	info, err := e.m.Probe(ctx, e.url("resume.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	d := e.m.Decide(*info, plexWeb, allow())
	if d.Kind != core.DecisionRemux {
		t.Fatalf("decision %+v", d)
	}
	hs, err := e.m.StartHLS(ctx, core.HLSRequest{
		SessionID: "it-resume", InputURL: e.url("resume.mkv"), Info: *info, Decision: d,
	})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	seg := fetch(t, hs, 3)
	if took := time.Since(started); took > 5*time.Second {
		t.Fatalf("resume segment 3 took %v, want under 5s", took)
	}
	if len(seg) < 188 || seg[0] != 0x47 {
		t.Fatalf("segment 3 is not MPEG-TS (%d bytes)", len(seg))
	}
	e.m.Stop("it-resume")
}

func TestIntegrationTranscode(t *testing.T) {
	e := newRealEnv(t)
	e.gen(t, "t.mkv", 16, "-c:v", "libx264", "-preset", "ultrafast", "-g", "48", "-c:a", "aac", "-ac", "2")
	ctx := context.Background()
	info, err := e.m.Probe(ctx, e.url("t.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	o := allow()
	o.MaxHeight = 240
	d := e.m.Decide(*info, appleTV, o)
	if d.Kind != core.DecisionTranscode || d.MaxHeight != 240 {
		t.Fatalf("decision %+v", d)
	}
	hs, err := e.m.StartHLS(ctx, core.HLSRequest{SessionID: "it-tx", InputURL: e.url("t.mkv"), Info: *info, Decision: d})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("hwaccel %+v, session hw=%v", e.m.HWAccel(), hs.(*Session).HWAccelerated())
	for _, n := range []int{0, 1, 3} {
		seg := fetch(t, hs, n)
		ps := e.probeFile(t, e.writeTemp(t, "tx.ts", seg))
		var h int
		for _, s := range ps {
			if s.CodecType == "video" {
				h = s.Height
				if s.CodecName != "h264" {
					t.Errorf("codec %s", s.CodecName)
				}
			}
		}
		if h != 240 {
			t.Errorf("segment %d height %d", n, h)
		}
		if st := startTime(t, ps, "video"); math.Abs(st-(float64(n*4)+1.4)) > 0.6 {
			t.Errorf("segment %d starts at %.3f, want %.1f", n, st, float64(n*4)+1.4)
		}
	}
}

func TestIntegrationHEVCfMP4(t *testing.T) {
	e := newRealEnv(t, WithoutHWDetection())
	e.gen(t, "h.mkv", 12, "-c:v", "libx265", "-preset", "ultrafast", "-x265-params", "log-level=error:keyint=24",
		"-c:a", "aac", "-ac", "2")
	ctx := context.Background()
	info, err := e.m.Probe(ctx, e.url("h.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	d := e.m.Decide(*info, iOS, allow())
	if d.Kind != core.DecisionRemux || d.Container != ContainerFMP4 {
		t.Fatalf("decision %+v", d)
	}
	hs, err := e.m.StartHLS(ctx, core.HLSRequest{SessionID: "it-hevc", InputURL: e.url("h.mkv"), Info: *info, Decision: d})
	if err != nil {
		t.Fatal(err)
	}
	init := fetch(t, hs, InitSegment)
	seg := fetch(t, hs, 1)
	ps := e.probeFile(t, e.writeTemp(t, "h.mp4", append(append([]byte{}, init...), seg...)))
	found := false
	for _, s := range ps {
		if s.CodecType == "video" {
			found = true
			if s.CodecName != "hevc" || s.CodecTag != "hvc1" {
				t.Errorf("video %+v", s)
			}
		}
	}
	if !found {
		t.Error("no video in fMP4 segment")
	}
}

func TestIntegrationExtractSubtitle(t *testing.T) {
	e := newRealEnv(t, WithoutHWDetection())
	srt := e.writeTemp(t, "in.srt", []byte("1\n00:00:01,000 --> 00:00:02,500\nHello there\n\n2\n00:00:03,000 --> 00:00:04,000\nGeneral\n"))
	e.gen(t, "s.mkv", 5, "-i", srt, "-map", "0:v", "-map", "1:a", "-map", "2:s", "-c:v", "libx264", "-preset", "ultrafast",
		"-c:a", "aac", "-c:s", "srt")
	ctx := context.Background()
	info, err := e.m.Probe(ctx, e.url("s.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	var sub *core.StreamInfo
	for i := range info.Streams {
		if info.Streams[i].Type == "subtitle" {
			sub = &info.Streams[i]
		}
	}
	if sub == nil || sub.Codec != "subrip" {
		t.Fatalf("no subrip stream: %+v", info.Streams)
	}
	var out bytes.Buffer
	if err := e.m.ExtractSubtitle(ctx, e.url("s.mkv"), sub.Index, sub.Codec, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "WEBVTT") || !strings.Contains(out.String(), "Hello there") ||
		!strings.Contains(out.String(), " --> 00:02.5") {
		t.Errorf("vtt:\n%s", out.String())
	}
}

func TestIntegrationHWDetection(t *testing.T) {
	e := newRealEnv(t)
	hw := e.m.HWAccel()
	t.Logf("ffmpeg %s at %s, hwaccel %+v", e.m.Version(), e.m.FFmpegPath(), hw)
	if hw.Name == "" || len(hw.Encoders) == 0 && hw.Name != AccelNone {
		t.Errorf("hw %+v", hw)
	}
	if e.m.Version() == "" || e.m.Version() == "unknown" {
		t.Errorf("version %q", e.m.Version())
	}
}

func TestIntegrationSDRAndHDRWithoutAssumingZscale(t *testing.T) {
	e := newRealEnv(t)
	t.Logf("ffmpeg features path=%s version=%s %s probe=%s", e.m.FFmpegPath(), e.m.Version(), featureLine(e.m.feat), probeState(e.m.feat))
	if !e.m.feat.filtersProbed {
		t.Fatal("filter probe did not record a filter list")
	}
	e.gen(t, "dvd.mkv", 3, "-map", "0:v", "-map", "1:a",
		"-vf", "scale=720:480,setsar=32/27",
		"-c:v", "libx264", "-preset", "ultrafast",
		"-color_primaries", "smpte170m", "-color_trc", "bt709", "-colorspace", "smpte170m",
		"-c:a", "aac")
	ctx := context.Background()
	info, err := e.m.Probe(ctx, e.url("dvd.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	caps := appleTV
	caps.HLSContainers = []string{"mkv"}
	opts := allow()
	opts.DirectPlay, opts.DirectStream = false, false
	d := e.m.Decide(*info, caps, opts)
	if d.VideoCopy || d.Container != ContainerMatroska {
		t.Fatalf("decision %+v", d)
	}
	hs, err := e.m.StartHLS(ctx, core.HLSRequest{SessionID: "dvd-sdr", InputURL: e.url("dvd.mkv"), Info: *info, Decision: d})
	if err != nil {
		t.Fatalf("SDR transcode: %v", err)
	}
	defer hs.Close()
	graph := sessionGraph(hs.(*Session))
	if !e.m.feat.hasFilter("zscale") && strings.Contains(graph, "zscale") {
		t.Fatalf("zscale-less ffmpeg used zscale: %s", graph)
	}
	if e.m.feat.hasFilter("zscale") && !strings.Contains(graph, "zscale") {
		t.Fatalf("zscale ffmpeg did not use it: %s", graph)
	}
	if !e.m.feat.hasFilter("zscale") && !strings.Contains(graph, "colorspace=") && !strings.Contains(graph, "out_color_matrix=") {
		t.Fatalf("no SDR colour conversion: %s", graph)
	}
	saw := false
	for _, s := range probePlayable(t, e, hs, "dvd-out") {
		if s.CodecType != "video" {
			continue
		}
		saw = true
		if s.CodecName != "h264" {
			t.Errorf("video %+v", s)
		}
		if s.ColorPrimaries != "" && s.ColorPrimaries != "bt709" {
			t.Errorf("SDR output primaries %q, want bt709", s.ColorPrimaries)
		}
	}
	if !saw {
		t.Fatal("SDR transcode produced no video")
	}

	// FFmpeg 8 takes the encoder's colour from the frames. setparams tags
	// them PQ; output flags alone are dropped by jellyfin-ffmpeg 8.1.
	e.gen(t, "hdr.mkv", 3, "-map", "0:v", "-map", "1:a",
		"-vf", "setparams=color_primaries=bt2020:color_trc=smpte2084:colorspace=bt2020nc",
		"-c:v", "libx265", "-preset", "ultrafast", "-pix_fmt", "yuv420p10le",
		"-x265-params", "log-level=error:pools=1:frame-threads=1:keyint=24:colorprim=bt2020:transfer=smpte2084:colormatrix=bt2020nc",
		"-color_primaries", "bt2020", "-color_trc", "smpte2084", "-colorspace", "bt2020nc", "-c:a", "aac")
	hinfo, err := e.m.Probe(ctx, e.url("hdr.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	if dynamicRange(hinfo.Video()) != rangeHDR10 {
		t.Fatalf("generated clip not tagged HDR10: %+v", hinfo.Video())
	}
	hd := e.m.Decide(*hinfo, plexWeb, allow())
	hhs, err := e.m.StartHLS(ctx, core.HLSRequest{SessionID: "hdr-tonemap", InputURL: e.url("hdr.mkv"), Info: *hinfo, Decision: hd})
	if err != nil {
		var graphErr *GraphError
		if !errors.As(err, &graphErr) || !strings.Contains(err.Error(), "tone-map") || strings.Contains(err.Error(), "No such filter") {
			t.Fatalf("HDR start: %v", err)
		}
		if !strings.Contains(e.m.Capability().PlaybackError, "tone-map") && e.m.Capability().Notice == "" {
			t.Fatalf("no owner diagnostic: %+v", e.m.Capability())
		}
		return
	}
	defer hhs.Close()
	hgraph := sessionGraph(hhs.(*Session))
	if !e.m.feat.hasFilter("zscale") && strings.Contains(hgraph, "zscale") {
		t.Fatalf("HDR graph used zscale this ffmpeg does not have: %s", hgraph)
	}
	sawVideo := false
	for _, s := range probePlayable(t, e, hhs, "hdr-out") {
		if s.CodecType != "video" {
			continue
		}
		sawVideo = true
		if s.CodecName != "h264" {
			t.Errorf("video %+v", s)
		}
		if s.ColorTransfer == "smpte2084" || s.ColorPrimaries == "bt2020" {
			t.Errorf("HDR was not mapped to SDR: %+v", s)
		}
	}
	if !sawVideo {
		t.Fatal("HDR transcode produced no video")
	}
}

// probePlayable joins a Matroska init header to segment 0. Segment files
// from the static-header muxer are clusters, not standalone files.
func probePlayable(t *testing.T, e *realEnv, hs core.HLSSession, name string) []probedStream {
	t.Helper()
	body := fetch(t, hs, 0)
	ext := "ts"
	if s, ok := hs.(*Session); ok && hasInitSegment(s.container) {
		body = append(append([]byte(nil), fetch(t, hs, InitSegment)...), body...)
		ext = "mkv"
	}
	return e.probeFile(t, e.writeTemp(t, name+"."+ext, body))
}

func sessionGraph(s *Session) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur == nil {
		return ""
	}
	return filterGraph(buildArgs(s.cur.spec))
}

func TestIntegrationToneMap(t *testing.T) {
	for _, hw := range []bool{true, false} {
		name := "cpu"
		if hw {
			name = "hw"
		}
		t.Run(name, func(t *testing.T) {
			var opts []Option
			if !hw {
				opts = append(opts, WithoutHWDetection())
			}
			e := newRealEnv(t, opts...)
			e.gen(t, "hdr.mkv", 8, "-map", "0:v", "-map", "1:a", "-c:v", "libx265", "-preset", "ultrafast", "-pix_fmt", "yuv420p10le",
				"-x265-params", "log-level=error:keyint=24:colorprim=bt2020:transfer=smpte2084:colormatrix=bt2020nc",
				"-color_primaries", "bt2020", "-color_trc", "smpte2084", "-colorspace", "bt2020nc", "-c:a", "aac")
			ctx := context.Background()
			info, err := e.m.Probe(ctx, e.url("hdr.mkv"))
			if err != nil {
				t.Fatal(err)
			}
			if dynamicRange(info.Video()) != rangeHDR10 {
				t.Skipf("generated clip not tagged HDR10: %+v", info.Video())
			}
			o := allow()
			o.MaxHeight = 360
			d := e.m.Decide(*info, plexWeb, o)
			if !d.ToneMap || d.VideoCodec != "h264" {
				t.Fatalf("decision %+v", d)
			}
			hs, err := e.m.StartHLS(ctx, core.HLSRequest{SessionID: "tm-" + name, InputURL: e.url("hdr.mkv"), Info: *info, Decision: d})
			if err != nil {
				t.Fatal(err)
			}
			seg := fetch(t, hs, 1)
			ps := e.probeFile(t, e.writeTemp(t, "tm.ts", seg))
			for _, s := range ps {
				if s.CodecType == "video" && (s.CodecName != "h264" || s.Height != 360) {
					t.Errorf("video %+v", s)
				}
			}
			usedHW := hs.(*Session).HWAccelerated()
			t.Logf("hw session=%v", usedHW)
			if hw && e.m.HWAccel().Name == AccelVideoToolbox && e.m.feat.hasFilter("scale_vt") && !usedHW {
				t.Error("VideoToolbox tone-mapping fell back to CPU")
			}
		})
	}
}
