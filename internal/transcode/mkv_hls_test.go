package transcode

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

func TestHLSTargetIsSeparateFromDirectPlayContainers(t *testing.T) {
	for _, tc := range []struct {
		targets []string
		want    string
	}{{nil, ContainerFMP4}, {[]string{"mkv"}, ContainerMatroska}, {[]string{"mp4"}, ContainerFMP4}} {
		caps := appleTV
		caps.Containers = []string{"mkv", "mp4", "mpegts"}
		caps.HLSContainers = tc.targets
		opts := allow()
		opts.DirectPlay = false
		d := Decide(mustParse(t, "mkv_hevc_hdr10.json"), caps, opts, Policy{})
		if d.Container != tc.want || !d.VideoCopy {
			t.Fatalf("targets %v: %+v", tc.targets, d)
		}
	}
}

func TestIntegrationMatroskaCopyRampNeverReplaysEarlierKeyframes(t *testing.T) {
	e := newRealEnv(t, WithoutHWDetection())
	e.m.UpdateConfig(config.Transcode{FastStart: true, FastStartSegments: 2})
	// Eight-second GOPs cannot satisfy a two-second copy ramp. The old
	// nominal 4s restart sought backwards and replayed the opening frames.
	e.gen(t, "long-gop.mkv", 24, "-c:v", "libx264", "-preset", "ultrafast", "-g", "192", "-keyint_min", "192", "-sc_threshold", "0", "-c:a", "aac")
	info, err := e.m.Probe(t.Context(), e.url("long-gop.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	d := core.Decision{Kind: core.DecisionRemux, Container: ContainerMatroska, VideoCopy: true, AudioCopy: true, AudioStream: 1}
	hs, err := e.m.StartHLS(t.Context(), core.HLSRequest{SessionID: "long-gop", InputURL: e.url("long-gop.mkv"), Info: *info, Decision: d})
	if err != nil {
		t.Fatal(err)
	}
	joined := fetch(t, hs, InitSegment)
	for n := 0; n < 3; n++ {
		joined = append(joined, fetch(t, hs, n)...)
	}
	file := e.writeTemp(t, "long-gop-joined.mkv", joined)
	out, err := exec.Command(e.ffprobe, "-v", "error", "-select_streams", "v:0", "-show_entries", "frame=best_effort_timestamp_time", "-of", "json", file).Output()
	if err != nil {
		t.Fatal(err)
	}
	var frames struct {
		Frames []struct {
			Time string `json:"best_effort_timestamp_time"`
		} `json:"frames"`
	}
	if err := json.Unmarshal(out, &frames); err != nil {
		t.Fatal(err)
	}
	// Three 4s segments of 24fps video: segments hold what the playlist
	// says (a copy cut by time), not ffmpeg's 8s keyframe-bound cuts.
	if len(frames.Frames) < 3*4*24-2 {
		t.Fatalf("only decoded %d frames across three segments", len(frames.Frames))
	}
	last := -1.0
	for _, frame := range frames.Frames {
		at, err := strconv.ParseFloat(frame.Time, 64)
		if err != nil {
			t.Fatal(err)
		}
		if at < last {
			t.Fatalf("copied media jumped backwards: %.3f -> %.3f", last, at)
		}
		last = at
	}
}

func TestMatroskaHLSArgsAndPlaylist(t *testing.T) {
	d := core.Decision{Kind: core.DecisionRemux, Container: ContainerMatroska, VideoCopy: true, AudioCopy: true, AudioStream: 1}
	s := baseSpec(mustParse(t, "mkv_hevc_hdr10.json"), d)
	args := buildArgs(s)
	mustContain(t, args, "-f ssegment", "-segment_format matroska", "-individual_header_trailer 0", "-segment_header_filename", "-reset_timestamps 0", "-segment_start_number 50", "-segment_list_type m3u8", "-segment_format_options output_ts_offset=210.000", "%05d.mkv")
	mustNotContain(t, args, "-copyts", "-start_at_zero", "-hls_segment_type", "-tag:v hvc1", "-hls_fmp4_init_filename")
	b := string(mediaPlaylist(20*time.Second, 4*time.Second, ContainerMatroska, func(n int) string { return SegmentName(n, ContainerMatroska) }))
	if !strings.Contains(b, `#EXT-X-MAP:URI="header"`) || !strings.Contains(b, "00000.mkv") || !strings.Contains(b, "#EXT-X-VERSION:7") {
		t.Fatal(b)
	}
	if n, ok := ParseSegmentName("00050.mkv"); !ok || n != 50 {
		t.Fatalf("segment name: %d %v", n, ok)
	}
	if n, ok := ParseSegmentName("header"); !ok || n != InitSegment {
		t.Fatalf("header route: %d %v", n, ok)
	}
}

func TestIntegrationMatroskaHLSDecodeSeekAndSwap(t *testing.T) {
	e := newRealEnv(t)
	e.gen(t, "mkv-a.mkv", 24, "-c:v", "libx265", "-preset", "ultrafast", "-x265-params", "pools=1:frame-threads=1:keyint=24:min-keyint=24:scenecut=0", "-c:a", "eac3", "-ac", "2")
	e.gen(t, "mkv-b.mkv", 24, "-c:v", "libx265", "-preset", "ultrafast", "-x265-params", "pools=1:frame-threads=1:keyint=24:min-keyint=24:scenecut=0", "-c:a", "eac3", "-ac", "2")
	ctx := context.Background()
	info, err := e.m.Probe(ctx, e.url("mkv-a.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	caps := appleTV
	caps.HLSContainers = []string{"mkv"}
	opts := allow()
	opts.DirectPlay = false
	d := e.m.Decide(*info, caps, opts)
	if d.Container != ContainerMatroska || !d.VideoCopy || !d.AudioCopy {
		t.Fatalf("decision: %+v", d)
	}
	hs, err := e.m.StartHLS(ctx, core.HLSRequest{SessionID: "it-matroska", InputURL: e.url("mkv-a.mkv"), Info: *info, Decision: d})
	if err != nil {
		t.Fatal(err)
	}
	decode := func(n int, name string, wantStart float64) {
		t.Helper()
		r := httptest.NewRecorder()
		hs.ServeSegment(r, httptest.NewRequest(http.MethodGet, "/", nil), n)
		if r.Code != 200 || r.Header().Get("Content-Type") != "video/x-matroska" {
			t.Fatalf("segment %d: %d %s", n, r.Code, r.Body.String())
		}
		b := r.Body.Bytes()
		if len(b) < 4 || string(b[:4]) != "\x1f\x43\xb6\x75" {
			t.Fatalf("segment %d is not a Matroska Cluster", n)
		}
		header := fetch(t, hs, InitSegment)
		if !bytes.HasPrefix(header, []byte{0x1a, 0x45, 0xdf, 0xa3}) {
			t.Fatal("initialization resource lacks EBML header")
		}
		path := e.writeTemp(t, name, append(header, b...))
		streams := e.probeFile(t, path)
		if got := startTime(t, streams, "video"); math.Abs(got-wantStart) > 1.1 {
			t.Fatalf("segment %d starts %.3f, want %.3f", n, got, wantStart)
		}
		if got := startTime(t, streams, "audio"); math.Abs(got-wantStart) > 1.1 {
			t.Fatalf("segment %d audio starts %.3f, want %.3f", n, got, wantStart)
		}
		if out, err := exec.Command(e.ffmpeg, "-v", "error", "-i", path, "-map", "0:v:0", "-frames:v", "3", "-f", "null", "-").CombinedOutput(); err != nil {
			t.Fatalf("decode segment %d: %v %s", n, err, out)
		}
		if out, err := exec.Command(e.ffmpeg, "-v", "error", "-i", path, "-map", "0:a:0", "-frames:a", "3", "-f", "null", "-").CombinedOutput(); err != nil {
			t.Fatalf("decode audio segment %d: %v %s", n, err, out)
		}
	}
	decode(0, "first.mkv", 10)
	// Native Plex joins EXT-X-MAP and successive fragments into one demuxer
	// input. Decode across boundaries, rather than only the first few frames.
	joined := fetch(t, hs, InitSegment)
	for n := 0; n < 3; n++ {
		joined = append(joined, fetch(t, hs, n)...)
	}
	path := e.writeTemp(t, "continuous.mkv", joined)
	if out, err := exec.Command(e.ffmpeg, "-v", "warning", "-xerror", "-i", path, "-map", "0:v:0", "-f", "null", "-").CombinedOutput(); err != nil || bytes.Contains(out, []byte("File extends beyond end of segment")) || bytes.Contains(out, []byte("EBML number")) {
		t.Fatalf("continuous Matroska decode: %v %s", err, out)
	}
	decode(4, "seek.mkv", 26)
	other, err := e.m.Probe(ctx, e.url("mkv-b.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	if err := hs.SwapInput(ctx, e.url("mkv-b.mkv"), *other, 8*time.Second); err != nil {
		t.Fatal(err)
	}
	decode(2, "swap.mkv", 18)
}

func TestIntegrationMatroskaTailResumeAudioOutlastsVideo(t *testing.T) {
	e := newRealEnv(t, WithoutHWDetection())
	outPath := filepath.Join(e.dir, "tail-audio.mkv")
	cmd := exec.CommandContext(t.Context(), e.ffmpeg,
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-t", "16", "-i", "testsrc2=size=640x360:rate=24",
		"-f", "lavfi", "-t", "24", "-i", "sine=frequency=440:sample_rate=48000",
		"-c:v", "libx265", "-preset", "ultrafast", "-x265-params", "pools=1:frame-threads=1:keyint=24:min-keyint=24:scenecut=0",
		"-c:a", "eac3", "-ac", "2",
		"-max_interleave_delta", "0",
		outPath,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("cannot generate tail-audio.mkv: %v: %s", err, out)
	}
	info, err := e.m.Probe(t.Context(), e.url("tail-audio.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	d := core.Decision{Kind: core.DecisionRemux, Container: ContainerMatroska, VideoCopy: true, AudioCopy: true, AudioStream: 1}
	hs, err := e.m.StartHLS(t.Context(), core.HLSRequest{
		SessionID: "tail-audio",
		InputURL:  e.url("tail-audio.mkv"),
		Info:      *info,
		Decision:  d,
	})
	if err != nil {
		t.Fatal(err)
	}
	fetch(t, hs, 0)
	if err := hs.SwapInput(t.Context(), e.url("tail-audio.mkv"), *info, 8*time.Second); err != nil {
		t.Fatal(err)
	}
	// Resume near the end of the video stream (segment 2, 8s) and walk through
	// the trailing audio-only segments (segments 4 and 5, 16..24s) to the end.
	joined := fetch(t, hs, InitSegment)
	for n := 2; n < 6; n++ {
		b := fetch(t, hs, n)
		if len(b) < 4 || string(b[:4]) != "\x1f\x43\xb6\x75" {
			t.Fatalf("segment %d is not a Matroska Cluster", n)
		}
		joined = append(joined, b...)
	}
	path := e.writeTemp(t, "tail-resume.mkv", joined)
	streams := e.probeFile(t, path)
	if got := startTime(t, streams, "video"); math.Abs(got-18) > 1.1 {
		t.Fatalf("tail resume video starts %.3f, want 18.000", got)
	}
	if got := startTime(t, streams, "audio"); math.Abs(got-18) > 1.1 {
		t.Fatalf("tail resume audio starts %.3f, want 18.000", got)
	}
}

func TestMatroskaChunkArgsKeepPositiveTitleClock(t *testing.T) {
	d := core.Decision{Kind: core.DecisionRemux, Container: ContainerMatroska, VideoCopy: true, AudioCopy: true, AudioStream: 1}
	s := baseSpec(mustParse(t, "mkv_hevc_hdr10.json"), d)
	s.ChunkDir = "chunks"
	s.Features.jellyfinSegmentClock = true
	args := buildArgs(s)
	mustContain(t, args, "-copyts", "-start_at_zero", "-output_ts_offset 10.000", "-segment_time_delta 210.000", "-bsf:a noise=amount=0:drop='lt(pts*tb,200.000)'")
	mustNotContain(t, args, "-segment_format_options output_ts_offset=")
	s.Features.jellyfinSegmentClock = false
	args = buildArgs(s)
	mustContain(t, args, "-segment_format_options output_ts_offset=210.000", "-bsf:a noise=amount=0:drop='lt(pts*tb,0)'")
	mustNotContain(t, args, "-copyts", "-segment_time_delta")
}
