package transcode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/mediaproc"
)

// The held middle phase represents a ten-second source outage. The switcher's
// wall clock continues feeding ticks, and neither muxer may change contracts.
func TestContinuousPlayout(t *testing.T) {
	e := newRealEnv(t, WithoutHWDetection())
	ctx, cancel := context.WithTimeout(t.Context(), 75*time.Second)
	defer cancel()
	format := RawFormat{Width: 320, Height: 180, FPSNum: 30, FPSDen: 1, SampleRate: 48000, Channels: 2, PixelFormat: "yuv420p"}
	runner, err := e.m.StartEncoder(ctx, EncoderRequest{Source: format, DVRWindow: time.Minute, Decision: core.Decision{VideoCodec: "h264", AudioCodec: "aac", BitrateKbps: 500}})
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close()
	enc := runner.(*continuousEncoder)
	frame := RawFrame{Video: make([]byte, format.Width*format.Height*3/2), PCM: make([]byte, 6400)}
	for i := format.Width * format.Height; i < len(frame.Video); i++ {
		frame.Video[i] = 128
	}
	start := time.Now()
	for n := 0; n < 660; n++ {
		// Two changing patterns separated by ten seconds of one held picture.
		if n < 120 || n >= 420 {
			for i := 0; i < format.Width*format.Height; i++ {
				frame.Video[i] = byte(16 + (i+n)%200)
			}
		}
		if err := runner.Write(ctx, frame); err != nil {
			t.Fatalf("tick %d: %v", n, err)
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Until(start.Add(time.Duration(n+1) * time.Second / 30))):
		}
	}
	// Allow complete segments to be published; shutdown intentionally does not
	// publish the incomplete final segment as though it were a full GOP.
	for _, name := range []string{"index.m3u8"} {
		path := filepath.Join(enc.dir, name)
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "DISCONTINUITY") {
			t.Fatal("discontinuity in output")
		}
		if name == "index.mpd" {
			continue
		} // This host lacks the DASH demuxer; validate fMP4 tracks below.
		b = append(b, []byte("\n#EXT-X-ENDLIST\n")...)
		path = filepath.Join(enc.dir, "capture-"+name)
		if err := os.WriteFile(path, b, 0600); err != nil {
			t.Fatal(err)
		}
		cmd := mediaproc.CommandContext(ctx, 2*time.Minute, e.ffmpeg, "-hide_banner", "-loglevel", "error", "-xerror", "-i", path, "-t", "12", "-f", "null", "-")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("decode %s: %v %s manifest=%s", name, err, out, b)
		}
	}
	capture, err := os.ReadFile(filepath.Join(enc.dir, "init.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 5; n++ {
		b, err := os.ReadFile(filepath.Join(enc.dir, fmt.Sprintf("fragment-%09d.m4s", n)))
		if err != nil {
			t.Fatal(err)
		}
		capture = append(capture, b...)
	}
	path := filepath.Join(enc.dir, "capture.mp4")
	if err := os.WriteFile(path, capture, 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := mediaproc.CommandContext(ctx, 2*time.Minute, e.ffmpeg, "-v", "error", "-xerror", "-i", path, "-f", "null", "-").CombinedOutput(); err != nil {
		t.Fatalf("fMP4 decode: %v %s", err, out)
	}
	segments := runner.Segments().Since(-1, 100)
	if len(segments) != 7 {
		t.Fatalf("finished segments: %v", segments)
	}
	for i, seg := range segments {
		start, frames := continuousSegmentFrames(format, int64(i))
		if seg.Seq != int64(i) || seg.Duration != continuousFrameTime(format, frames) || !seg.Start.Equal(segments[0].Start.Add(continuousFrameTime(format, start))) {
			t.Fatalf("segment clock: %v", segments)
		}
	}

	var contract string
	var lastPTS [2]float64
	lastPTS[0] = math.Inf(-1)
	lastPTS[1] = math.Inf(-1)
	var last [2]float64
	last[0] = math.Inf(-1)
	last[1] = math.Inf(-1)
	for n := 0; n < 5; n++ {
		path := filepath.Join(enc.dir, fmt.Sprintf("seg-%09d.ts", n))
		out, err := mediaproc.CommandContext(ctx, 2*time.Minute, e.ffprobe, "-v", "error", "-show_streams", "-show_packets", "-of", "json", path).Output()
		if err != nil {
			t.Fatal(err)
		}
		var p struct {
			Streams []struct {
				CodecName     string `json:"codec_name"`
				Width, Height int
				Profile       string
			}
			Packets []struct {
				StreamIndex int    `json:"stream_index"`
				PTS         string `json:"pts_time"`
				DTS         string `json:"dts_time"`
			}
		}
		if err := json.Unmarshal(out, &p); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(p.Streams)
		if n == 0 {
			contract = string(b)
		} else if string(b) != contract {
			t.Fatal("codec parameters changed")
		}
		first := -1.0
		end := 0.0
		for _, pkt := range p.Packets {
			var pts, dts float64
			fmt.Sscan(pkt.PTS, &pts)
			fmt.Sscan(pkt.DTS, &dts)
			if pkt.StreamIndex > 1 {
				continue
			}
			if pts <= lastPTS[pkt.StreamIndex] {
				t.Fatalf("nonmonotonic PTS %v", pkt)
			}
			lastPTS[pkt.StreamIndex] = pts
			if dts <= last[pkt.StreamIndex] {
				t.Fatalf("nonmonotonic timestamp %v after %v", pkt, last)
			}
			last[pkt.StreamIndex] = dts
			if pkt.StreamIndex == 0 {
				if first < 0 {
					first = pts
				}
				end = pts
			}
		}
		_, frames := continuousSegmentFrames(format, int64(n))
		if math.Abs(end-first+1.0/30-float64(frames)/30) > 0.001 {
			t.Fatalf("segment duration %f", end-first+1.0/30)
		}
	}
	// Decode a published capture to exercise raw socket framing and prewarm.
	d, err := e.m.StartDecoder(ctx, DecoderRequest{InputURL: e.url(filepath.ToSlash(filepath.Join("hls", filepath.Base(enc.dir), "seg-000000000.ts"))), Format: format})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	time.Sleep(20 * time.Second)
	for range 12 {
		f, err := d.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(f.Video) != len(frame.Video) || len(f.PCM) != 6400 {
			t.Fatal("raw size mismatch")
		}
	}
	loop, err := e.m.StartDecoder(ctx, DecoderRequest{InputURL: filepath.Join(enc.dir, "seg-000000000.ts"), Format: format, Loop: true})
	if err != nil {
		t.Fatal(err)
	}
	defer loop.Close()
	// The master contains 120 pictures; reading beyond its end proves the
	// decoder crosses the loop point without an EOF or process restart.
	for range 150 {
		if _, err := loop.Next(ctx); err != nil {
			t.Fatalf("local master loop: %v", err)
		}
	}

}

// Index publication and eviction are one invariant shared by every delivery
// format. Simulated atomic muxer files keep this part independent of ffmpeg.
func TestContinuousSegmentIndex(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	e := &continuousEncoder{continuousProcess: &continuousProcess{ctx: ctx}, dir: t.TempDir(), req: EncoderRequest{Source: RawFormat{Width: 320, Height: 180, FPSNum: 30000, FPSDen: 1001}, DVRWindow: 8 * time.Second}}
	index := newContinuousSegmentIndex(e)
	e.index = index
	start := time.Unix(1000, 0)
	index.setStart(start)
	for n := int64(0); n < 6; n++ {
		first, count := continuousSegmentFrames(e.req.Source, n)
		for tick := first; tick < first+count; tick++ {
			index.recordPolicy(tick, []any{"programme", n})
		}
		for _, path := range []string{index.ts(n), index.mp4(n)} {
			if err := os.WriteFile(path, []byte("finished"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		index.refresh()
	}
	segments := index.Since(-1, 10)
	if len(segments) != 2 || segments[0].Seq != 4 || segments[1].Seq != 5 {
		t.Fatal(segments)
	}
	for _, seg := range segments {
		if len(seg.PolicyTags) != 2 || seg.PolicyTags[0] != "programme" || seg.PolicyTags[1] != seg.Seq {
			t.Fatalf("lost policy: %+v", seg)
		}
		if seg.Duration != 4004*time.Millisecond || !seg.Start.Equal(start.Add(time.Duration(seg.Seq-2)*4004*time.Millisecond)) {
			t.Fatal(seg)
		}
	}
	if _, err := index.Wait(ctx, -1); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("evicted wait: %v", err)
	}
	if _, err := index.open(0, false); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("evicted TS: %v", err)
	}
	if _, err := index.open(0, true); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("evicted fMP4: %v", err)
	}
	got, err := index.Wait(ctx, 4)
	if err != nil || got.Seq != 5 {
		t.Fatalf("wait: %v %v", got, err)
	}
	if window := index.Window(segments[0].Start.Add(time.Millisecond), segments[1].Start); len(window) != 1 || window[0].Seq != 4 {
		t.Fatal(window)
	}
	blocked, cancelWait := context.WithCancel(ctx)
	cancelWait()
	if _, err := index.Wait(blocked, 5); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
