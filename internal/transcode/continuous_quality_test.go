package transcode

import (
	"flag"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/mediaproc"
)

var signalQuality = flag.Bool("signal-quality", false, "compare continuous encoder programme SSIM before/after live tuning")

func TestContinuousProgrammeQuality(t *testing.T) {
	if !*signalQuality {
		t.Skip("opt-in quality comparison")
	}
	e := newRealEnv(t, WithoutHWDetection())
	scores := []float64{}
	for _, mode := range []string{"before", "after"} {
		params := "vbv-init=0.1"
		if mode == "after" {
			params += ":rc-lookahead=20:sync-lookahead=0:aq-mode=2:aq-strength=0.8:psy-rd=1.0,0.15"
		}
		path := filepath.Join(e.dir, mode+".mp4")
		args := []string{"-v", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=1920x1080:rate=30", "-t", "8", "-c:v", "libx264", "-preset", "veryfast", "-crf", "18", "-maxrate", "4269k", "-bufsize", "474k", "-profile:v", "high", "-pix_fmt", "yuv420p", "-threads", "4", "-bf", "0", "-g", "120", "-keyint_min", "120", "-sc_threshold", "0", "-x264-params", params, path}
		if out, err := mediaproc.CommandContext(t.Context(), 2*time.Minute, e.ffmpeg, args...).CombinedOutput(); err != nil {
			t.Fatalf("encode: %v %s", err, out)
		}
		out, err := mediaproc.CommandContext(t.Context(), 2*time.Minute, e.ffmpeg, "-hide_banner", "-i", path, "-f", "lavfi", "-i", "testsrc2=size=1920x1080:rate=30", "-filter_complex", "[0:v]setpts=PTS-STARTPTS[a];[1:v]setpts=PTS-STARTPTS[b];[a][b]ssim", "-t", "8", "-f", "null", "-").CombinedOutput()
		if err != nil {
			t.Fatalf("SSIM: %v %s", err, out)
		}
		matches := regexp.MustCompile(`All:([0-9.]+)`).FindAllSubmatch(out, -1)
		if len(matches) == 0 {
			t.Fatal(string(out))
		}
		score, _ := strconv.ParseFloat(string(matches[len(matches)-1][1]), 64)
		scores = append(scores, score)
		t.Logf("%s programme SSIM %.6f", mode, score)
	}
	if scores[1] < scores[0]-.003 {
		t.Fatalf("programme quality regression: %v", scores)
	}
}
