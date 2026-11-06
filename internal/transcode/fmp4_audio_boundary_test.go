package transcode

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/mediaproc"
)

// A browser retains the first init while a later encoder replaces the next
// fragment. Packet checks must include audio at the preceding cut, not merely
// the new fragment's first timestamp: otherwise B-frame interleaving can leave
// an audible hole even though both video boundaries are correct.
func TestIntegrationFMP4EncodedRestartAudioBoundary(t *testing.T) {
	e := newRealEnv(t, WithoutHWDetection())
	e.gen(t, "audio-boundary.mkv", 16, "-c:v", "libx264", "-threads", "2", "-preset", "veryfast", "-g", "48", "-bf", "3", "-c:a", "aac", "-ac", "2")
	info, err := e.m.Probe(t.Context(), e.url("audio-boundary.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	hs, err := e.m.StartHLS(t.Context(), core.HLSRequest{SessionID: "audio-boundary", InputURL: e.url("audio-boundary.mkv"), Info: *info, Decision: core.Decision{Kind: core.DecisionTranscode, Container: ContainerFMP4, VideoCodec: "h264", MaxHeight: 360, BitrateKbps: 800, AudioCopy: true, AudioStream: 1}, SegmentDuration: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer hs.Close()
	init := fetch(t, hs, InitSegment)
	_ = fetch(t, hs, 0)
	before := fetch(t, hs, 1)
	s := hs.(*Session)
	s.mu.Lock()
	for n := range s.done[0] {
		if n >= 2 {
			delete(s.done[0], n)
		}
	}
	err = s.startRunLocked(2, false)
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	after := fetch(t, hs, 2)
	span := func(name string, seg []byte) (float64, float64) {
		t.Helper()
		p := filepath.Join(e.dir, name+".mp4")
		if err := os.WriteFile(p, append(append([]byte(nil), init...), seg...), 0600); err != nil {
			t.Fatal(err)
		}
		out, err := mediaproc.CommandContext(t.Context(), 2*time.Minute, e.ffprobe, "-v", "error", "-select_streams", "a:0", "-show_entries", "packet=pts_time,duration_time", "-of", "json", p).Output()
		if err != nil {
			t.Fatal(err)
		}
		var d struct {
			Packets []struct {
				PTS string `json:"pts_time"`
				Dur string `json:"duration_time"`
			} `json:"packets"`
		}
		if err := json.Unmarshal(out, &d); err != nil {
			t.Fatal(err)
		}
		if len(d.Packets) == 0 {
			t.Fatal("no audio packets")
		}
		first, last := math.Inf(1), math.Inf(-1)
		for _, p := range d.Packets {
			pts, err := strconv.ParseFloat(p.PTS, 64)
			if err != nil {
				t.Fatal(err)
			}
			dur, err := strconv.ParseFloat(p.Dur, 64)
			if err != nil {
				t.Fatal(err)
			}
			first = math.Min(first, pts)
			last = math.Max(last, pts+dur)
		}
		return first, last
	}
	_, end := span("before", before)
	start, _ := span("after", after)
	if gap := start - end; gap > 0.025 || math.Abs(start-4) > 0.05 {
		t.Fatalf("audio boundary %.6f -> %.6f, gap %.6f", end, start, gap)
	}
	t.Logf("audio boundary %.6f -> %.6f", end, start)
}
