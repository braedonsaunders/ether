//go:build transcodechaos

package transcode

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/mediaproc"
)

// Opt-in media contract check; CHAOS_DV8 names a local DV8.1 + HDR10+ MKV.
// No test downloads media. Use the cached chaos corpus and the fixture script.
func TestTranscodeChaosDVBaseContract(t *testing.T) {
	input := os.Getenv("CHAOS_DV8")
	if input == "" {
		t.Skip("set CHAOS_DV8 to local DV8.1 + HDR10+ fixture")
	}
	for _, container := range []string{ContainerMatroska, ContainerFMP4} {
		for _, dv := range []bool{false, true} {
			name := container + "-base"
			if dv {
				name = container + "-dv"
			}
			t.Run(name, func(t *testing.T) {
				e := newRealEnv(t, WithoutHWDetection())
				data, err := os.ReadFile(input)
				if err != nil {
					t.Fatal(err)
				}
				src := e.writeTemp(t, "source.mkv", data)
				command := func(bin string, args ...string) []byte {
					t.Helper()
					b, err := mediaproc.CommandContext(t.Context(), 2*time.Minute, bin, args...).CombinedOutput()
					if err != nil {
						t.Fatalf("%s: %v: %s", bin, err, b)
					}
					return b
				}
				probe := command(e.ffprobe, "-v", "error", "-show_streams", "-of", "json", src)
				info, err := ParseProbe(probe)
				if err != nil {
					t.Fatal(err)
				}
				if v := info.Video(); v == nil || !v.DolbyVision || v.DolbyVisionProfile != 8 || v.DolbyVisionCompatibilityID != 1 {
					t.Fatalf("not DV8.1: %s", probe)
				}
				frames := command(e.ffprobe, "-v", "error", "-select_streams", "v", "-read_intervals", "%+#1", "-show_frames", "-of", "json", src)
				if !bytes.Contains(frames, []byte("HDR10+")) || !bytes.Contains(frames, []byte("Dolby Vision RPU")) {
					t.Fatalf("fixture lacks dynamic metadata: %s", frames)
				}
				caps := hdr10TV
				caps.Platform, caps.Device, caps.DolbyVision = "Android", "SHIELD Android TV", dv
				caps.HLSContainers = []string{container}
				opts := allow()
				opts.DirectPlay = false
				d := e.m.Decide(*info, caps, opts)
				if !d.VideoCopy || d.StripDolbyVision == dv {
					t.Fatalf("wrong decision: %+v", d)
				}
				hs, err := e.m.StartHLS(context.Background(), core.HLSRequest{SessionID: "dv-contract", InputURL: e.url("source.mkv"), Info: *info, Decision: d})
				if err != nil {
					t.Fatal(err)
				}
				defer hs.Close()
				body := fetch(t, hs, 0)
				body = append(append([]byte(nil), fetch(t, hs, InitSegment)...), body...)
				out := e.writeTemp(t, "output.mkv", body)
				result := command(e.ffprobe, "-v", "error", "-show_streams", "-of", "json", out)
				got, err := ParseProbe(result)
				if err != nil {
					t.Fatal(err)
				}
				if got.Video().DolbyVision != dv || got.Video().ColorTransfer != "smpte2084" {
					t.Fatalf("container mismatch: %s", result)
				}
				result = command(e.ffprobe, "-v", "error", "-select_streams", "v", "-read_intervals", "%+#1", "-show_frames", "-of", "json", out)
				if !bytes.Contains(result, []byte("HDR10+")) || bytes.Contains(result, []byte("Dolby Vision RPU")) != dv {
					t.Fatalf("frame metadata mismatch: %s", result)
				}
				attrs := HLSVariantAttributes(*info, d)
				if strings.Contains(attrs, "SUPPLEMENTAL-CODECS") != dv || !strings.Contains(attrs, "VIDEO-RANGE=PQ") {
					t.Fatal(attrs)
				}
				// Verify the actual elementary stream, not only ffprobe's container flags.
				raw := filepath.Join(e.dir, "out.hevc")
				command(e.ffmpeg, "-v", "error", "-y", "-i", out, "-c:v", "copy", "-bsf:v", "hevc_mp4toannexb", "-f", "hevc", raw)
				b, err := os.ReadFile(raw)
				if err != nil {
					t.Fatal(err)
				}
				rpus := 0
				for i := 0; i+4 < len(b); i++ {
					if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
						n := (b[i+3] >> 1) & 63
						if n == 62 || n == 63 {
							rpus++
						}
					}
				}
				if (rpus > 0) != dv {
					t.Fatalf("DV NAL count=%d dv=%v", rpus, dv)
				}
				if container == ContainerFMP4 && (!bytes.Contains(body, []byte("hvc1")) || (bytes.Contains(body, []byte("dvcC")) || bytes.Contains(body, []byte("dvvC"))) != dv) {
					t.Fatal("MP4 sample entry/config disagrees with decision")
				}
				srcRaw := filepath.Join(e.dir, "source.hevc")
				command(e.ffmpeg, "-v", "error", "-y", "-i", src, "-c:v", "copy", "-bsf:v", "hevc_mp4toannexb", "-f", "hevc", srcRaw)
				srcBytes, err := os.ReadFile(srcRaw)
				if err != nil {
					t.Fatal(err)
				}
				before, after := chaosVCL(srcBytes), chaosVCL(b)
				if len(after) == 0 || len(after) > len(before) || !reflect.DeepEqual(before[:len(after)], after) {
					t.Fatal("HDR base video NALs changed during copy")
				}

				t.Logf("%s DV config=%v DV NALs=%d HDR10+ retained; %s", name, got.Video().DolbyVision, rpus, attrs)
			})
		}
	}
}

// Ignore start-code width and non-VCL metadata; compare coded picture bytes.
func chaosVCL(b []byte) [][]byte {
	var out [][]byte
	start := -1
	for i := 0; i+3 < len(b); i++ {
		if b[i] != 0 || b[i+1] != 0 || b[i+2] != 1 {
			continue
		}
		end := i
		for end > 0 && b[end-1] == 0 {
			end--
		}
		if start >= 0 && (b[start]>>1)&63 < 32 {
			out = append(out, b[start:end])
		}
		start = i + 3
	}
	if start >= 0 && (b[start]>>1)&63 < 32 {
		out = append(out, bytes.TrimRight(b[start:], "\x00"))
	}
	return out
}
