package transcode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
	args := []string{"-v", "error", "-nostdin", "-f", "lavfi", "-i", "testsrc2=size=64x64:rate=10", "-f", "lavfi", "-i", "aevalsrc=0.1*sin(2*PI*440*t):s=48000:c=5.1", "-t", "10", "-c:v", "libx264", "-preset", "ultrafast", "-threads", "1", "-c:a", "ac3", "-b:a", "640k", input}
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
	if selected.Channels != 6 || selected.Codec != "ac3" {
		t.Fatalf("expected 640 kbps 5.1 fixture, got %+v", selected)
	}
	info.Streams = []core.StreamInfo{selected}
	hs, err := e.m.StartHLS(ctx, core.HLSRequest{SessionID: "audio-only", InputURL: e.url("original.mkv"), Info: *info, StartOffset: 4 * time.Second, SegmentDuration: AudioFallbackSegmentDuration, Decision: core.Decision{Kind: core.DecisionTranscode, Container: ContainerFMP4, VideoCopy: true, AudioCodec: "aac", AudioStream: selected.Index, SubtitleStream: -1, AudioChannels: 2, AudioSampleRate: 48000, AudioBitrateKbps: 192}})
	if err != nil {
		t.Fatal(err)
	}
	defer hs.Close()
	rec := httptest.NewRecorder()
	hs.ServePlaylist(rec, httptest.NewRequest(http.MethodGet, "/index.m3u8", nil))
	playlist := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(playlist, "#EXT-X-TARGETDURATION:2\n") ||
		!strings.Contains(playlist, `#EXT-X-MAP:URI="init.mp4"`) || strings.Contains(playlist, "BYTERANGE") {
		t.Fatalf("audio playlist violates native client contract: %s", playlist)
	}
	entries := parseFFmpegPlaylist(rec.Body.Bytes()).entries
	if len(entries) < 3 {
		t.Fatalf("too few fragments: %s", playlist)
	}
	init := fetch(t, hs, InitSegment)
	var bytes []byte
	for i, entry := range entries {
		if entry.dur <= 0 || entry.dur > 2*time.Second {
			t.Fatalf("fragment %d EXTINF=%v", i, entry.dur)
		}
		n, ok := ParseSegmentName(entry.name)
		if !ok || n < 0 {
			t.Fatalf("invalid media resource %q", entry.name)
		}
		media := fetch(t, hs, n)
		if len(media) == 0 || len(media) >= 2*1024*1024 {
			t.Fatalf("fragment %d size=%d", i, len(media))
		}
		combined := append(append([]byte(nil), init...), media...)
		path := filepath.Join(e.dir, "fragment.mp4")
		if err := os.WriteFile(path, combined, 0600); err != nil {
			t.Fatal(err)
		}
		raw, err := mediaproc.CommandContext(ctx, time.Minute, e.ffprobe, "-v", "error", "-show_packets", "-of", "json", path).Output()
		if err != nil {
			t.Fatal(err)
		}
		var packets struct {
			Packets []struct {
				PTSTime  string `json:"pts_time"`
				Duration string `json:"duration_time"`
			}
		}
		if err := json.Unmarshal(raw, &packets); err != nil {
			t.Fatal(err)
		}
		if len(packets.Packets) == 0 {
			t.Fatalf("empty fragment %d", i)
		}
		first, err := strconv.ParseFloat(packets.Packets[0].PTSTime, 64)
		if err != nil {
			t.Fatal(err)
		}
		lastPacket := packets.Packets[len(packets.Packets)-1]
		last, err := strconv.ParseFloat(lastPacket.PTSTime, 64)
		if err != nil {
			t.Fatal(err)
		}
		duration, err := strconv.ParseFloat(lastPacket.Duration, 64)
		if err != nil {
			t.Fatal(err)
		}
		if span := last + duration - first; span <= 0 || span > 2.0 {
			t.Fatalf("fragment %d actual duration=%f", i, span)
		}
		if i == 0 {
			bytes = combined
		}
	}
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
			Channels  int    `json:"channels"`
		}
		Packets []struct {
			PTS string `json:"pts_time"`
		}
	}
	if err = json.Unmarshal(out, &probe); err != nil {
		t.Fatal(err)
	}
	if len(probe.Streams) != 1 || probe.Streams[0].CodecType != "audio" || probe.Streams[0].CodecName != "aac" || probe.Streams[0].Channels != 2 || len(probe.Packets) == 0 {
		t.Fatalf("not audio only: %s", out)
	}
	pts, err := strconv.ParseFloat(probe.Packets[0].PTS, 64)
	if err != nil || pts < 3.9 || pts > 4.1 {
		t.Fatalf("audio clock %v %v", pts, err)
	}
}
