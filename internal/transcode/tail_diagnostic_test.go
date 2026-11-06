package transcode

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/mediaproc"
)

func TestShortVideoTailDiagnostic(t *testing.T) {
	e := newRealEnv(t, WithoutHWDetection(), WithSegmentTimeout(8*time.Second))
	path := filepath.Join(e.dir, "short.mkv")
	cmd := mediaproc.CommandContext(t.Context(), 2*time.Minute, e.ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-t", "9", "-i", "testsrc2=size=320x180:rate=25", "-f", "lavfi", "-t", "11", "-i", "sine=frequency=440:sample_rate=48000", "-c:v", "libx264", "-preset", "ultrafast", "-g", "50", "-c:a", "aac", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v %s", err, out)
	}
	info, err := e.m.Probe(t.Context(), e.url("short.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	d := core.Decision{Kind: core.DecisionTranscode, Container: ContainerFMP4, VideoCodec: "h264", AudioCodec: "aac", AudioStream: 1}
	hs, err := e.m.StartHLS(t.Context(), core.HLSRequest{SessionID: "short-fmp4", InputURL: e.url("short.mkv"), Info: *info, Decision: d, SegmentDuration: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	s := hs.(*Session)
	if s.total != 6 {
		t.Fatalf("segments = %d, want 6", s.total)
	}
	first := fetch(t, s, 0)
	init := fetch(t, s, InitSegment)
	var whole bytes.Buffer
	whole.Write(init)
	whole.Write(first)
	var last []byte
	for n := 1; n < s.total; n++ {
		last = fetch(t, s, n)
		whole.Write(last)
	}
	if after := fetch(t, s, InitSegment); !bytes.Equal(init, after) {
		t.Fatal("audio-only tail replaced the retained video init")
	}
	tail := append(append([]byte(nil), init...), last...)
	p := filepath.Join(e.dir, "tail.mp4")
	if err := os.WriteFile(p, tail, 0600); err != nil {
		t.Fatal(err)
	}
	pcm, err := mediaproc.CommandContext(t.Context(), 2*time.Minute, e.ffmpeg, "-v", "error", "-i", p, "-map", "0:a:0", "-f", "s16le", "-ac", "1", "-ar", "48000", "pipe:1").Output()
	if err != nil {
		t.Fatalf("decode retained init + tail audio: %v", err)
	}
	if len(pcm) < 70000 || bytes.Count(pcm, []byte{0}) == len(pcm) {
		t.Fatalf("tail audio not playable: %d PCM bytes", len(pcm))
	}
	probe, err := mediaproc.CommandContext(t.Context(), 2*time.Minute, e.ffprobe, "-v", "error", "-select_streams", "a:0", "-show_entries", "packet=pts_time", "-of", "json", p).Output()
	if err != nil {
		t.Fatalf("probe tail timestamps: %v", err)
	}
	var packets struct {
		Packets []struct {
			PTS string `json:"pts_time"`
		} `json:"packets"`
	}
	if err := json.Unmarshal(probe, &packets); err != nil || len(packets.Packets) == 0 {
		t.Fatalf("tail packets: %v %s", err, probe)
	}
	firstPTS, err1 := strconv.ParseFloat(packets.Packets[0].PTS, 64)
	lastPTS, err2 := strconv.ParseFloat(packets.Packets[len(packets.Packets)-1].PTS, 64)
	if err1 != nil || err2 != nil || firstPTS < 9.8 || firstPTS > 10.2 || lastPTS < 10.8 {
		t.Fatalf("tail audio timestamps: %.3f..%.3f (%v, %v)", firstPTS, lastPTS, err1, err2)
	}
	full := filepath.Join(e.dir, "whole.mp4")
	if err := os.WriteFile(full, whole.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	out, err := mediaproc.CommandContext(t.Context(), 2*time.Minute, e.ffmpeg, "-v", "error", "-xerror", "-i", full, "-map", "0:v:0", "-map", "0:a:0", "-f", "null", "-").CombinedOutput()
	if err != nil {
		t.Fatalf("decode complete retained-init output: %v: %s", err, out)
	}
}
