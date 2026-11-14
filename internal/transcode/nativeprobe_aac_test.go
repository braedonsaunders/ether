package transcode

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

// Configuration IDs are not channel counts. Keep the table, reserved/PCE
// fallback, and both container consumers under the same offline contract.
func TestAACChannelConfigurations(t *testing.T) {
	counts := []int{0, 1, 2, 3, 4, 5, 6, 8, 0, 0, 0, 7, 8, 24, 8, 0}
	layouts := []string{"", "mono", "stereo", "3.0", "4.0", "5.0", "5.1", "7.1(wide)", "", "", "", "6.1(back)", "7.1", "22.2", "5.1.2(back)", ""}
	for config, count := range counts {
		// AAC-LC, 48 kHz, followed by GASpecificConfig flags cleared.
		asc := []byte{0x11, byte(0x80 | config<<3)}
		profile, _, got, layout, ok := aacConfig(asc)
		if ok != (count > 0) || got != count || layout != layouts[config] || (ok && profile != "LC") {
			t.Errorf("config %d: profile=%q channels=%d layout=%q ok=%v", config, profile, got, layout, ok)
		}
	}
	for _, ext := range []string{"mkv", "mp4"} {
		t.Run(ext, func(t *testing.T) {
			file := filepath.Join("testdata", "aac-channel-config", "aac71."+ext)
			raw, err := os.ReadFile(file + ".ffprobe.json")
			if err != nil {
				t.Fatal(err)
			}
			want, err := ParseProbe(raw)
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			got, err := ProbeSource(context.Background(), bytes.NewReader(data), int64(len(data)))
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Streams) != 2 || len(want.Streams) != 2 {
				t.Fatal("expected video and audio streams")
			}
			g, w := got.Streams[1], want.Streams[1]
			if w.Channels != 8 || g.Channels != w.Channels || g.SampleRate != w.SampleRate || g.Codec != w.Codec || g.Profile != w.Profile {
				t.Fatalf("native %+v differs from ffprobe %+v", g, w)
			}
			// FFmpeg assumes incorrectly encoded conventional 7.1 by default for
			// config 7. Native headers retain the spec's front-wide assignment.
			if g.ChannelLayout != "7.1(wide)" {
				t.Fatalf("layout %q", g.ChannelLayout)
			}
		})
	}
}
