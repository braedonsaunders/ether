package transcode

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

// Exercise observed scan/rotation evidence with real, tiny encoded sources.
// No packet scans or external services are needed by the native probe.
func TestProbeScanFacts(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe unavailable")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command(ffmpeg, append([]string{"-v", "error", "-y"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("ffmpeg: %v: %s", err, out)
		}
	}
	probe := func(path string) *core.MediaInfo {
		t.Helper()
		raw, err := exec.Command(ffprobe, "-v", "error", "-show_streams", "-show_data", "-show_format", "-of", "json", path).Output()
		if err != nil {
			t.Fatal(err)
		}
		info, err := ParseProbe(raw)
		if err != nil {
			t.Fatal(err)
		}
		return info
	}
	native := func(path string) *core.MediaInfo {
		t.Helper()
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		info, err := ProbeSource(context.Background(), bytes.NewReader(raw), int64(len(raw)))
		if err != nil {
			t.Fatal(err)
		}
		return info
	}
	for _, tc := range []struct {
		name, ext, codec string
		interlaced       bool
		args             []string
	}{
		{"avc-progressive", ".mp4", "libx264", false, nil},
		{"avc-tff", ".mkv", "libx264", true, []string{"-flags", "+ilme+ildct", "-x264-params", "tff=1"}},
		{"avc-bff", ".mkv", "libx264", true, []string{"-flags", "+ilme+ildct", "-x264-params", "bff=1"}},
		{"hevc-progressive", ".mp4", "libx265", false, []string{"-x265-params", "pools=none:frame-threads=1:log-level=error"}},
		{"hevc-tff", ".mkv", "libx265", true, []string{"-x265-params", "pools=none:frame-threads=1:log-level=error:interlace=1", "-field_order", "tt"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, tc.name+tc.ext)
			args := []string{"-f", "lavfi", "-i", "testsrc2=size=64x64:rate=25", "-t", "0.24", "-an", "-c:v", tc.codec, "-threads", "1", "-preset", "ultrafast"}
			args = append(args, tc.args...)
			run(append(args, path)...)
			want, got := probe(path).Video(), native(path).Video()
			if want.ProbedInterlaced == nil || *want.ProbedInterlaced != tc.interlaced {
				t.Fatalf("ffprobe scan: %+v", want)
			}
			if got.ProbedInterlaced == nil || *got.ProbedInterlaced != tc.interlaced {
				t.Fatalf("native scan: %+v", got)
			}
			if !(tc.codec == "libx265" && tc.interlaced) && got.FieldOrder != want.FieldOrder {
				t.Fatalf("field order native=%q ffprobe=%q", got.FieldOrder, want.FieldOrder)
			}
			if !tc.interlaced && tc.codec == "libx264" {
				for _, degrees := range []string{"90", "-90", "180"} {
					rotated := filepath.Join(dir, "rotated"+degrees+".mp4")
					run("-display_rotation", degrees, "-i", path, "-c", "copy", rotated)
					a, b := probe(rotated).Video().Rotation, native(rotated).Video().Rotation
					if a == nil || b == nil || math.Abs(*a-*b) > 0.001 {
						t.Fatalf("rotation %s: ffprobe=%v native=%v", degrees, a, b)
					}
				}
			}
		})
	}
	t.Run("unknown-and-explicit-zero", func(t *testing.T) {
		for _, raw := range []string{`{"streams":[{"codec_type":"video","codec_name":"hevc"}]}`, `{"streams":[{"codec_type":"video","codec_name":"hevc","field_order":"unknown"}]}`} {
			info, err := ParseProbe([]byte(raw))
			if err != nil {
				t.Fatal(err)
			}
			v := info.Video()
			if v.ProbedInterlaced != nil || v.FieldOrder != "" || v.Rotation != nil {
				t.Fatalf("invented facts %+v", v)
			}
		}
		info, err := ParseProbe([]byte(`{"streams":[{"codec_type":"video","codec_name":"hevc","field_order":"progressive","side_data_list":[{"side_data_type":"Display Matrix","rotation":0}]}]}`))
		if err != nil {
			t.Fatal(err)
		}
		v := info.Video()
		if v.Rotation == nil || *v.Rotation != 0 || v.ProbedInterlaced == nil || *v.ProbedInterlaced {
			t.Fatalf("lost zero/false %+v", v)
		}
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		var stored core.StreamInfo
		if err := json.Unmarshal(raw, &stored); err != nil {
			t.Fatal(err)
		}
		if stored.Rotation == nil || stored.ProbedInterlaced == nil {
			t.Fatal("lost persisted evidence")
		}
	})
}
