package transcode

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestH264HighLevelUsesFrameRateGeometryAndBuffer(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		width, height        int
		fps                  float64
		peak, pictures, want int
	}{
		{"4k30", 3840, 2160, 30, 12000, 4, 51},
		{"4k60", 3840, 2160, 60, 12000, 4, 52},
		{"1080p30", 1920, 1080, 30, 12000, 4, 40},
		{"1080p60", 1920, 1080, 60, 12000, 4, 42},
		{"720p30", 1280, 720, 30, 12000, 4, 31},
		{"unknown fps", 3840, 2160, 0, 12000, 4, 0},
		{"too many references", 3840, 2160, 30, 12000, 7, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := H264HighLevel(tc.width, tc.height, tc.fps, tc.peak, tc.pictures); got != tc.want {
				t.Fatalf("got level %d, want %d", got, tc.want)
			}
		})
	}
}

// Streamyfin's 4K HEVC fallback exposed that ultrafast x264 can emit
// Constrained Baseline even with -profile:v high. Probe actual output rather
// than trusting the command line or the master-playlist codec string.
func TestH264FastStart4KOutputProfile(t *testing.T) {
	e := newRealEnv(t)
	info := core.MediaInfo{BitrateKbps: 8000, Streams: []core.StreamInfo{{Type: "video", Codec: "hevc", Width: 3840, Height: 2160, FrameRate: 30}}}
	s := runSpec{Info: info, FastStart: true, Decision: core.Decision{Kind: core.DecisionTranscode, VideoCodec: "h264", BitrateKbps: 8000, H264Level: 51, H264FrameRate: 30}}
	args := []string{"-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "color=size=3840x2160:rate=30", "-frames:v", "2"}
	args = append(args, planEncodeArgs(s, picturePlan{w: 3840, h: 2160}, "libx264", "h264", false)...)
	file := filepath.Join(e.dir, "faststart.mp4")
	args = append(args, file)
	if out, err := exec.Command(e.ffmpeg, args...).CombinedOutput(); err != nil {
		t.Fatalf("encode: %v: %s", err, out)
	}
	out, err := exec.Command(e.ffprobe, "-v", "error", "-select_streams", "v:0", "-show_entries", "stream=profile,level,width,height", "-of", "json", file).Output()
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Streams []struct {
			Profile string `json:"profile"`
			Level   int    `json:"level"`
			Width   int    `json:"width"`
			Height  int    `json:"height"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Streams) != 1 || result.Streams[0].Profile != "High" || result.Streams[0].Level != 51 || result.Streams[0].Width != 3840 || result.Streams[0].Height != 2160 {
		t.Fatalf("unexpected output %s", out)
	}
	if !strings.Contains(HLSVariantAttributes(info, s.Decision), "avc1.640033") {
		t.Fatal("playlist CODECS does not describe the selected SPS")
	}
	trace, err := exec.Command(e.ffmpeg, "-hide_banner", "-loglevel", "info", "-i", file, "-c", "copy", "-bsf:v", "trace_headers", "-f", "null", "-").CombinedOutput()
	if err != nil {
		t.Fatalf("SPS trace: %v: %s", err, trace)
	}
	for _, flag := range []string{"constraint_set0_flag", "constraint_set1_flag", "constraint_set2_flag", "constraint_set3_flag", "constraint_set4_flag", "constraint_set5_flag"} {
		matched := false
		for _, line := range strings.Split(string(trace), "\n") {
			if line = strings.TrimRight(line, "\r"); strings.Contains(line, flag) && strings.HasSuffix(line, "= 0") {
				matched = true
				break
			}
		}
		if !matched {
			t.Fatalf("SPS constraint flag %s does not match CODECS: %s", flag, trace)
		}
	}
}
