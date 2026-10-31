package transcode

import (
	"context"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestIntegrationMatroskaPresentationStartsAtSeekOffset(t *testing.T) {
	e := newRealEnv(t, WithoutHWDetection(), WithSegmentTimeout(5*time.Second))
	e.gen(t, "seek-offset.mkv", 24, "-c:v", "libx264", "-preset", "ultrafast", "-g", "24", "-c:a", "aac")
	info, err := e.m.Probe(t.Context(), e.url("seek-offset.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	d := core.Decision{Kind: core.DecisionRemux, Container: ContainerMatroska, VideoCopy: true, AudioCopy: true, AudioStream: 1}
	hs, err := e.m.StartHLS(t.Context(), core.HLSRequest{SessionID: "seek-offset", InputURL: e.url("seek-offset.mkv"), Info: *info, Decision: d, StartOffset: 12 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	data := append(fetch(t, hs, InitSegment), fetch(t, hs, 0)...)
	path := e.writeTemp(t, "offset-result.mkv", data)
	if got := startTime(t, e.probeFile(t, path), "video"); math.Abs(got-22) > 1.1 {
		t.Fatalf("seek presentation starts %.3f, want 22", got)
	}
}

func TestIntegrationMatroskaSubtitleSeekMatchesVideo(t *testing.T) {
	e := newRealEnv(t, WithoutHWDetection(), WithSegmentTimeout(20*time.Second))
	srt := filepath.Join(e.dir, "cues.srt")
	if err := os.WriteFile(srt, []byte("1\n00:00:12,000 --> 00:00:14,000\nAt twelve\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.gen(t, "subs.mkv", 24, "-i", srt, "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-g", "24", "-c:a", "aac", "-c:s", "srt")
	info, err := e.m.Probe(t.Context(), e.url("subs.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	sub := -1
	for _, stream := range info.Streams {
		if stream.Type == "subtitle" {
			sub = stream.Index
		}
	}
	if sub < 0 {
		t.Fatal("generated file has no subtitle stream")
	}
	d := core.Decision{Kind: core.DecisionTranscode, Container: ContainerMatroska, VideoCodec: "h264", AudioCodec: "aac", AudioCopy: true, AudioStream: 1, SubtitleStream: sub, MaxHeight: 360, BitrateKbps: 1500}
	hs, err := e.m.StartHLS(t.Context(), core.HLSRequest{SessionID: "sub-seek", InputURL: e.url("subs.mkv"), Info: *info, Decision: d, StartOffset: 12 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	path := e.writeTemp(t, "sub-seek.mkv", append(fetch(t, hs, InitSegment), fetch(t, hs, 0)...))
	streams := e.probeFile(t, path)
	video, cue := startTime(t, streams, "video"), startTime(t, streams, "subtitle")
	if math.Abs(video-cue) > 0.5 || math.Abs(video-22) > 1.5 {
		t.Fatalf("resumed segment video %.3f subtitle %.3f, want both near 22", video, cue)
	}
}

func TestIntegrationMatroskaPresentationLongSeekOffset(t *testing.T) {
	e := newRealEnv(t, WithoutHWDetection(), WithSegmentTimeout(5*time.Second))
	// -max_interleave_delta 0: without it a loaded machine lets the x265
	// thread fall >10s behind the audio and the muxer flushes audio alone,
	// so the fixture's interleaving (and where -ss lands) varied per run.
	gctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute) // bounded under load
	defer cancel()
	out, err := exec.CommandContext(gctx, e.ffmpeg, "-hide_banner", "-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i", "color=size=64x64:rate=1", "-f", "lavfi", "-i", "anullsrc=sample_rate=8000:channel_layout=stereo", "-t", "4500", "-c:v", "libx265", "-preset", "ultrafast", "-x265-params", "pools=1:frame-threads=1:keyint=2:min-keyint=2:scenecut=0", "-c:a", "aac", "-max_interleave_delta", "0", filepath.Join(e.dir, "long-seek.mkv")).CombinedOutput()
	if err != nil {
		t.Skipf("cannot generate HEVC: %v %s", err, out)
	}
	info, err := e.m.Probe(t.Context(), e.url("long-seek.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	d := core.Decision{Kind: core.DecisionRemux, Container: ContainerMatroska, VideoCopy: true, AudioCopy: true, AudioStream: 1}
	hs, err := e.m.StartHLS(t.Context(), core.HLSRequest{SessionID: "long-seek-offset", InputURL: e.url("long-seek.mkv"), Info: *info, Decision: d, StartOffset: 4451 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	data := append(fetch(t, hs, InitSegment), fetch(t, hs, 0)...)
	path := e.writeTemp(t, "long-offset-result.mkv", data)
	if got := startTime(t, e.probeFile(t, path), "video"); math.Abs(got-4461) > 2.1 {
		t.Fatalf("long seek starts %.3f, want 4461", got)
	}
}
