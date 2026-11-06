package transcode

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/mediaproc"
)

// TestNativeProbeMatchesFFprobe compares native probing with ffprobe on
// local files. It runs only when ETHER_NATIVEPROBE_DIR and ETHER_FFPROBE
// are set.
func TestNativeProbeMatchesFFprobe(t *testing.T) {
	dir, ffprobe := os.Getenv("ETHER_NATIVEPROBE_DIR"), os.Getenv("ETHER_FFPROBE")
	if dir == "" || ffprobe == "" {
		t.Skip("set ETHER_NATIVEPROBE_DIR and ETHER_FFPROBE")
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.mkv"))
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			out, err := mediaproc.CommandContext(t.Context(), 2*time.Minute, ffprobe, probeArgs(f, probeAttempts[0])...).Output()
			if err != nil {
				t.Fatal(err)
			}
			want, err := ParseProbe(out)
			if err != nil {
				t.Fatal(err)
			}
			for i := range want.Streams {
				if want.Streams[i].ColorTransfer == "unknown" {
					want.Streams[i].ColorTransfer = ""
				}
			}
			fh, _ := os.Open(f)
			defer fh.Close()
			st, _ := fh.Stat()
			got, err := ProbeSource(context.Background(), fh, st.Size())
			if err != nil {
				t.Fatalf("native: %v", err)
			}
			// Frame-header bitrates (AC-3, DTS, MP3) need packets; native
			// probing leaves them to the copied-audio estimate.
			for i := range want.Streams {
				if i < len(got.Streams) && got.Streams[i].BitrateKbps == 0 && want.Streams[i].Type == "audio" {
					want.Streams[i].BitrateKbps = 0
				}
			}
			w, _ := json.MarshalIndent(want, "", " ")
			g, _ := json.MarshalIndent(got, "", " ")
			if string(w) != string(g) {
				t.Errorf("mismatch\nffprobe: %s\nnative:  %s", w, g)
			}
		})
	}
}
