package transcode

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/mediaproc"
)

func TestOriginalAudioOnlyRendition(t *testing.T) {
	e := newRealEnv(t, WithoutHWDetection(), WithBuffering(6*time.Second, 0, 0))
	if !e.m.AudioFallbackAvailable() {
		t.Fatal("real AAC pipeline not advertised")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	input := filepath.Join(e.dir, "original.mkv")
	args := []string{"-v", "error", "-nostdin", "-f", "lavfi", "-i", "testsrc2=size=64x64:rate=10", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-t", "10", "-c:v", "libx264", "-preset", "ultrafast", "-threads", "1", "-c:a", "flac", input}
	if out, err := mediaproc.CommandContext(ctx, time.Minute, e.ffmpeg, args...).CombinedOutput(); err != nil {
		t.Fatalf("fixture %v %s", err, out)
	}
	info, err := e.m.Probe(ctx, e.url("original.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	var selected core.StreamInfo
	for _, s := range info.Streams {
		if s.Type == "audio" {
			selected = s
		}
	}
	if selected.Type == "" {
		t.Fatal("missing source audio")
	}
	info.Streams = []core.StreamInfo{selected}
	hs, err := e.m.StartHLS(ctx, core.HLSRequest{SessionID: "audio-only", InputURL: e.url("original.mkv"), Info: *info, StartOffset: 4 * time.Second, SegmentDuration: 2 * time.Second, Decision: core.Decision{Kind: core.DecisionTranscode, Container: ContainerFMP4, VideoCopy: true, AudioCodec: "aac", AudioStream: selected.Index, SubtitleStream: -1, AudioChannels: 1, AudioSampleRate: 48000}})
	if err != nil {
		t.Fatal(err)
	}
	defer hs.Close()
	bytes := append(fetch(t, hs, InitSegment), fetch(t, hs, 0)...)
	output := filepath.Join(e.dir, "audio-only.mp4")
	if err = os.WriteFile(output, bytes, 0600); err != nil {
		t.Fatal(err)
	}
	out, err := mediaproc.CommandContext(ctx, time.Minute, e.ffprobe, "-v", "error", "-show_streams", "-show_packets", "-of", "json", output).Output()
	if err != nil {
		t.Fatal(err)
	}
	var probe struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
			CodecName string `json:"codec_name"`
		}
		Packets []struct {
			PTS string `json:"pts_time"`
		}
	}
	if err = json.Unmarshal(out, &probe); err != nil {
		t.Fatal(err)
	}
	if len(probe.Streams) != 1 || probe.Streams[0].CodecType != "audio" || probe.Streams[0].CodecName != "aac" || len(probe.Packets) == 0 {
		t.Fatalf("not audio only: %s", out)
	}
	pts, err := strconv.ParseFloat(probe.Packets[0].PTS, 64)
	if err != nil || pts < 3.9 || pts > 4.1 {
		t.Fatalf("audio clock %v %v", pts, err)
	}
}
