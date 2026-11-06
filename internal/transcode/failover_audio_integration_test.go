package transcode

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/mediaproc"
)

// Keep the original decoder initialization, just like a player already
// consuming a DASH stream. A replacement's same-codec audio must decode
// against that configuration, even when its channels and sample rate differ.
func TestIntegrationDASHFailoverKeepsAudioConfiguration(t *testing.T) {
	e := newRealEnv(t, WithoutHWDetection())
	e.gen(t, "stereo.mkv", 24, "-c:v", "libx264", "-preset", "ultrafast", "-g", "24", "-c:a", "aac", "-ac", "2", "-ar", "44100")
	e.gen(t, "surround.mkv", 24, "-c:v", "libx264", "-preset", "ultrafast", "-g", "24", "-c:a", "aac", "-ac", "6", "-ar", "48000")
	ctx := context.Background()
	old, err := e.m.Probe(ctx, e.url("stereo.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	next, err := e.m.Probe(ctx, e.url("surround.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	d := e.m.Decide(*old, plexWeb, allow())
	if !d.AudioCopy {
		t.Fatal("test must begin with copied AAC")
	}
	ds := startDASH(t, e.m, core.HLSRequest{SessionID: "audio-config", InputURL: e.url("stereo.mkv"), Info: *old, Decision: d})
	parts := [][]byte{dashGet(t, ds, 1, InitSegment), dashGet(t, ds, 1, 0), dashGet(t, ds, 1, 1)}
	if err := ds.SwapInput(ctx, e.url("surround.mkv"), *next, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	parts = append(parts, dashGet(t, ds, 1, 2), dashGet(t, ds, 1, 3))
	var data []byte
	for _, b := range parts {
		data = append(data, b...)
	}
	file := e.writeTemp(t, "continuous-audio.mp4", data)
	if b, err := mediaproc.CommandContext(t.Context(), 2*time.Minute, e.ffmpeg, "-hide_banner", "-v", "error", "-xerror", "-i", file, "-map", "0:a:0", "-f", "null", "-").CombinedOutput(); err != nil {
		t.Fatalf("existing AAC decoder rejected the replacement: %v\n%s", err, b)
	}
}
