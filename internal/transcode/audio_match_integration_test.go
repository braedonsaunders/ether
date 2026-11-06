package transcode

import (
	"context"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/mediaproc"
)

// Decode the same static-header Matroska the Apple TV keeps open. Both video
// legs must remain copy, but the doubled replacement audio must come back to
// the opening level after audio-only correction.
func TestIntegrationSourceAudioLevelMatch(t *testing.T) {
	t.Run("attenuate", func(t *testing.T) { testSourceAudioMatch(t, 2, -6) })
	t.Run("boost", func(t *testing.T) { testSourceAudioMatch(t, 0.75, 2.5) })
}
func testSourceAudioMatch(t *testing.T, replacementGain, wantGain float64) {
	// The production budget (1.5s) is a latency guard; under the race
	// detector on a busy CI runner the two real decodes can exceed it.
	e := newRealEnv(t, WithoutHWDetection(), WithBuffering(time.Minute, 0, 0), WithAudioMatchBudget(20*time.Second))
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	for _, tc := range []struct {
		name string
		gain float64
	}{{"original", 1}, {"louder", replacementGain}} {
		pcm := make([]byte, 24*audioMatchRate*4)
		for i := 0; i < len(pcm)/4; i++ {
			sec := float64(i) / audioMatchRate
			env := 0.025 + 0.17*math.Pow((1+math.Sin(sec*3.7+math.Sin(sec*1.3)))/2, 2)
			v := tc.gain * env * (math.Sin(sec*437*2*math.Pi) + 0.3*math.Sin(sec*437*3.14*2*math.Pi))
			binary.LittleEndian.PutUint32(pcm[i*4:], math.Float32bits(float32(v)))
		}
		raw := filepath.Join(e.dir, tc.name+".pcm")
		if err := os.WriteFile(raw, pcm, 0600); err != nil {
			t.Fatal(err)
		}
		args := []string{"-v", "error", "-nostdin", "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=24", "-f", "f32le", "-ar", "8000", "-ac", "1", "-i", raw, "-t", "24", "-c:v", "libx264", "-preset", "ultrafast", "-threads", "1", "-g", "24", "-keyint_min", "24", "-sc_threshold", "0", "-c:a", "aac", "-ar", "48000", "-b:a", "128k", filepath.Join(e.dir, tc.name+".mkv")}
		if out, err := mediaproc.CommandContext(ctx, 2*time.Minute, e.ffmpeg, args...).CombinedOutput(); err != nil {
			t.Fatalf("fixture: %v %s", err, out)
		}
	}
	original, err := e.m.Probe(ctx, e.url("original.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	other, err := e.m.Probe(ctx, e.url("louder.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	caps := appleTV
	caps.HLSContainers = []string{"mkv"}
	opts := allow()
	opts.DirectPlay = false
	d := e.m.Decide(*original, caps, opts)
	if !d.VideoCopy || !d.AudioCopy || d.Container != ContainerMatroska {
		t.Fatalf("opening %+v", d)
	}
	hs, err := e.m.StartHLS(ctx, core.HLSRequest{SessionID: "audio-match", InputURL: e.url("original.mkv"), Info: *original, Decision: d})
	if err != nil {
		t.Fatal(err)
	}
	defer hs.Close()
	s := hs.(*Session)
	joined := append([]byte(nil), fetch(t, hs, InitSegment)...)
	for {
		b := s.RecoveryBuffer()
		if b.ReadyUntil-b.Position >= 12*time.Second {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("buffer not ready")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := s.PrepareSwap(ctx, e.url("louder.mkv"), *other, 8*time.Second); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	n := len(s.audioMatch.prepared)
	s.mu.Unlock()
	if n != 1 {
		t.Fatal("real audio analysis did not accept the matching passage")
	}
	joined = append(joined, fetch(t, hs, 0)...)
	joined = append(joined, fetch(t, hs, 1)...)
	if err := s.SwapInput(ctx, e.url("louder.mkv"), *other, 8*time.Second); err != nil {
		t.Fatal(err)
	}
	nd := s.Decision()
	if !nd.VideoCopy || nd.AudioCopy {
		t.Fatalf("expected audio-only encode: %+v", nd)
	}
	s.mu.Lock()
	gain := s.audioMatch.gain
	s.mu.Unlock()
	if math.Abs(gain-wantGain) > 0.2 {
		t.Fatalf("gain %v", gain)
	}
	for seg := 2; seg < 5; seg++ {
		joined = append(joined, fetch(t, hs, seg)...)
	}
	if err := os.WriteFile(filepath.Join(e.dir, "joined.mkv"), joined, 0600); err != nil {
		t.Fatal(err)
	}
	// The original retained header must decode all packets, not just the new init.
	if out, err := mediaproc.CommandContext(ctx, 2*time.Minute, e.ffmpeg, "-v", "error", "-xerror", "-i", filepath.Join(e.dir, "joined.mkv"), "-f", "null", "-").CombinedOutput(); err != nil {
		t.Fatalf("retained header: %v %s", err, out)
	}
	ref := s.sampleAudio(ctx, e.url("original.mkv"), 1, 9*time.Second)
	corrected := s.sampleAudio(ctx, e.url("joined.mkv"), 1, 9*time.Second)
	delta, ok := matchingAudioGain(ref, corrected)
	if !ok || math.Abs(delta) > 0.4 {
		t.Fatalf("decoded level difference %v dB match=%v", delta, ok)
	}
	t.Logf("replacement: gain %.1f dB, decoded residual %.3f dB, video copied throughout", gain, delta)
}
