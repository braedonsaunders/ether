package transcode

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/mediaproc"
)

// Run explicitly with ETHER_HARDSUB_FIXTURES=1. Fixtures are generated offline;
// ordinary package CI doesn't need ffmpeg or a font installed.
func TestHardsubFixtures(t *testing.T) {
	if os.Getenv("ETHER_HARDSUB_FIXTURES") != "1" {
		t.Skip("opt-in ffmpeg measurement")
	}
	ff, err := exec.LookPath("ffmpeg")
	if custom := os.Getenv("ETHER_FIXTURE_FFMPEG"); custom != "" {
		ff = custom
		err = nil
	}
	if err != nil {
		t.Fatal(err)
	}
	font := "/System/Library/Fonts/Supplemental/Arial.ttf"
	if _, err := os.Stat(font); err != nil {
		font = "/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf"
	}
	text := func(s, y, color string) string {
		return "drawtext=fontfile=" + font + ":text='" + s + "':fontsize=16:fontcolor=" + color + ":borderw=2:bordercolor=black:x=(w-tw)/2:y=" + y
	}
	changing := func(y, color string) string {
		return text("First scene dialogue", y, color) + ":enable='lt(t,5)'," + text("Another conversation here", y, color) + ":enable='gte(t,5)'"
	}
	srt := filepath.Join(t.TempDir(), "dialogue.srt")
	if err := os.WriteFile(srt, []byte("1\n00:00:00,000 --> 00:00:05,000\nFirst scene dialogue\n\n2\n00:00:05,000 --> 00:00:12,000\nAnother conversation here\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, filter string
		burn         bool
	}{
		{"clean", "null", false},
		{"srt", "subtitles=" + srt + ":force_style='Fontsize=24,Outline=2'", true}, {"logo", text("STATION LOGO", "12", "white"), false},
		{"clean_edges", "drawgrid=width=50:height=40:thickness=2:color=white", false},
		{"bottom", changing("h-30", "white"), true}, {"top", changing("10", "white"), true},
		{"dual", changing("h-30", "white") + "," + changing("10", "yellow"), true},
		{"yellow", changing("h-30", "yellow"), true},
	}
	fp, fn, neg, pos := 0, 0, 0, 0
	m := &Manager{runner: ExecRunner{}, ffmpeg: ff}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "fixture.mkv")
			cmd := mediaproc.CommandContext(t.Context(), 2*time.Minute, ff, "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=640x360:rate=4:duration=12", "-vf", c.filter, "-threads", "1", "-c:v", "libx264", "-preset", "ultrafast", "-y", path)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("generate: %v %s", err, out)
			}
			start := time.Now()
			v, known := m.DetectHardSubs(context.Background(), path, 12*time.Second)
			t.Logf("burned=%v known=%v elapsed=%s", v, known, time.Since(start))
			if !known {
				t.Fatal("unknown")
			}
			if c.burn {
				pos++
				if !v {
					fn++
				}
			} else {
				neg++
				if v {
					fp++
				}
			}
		})
	}
	t.Logf("false positives=%d/%d false negatives=%d/%d", fp, neg, fn, pos)
	if fp != 0 || fn != 0 {
		t.Fail()
	}
}
