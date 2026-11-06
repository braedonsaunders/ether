package transcode

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/mediaproc"
)

// sdrBars is an 8-bit BT.709 SDR fixture with exact limited-range luma:
// reference white (235) on the left half, mid grey (126) on the right.
func sdrBars(size string) []string {
	return []string{"-vf", "scale=" + size + ",format=yuv420p,geq=lum='if(lt(X,W/2),235,126)':cb=128:cr=128",
		"-c:v", "libx264", "-preset", "ultrafast", "-g", "24", "-keyint_min", "24", "-sc_threshold", "0",
		"-color_primaries", "bt709", "-color_trc", "bt709", "-colorspace", "bt709", "-color_range", "tv", "-c:a", "flac"}
}

type frameFormat struct {
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	PixFmt    string `json:"pix_fmt"`
	SAR       string `json:"sample_aspect_ratio"`
	Range     string `json:"color_range"`
	Transfer  string `json:"color_transfer"`
	Primaries string `json:"color_primaries"`
	Time      string `json:"best_effort_timestamp_time"`
}

func (e *realEnv) frameFormats(t *testing.T, file string) []frameFormat {
	t.Helper()
	out, err := mediaproc.CommandContext(t.Context(), 2*time.Minute, e.ffprobe, "-v", "error", "-select_streams", "v:0", "-show_entries",
		"frame=width,height,pix_fmt,sample_aspect_ratio,color_range,color_transfer,color_primaries,best_effort_timestamp_time",
		"-of", "json", file).Output()
	if err != nil {
		t.Fatalf("ffprobe frames: %v", err)
	}
	var v struct {
		Frames []frameFormat `json:"frames"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatal(err)
	}
	return v.Frames
}

// pixel decodes the first frame at or after at and returns 10-bit Y, Cb,
// Cr at (x, y).
func (e *realEnv) pixel(t *testing.T, file string, at float64, x, y int) (int, int, int) {
	t.Helper()
	out, err := mediaproc.CommandContext(t.Context(), 2*time.Minute, e.ffmpeg, "-v", "error", "-i", file, "-map", "0:v:0",
		"-vf", "select='gte(t\\,"+strconv.FormatFloat(at, 'f', 3, 64)+")'", "-frames:v", "1",
		"-f", "rawvideo", "-pix_fmt", "yuv420p10le", "-").Output()
	if err != nil {
		t.Fatalf("decode frame at %.2f: %v", at, err)
	}
	f := e.frameFormats(t, file)[0]
	w, h := f.Width, f.Height
	if len(out) != w*h*3 { // 10-bit 4:2:0: 1.5 samples per pixel, 2 bytes each
		t.Fatalf("frame at %.2f: %d bytes for %dx%d", at, len(out), w, h)
	}
	sample := func(off int) int { return int(out[off]) | int(out[off+1])<<8 }
	cw := w / 2
	cb := w * h * 2
	cr := cb + cw*(h/2)*2
	return sample((y*w + x) * 2), sample(cb + ((y/2)*cw+x/2)*2), sample(cr + ((y/2)*cw+x/2)*2)
}

// pqCode is the 10-bit limited-range PQ code value of an SDR luma code
// (8-bit, limited) mapped as in Report ITU-R BT.2408-7 §5.1.2: BT.1886
// EOTF (gamma 2.4), 100% SDR at 203 cd/m², then the PQ inverse EOTF.
func pqCode(sdr8 int) float64 {
	v := float64(sdr8-16) / 219
	l := math.Pow(v, 2.4) * 203 / 10000
	const m1, m2, c1, c2, c3 = 2610.0 / 16384, 2523.0 / 4096 * 128, 3424.0 / 4096, 2413.0 / 4096 * 32, 2392.0 / 4096 * 32
	lm := math.Pow(l, m1)
	e := math.Pow((c1+c2*lm)/(1+c3*lm), m2)
	return 64 + e*876
}

type contractSwap struct {
	file string
	at   time.Duration
}

// playStatic runs a segmented-Matroska session the way Plex's native
// player consumes it: the opening EXT-X-MAP header and then every Cluster
// appended, across the given failovers. It returns the joined file and,
// per swap, whether the video was copied and whether its run used the
// hardware encoder.
func (e *realEnv) playStatic(t *testing.T, name, original string, swaps []contractSwap, segments int) (string, []bool, []bool) {
	t.Helper()
	ctx := context.Background()
	info, err := e.m.Probe(ctx, e.url(original))
	if err != nil {
		t.Fatal(err)
	}
	caps := appleTV
	caps.HLSContainers = []string{"mkv"}
	opts := allow()
	opts.DirectPlay = false
	d := e.m.Decide(*info, caps, opts)
	if d.Container != ContainerMatroska || !d.VideoCopy {
		t.Fatalf("decision: %+v", d)
	}
	hs, err := e.m.StartHLS(ctx, core.HLSRequest{SessionID: "it-contract-" + name, InputURL: e.url(original), Info: *info, Decision: d})
	if err != nil {
		t.Fatal(err)
	}
	defer hs.Close()
	joined := append([]byte(nil), fetch(t, hs, InitSegment)...)
	next := 0
	var copied, hw []bool
	for i, sw := range swaps {
		end := int(sw.at / (4 * time.Second))
		for ; next < end; next++ {
			joined = append(joined, fetch(t, hs, next)...)
		}
		if i > 0 {
			hw = append(hw, hs.(*Session).HWAccelerated())
		}
		other, err := e.m.Probe(ctx, e.url(sw.file))
		if err != nil {
			t.Fatal(err)
		}
		if err := hs.(*Session).CheckSwap(*other); err != nil {
			t.Fatalf("preflight %s: %v", sw.file, err)
		}
		if err := hs.SwapInput(ctx, e.url(sw.file), *other, sw.at); err != nil {
			t.Fatal(err)
		}
		nd := hs.(*Session).Decision()
		copied = append(copied, nd.VideoCopy)
		t.Logf("swap to %s at %v: copy=%v %s", sw.file, sw.at, nd.VideoCopy, nd.Reason)
	}
	for ; next < segments; next++ {
		joined = append(joined, fetch(t, hs, next)...)
	}
	hw = append(hw, hs.(*Session).HWAccelerated())
	path := e.writeTemp(t, name+"-static.mkv", joined)
	if out, err := mediaproc.CommandContext(t.Context(), 2*time.Minute, e.ffmpeg, "-v", "error", "-xerror", "-i", path, "-map", "0:v", "-f", "null", "-").CombinedOutput(); err != nil {
		t.Fatalf("original header + replacement clusters: %v\n%s", err, out)
	}
	return path, copied, hw
}

// assertUniform checks that every decoded frame has the header's size,
// pixel format, sample aspect ratio and range.
func assertUniform(t *testing.T, frames []frameFormat, w, h int, pix string, transfers ...string) {
	t.Helper()
	if len(frames) < 4*4*24-2 {
		t.Fatalf("decoded %d frames", len(frames))
	}
	for i, f := range frames {
		okTRC := false
		for _, trc := range transfers {
			okTRC = okTRC || f.Transfer == trc
		}
		if f.Width != w || f.Height != h || f.PixFmt != pix || (f.SAR != "1:1" && f.SAR != "") || f.Range != "tv" || !okTRC {
			t.Fatalf("frame %d at %s: %+v, want %dx%d %s 1:1 tv %v\n%s", i, f.Time, f, w, h, pix, transfers, formatRuns(frames))
		}
	}
}

// TestIntegrationMatroskaContractPillarboxesTenBitSDR reproduces the live
// Yellowstone session: 10-bit SDR HEVC at 2.00:1 without colour tags, then
// 8-bit 16:9 H.264. The replacement must decode under the opening header
// as 10-bit frames of the same size, pillarboxed, with SDR levels intact.
func TestIntegrationMatroskaContractPillarboxesTenBitSDR(t *testing.T) {
	testContractPillarbox(t, false)
}

// TestIntegrationVideoToolboxContractPillarboxesTenBitSDR is the same
// session with the failover encoded by hevc_videotoolbox (Main 10 from
// software-padded p010 frames). It skips where no hardware encoder ran.
func TestIntegrationVideoToolboxContractPillarboxesTenBitSDR(t *testing.T) {
	testContractPillarbox(t, true)
}

func testContractPillarbox(t *testing.T, hw bool) {
	var opts []Option
	if !hw {
		opts = append(opts, WithoutHWDetection())
	}
	e := newRealEnv(t, opts...)
	e.gen(t, "remux.mkv", 24, "-vf", "scale=640:320,setsar=1", "-c:v", "libx265", "-preset", "ultrafast", "-pix_fmt", "yuv420p10le",
		"-x265-params", "pools=1:frame-threads=1:keyint=24:min-keyint=24:scenecut=0", "-c:a", "flac")
	e.gen(t, "web.mkv", 24, sdrBars("640:360")...)
	path, _, ran := e.playStatic(t, "yellowstone", "remux.mkv", []contractSwap{{"web.mkv", 8 * time.Second}}, 5)
	if hw && !ran[0] {
		t.Skip("no hardware encoder ran the failover")
	}
	assertUniform(t, e.frameFormats(t, path), 640, 320, "yuv420p10le", "unknown", "", "bt709")
	// 640x360 fits 568x320 inside 640x320: 36-pixel bars each side.
	for _, p := range []struct {
		x       int
		y, c    int
		subject string
	}{
		{10, 64, 512, "bar"}, {630, 64, 512, "bar"}, {150, 940, 512, "white"}, {480, 504, 512, "grey"},
	} {
		y, cb, cr := e.pixel(t, path, 13, p.x, 160)
		t.Logf("%s at x=%d: YCbCr %d,%d,%d", p.subject, p.x, y, cb, cr)
		if abs(y-p.y) > 3 || abs(cb-p.c) > 3 || abs(cr-p.c) > 3 {
			t.Errorf("%s at x=%d: YCbCr %d,%d,%d, want %d,%d,%d", p.subject, p.x, y, cb, cr, p.y, p.c, p.c)
		}
	}
}

// TestIntegrationMatroskaContractConvertsSDRIntoHDR10 opens on HDR10, fails
// over to SDR, then back to HDR10. The SDR pictures are converted to PQ
// pixels at BT.2408 levels (not relabelled), and the return is a copy.
func TestIntegrationMatroskaContractConvertsSDRIntoHDR10(t *testing.T) {
	testContractSDRIntoHDR10(t, false)
}

// TestIntegrationVideoToolboxContractConvertsSDRIntoHDR10 encodes the
// converted SDR with hevc_videotoolbox. It skips where no hardware
// encoder ran.
func TestIntegrationVideoToolboxContractConvertsSDRIntoHDR10(t *testing.T) {
	testContractSDRIntoHDR10(t, true)
}

func testContractSDRIntoHDR10(t *testing.T, hw bool) {
	var opts []Option
	if !hw {
		opts = append(opts, WithoutHWDetection())
	}
	e := newRealEnv(t, opts...)
	if out, err := mediaproc.CommandContext(t.Context(), 2*time.Minute, e.ffmpeg, "-hide_banner", "-filters").Output(); err != nil || !strings.Contains(string(out), " zscale ") {
		t.Skip("ffmpeg without zscale cannot convert SDR to PQ (the bundled jellyfin-ffmpeg has it)")
	}
	// FFmpeg 8 takes encoder colour from the frames: tag them, and the
	// x265 VUI, so the Matroska Colour element and the probe say PQ.
	e.gen(t, "remux.mkv", 24, append([]string{"-vf", "setparams=color_primaries=bt2020:color_trc=smpte2084:colorspace=bt2020nc"},
		hdr10x265("ctu=64:open-gop=0:colorprim=bt2020:transfer=smpte2084:colormatrix=bt2020nc")...)...)
	e.gen(t, "web.mkv", 24, sdrBars("640:360")...)
	path, copied, ran := e.playStatic(t, "hdr-sdr-hdr", "remux.mkv", []contractSwap{
		{"web.mkv", 8 * time.Second},
		{"remux.mkv", 16 * time.Second},
	}, 6)
	if hw && !ran[0] {
		t.Skip("no hardware encoder ran the failover")
	}
	frames := e.frameFormats(t, path)
	assertUniform(t, frames, 640, 360, "yuv420p10le", "smpte2084")
	for _, f := range frames {
		if f.Primaries != "bt2020" {
			t.Fatalf("frame at %s: primaries %q", f.Time, f.Primaries)
		}
	}
	white, grey := pqCode(235), pqCode(126)
	if math.Abs(white-572.7) > 1 {
		t.Fatalf("PQ reference: 100%% SDR maps to code %.1f, want 58%% PQ", white)
	}
	for _, p := range []struct {
		x    int
		want float64
	}{{150, white}, {480, grey}} {
		y, cb, cr := e.pixel(t, path, 13, p.x, 180)
		t.Logf("x=%d: YCbCr %d,%d,%d (BT.2408 Y %.1f)", p.x, y, cb, cr, p.want)
		if math.Abs(float64(y)-p.want) > 4 || abs(cb-512) > 3 || abs(cr-512) > 3 {
			t.Errorf("x=%d: YCbCr %d,%d,%d, want Y %.1f neutral", p.x, y, cb, cr, p.want)
		}
	}
	// The original's own pictures return as a stream copy.
	if len(copied) != 2 || copied[0] || !copied[1] {
		t.Fatalf("video copy per swap = %v, want [false true]", copied)
	}
}

// formatRuns summarises consecutive frames sharing one format.
func formatRuns(frames []frameFormat) string {
	var out string
	start := 0
	for i := 1; i <= len(frames); i++ {
		if i < len(frames) {
			a, b := frames[i], frames[start]
			a.Time, b.Time = "", ""
			if a == b {
				continue
			}
		}
		f := frames[start]
		out += fmt.Sprintf("  %s..%s: %dx%d %s sar=%s %s/%s/%s\n", f.Time, frames[i-1].Time, f.Width, f.Height, f.PixFmt, f.SAR, f.Range, f.Transfer, f.Primaries)
		start = i
	}
	return out
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
