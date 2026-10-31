package transcode

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// Hardware accelerator names, as used in config.Transcode.HWAccel and
// core.HWAccel.Name.
const (
	AccelNone         = "none"
	AccelVideoToolbox = "videotoolbox"
	AccelNVENC        = "nvenc"
	AccelQSV          = "qsv"
	AccelVAAPI        = "vaapi"
)

// DefaultVAAPIDevice is the render node used when none is configured.
const DefaultVAAPIDevice = "/dev/dri/renderD128"

// features is what the ffmpeg build offers: encoders, hwaccels, filters.
type features struct {
	// scaleVTFormat is a build capability, not an FFmpeg version guess:
	// upstream builds can expose scale_vt without its format option.
	scaleVTFormat bool
	// Jellyfin patches the segment muxer's origin and first-entry duration.
	jellyfinSegmentClock bool
	encoders             map[string]bool
	hwaccels             map[string]bool
	filters              map[string]bool
}

func (f features) hasEncoder(n string) bool { return f.encoders[n] }
func (f features) hasFilter(n string) bool  { return f.filters[n] }

var (
	encoderLineRE = regexp.MustCompile(`^\s*([VASD][A-Z.]{5})\s+(\S+)`)
	filterLineRE  = regexp.MustCompile(`^\s*[A-Z.|]{2,3}\s+(\S+)\s+\S*->\S*`)
)

// parseEncoders parses `ffmpeg -hide_banner -encoders`.
func parseEncoders(out []byte) map[string]bool {
	m := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	started := false
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(strings.TrimSpace(line), "------") {
			started = true
			continue
		}
		if !started {
			continue
		}
		if mm := encoderLineRE.FindStringSubmatch(line); mm != nil {
			m[mm[2]] = true
		}
	}
	return m
}

// parseHWAccels parses `ffmpeg -hide_banner -hwaccels`.
func parseHWAccels(out []byte) map[string]bool {
	m := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	started := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "Hardware acceleration methods") {
			started = true
			continue
		}
		if started && line != "" && !strings.Contains(line, " ") {
			m[line] = true
		}
	}
	return m
}

// parseFilters parses `ffmpeg -hide_banner -filters`.
func parseFilters(out []byte) map[string]bool {
	m := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		if mm := filterLineRE.FindStringSubmatch(sc.Text()); mm != nil {
			m[mm[1]] = true
		}
	}
	return m
}

func probeFeatures(ctx context.Context, r Runner, ffmpeg string) (features, error) {
	run := func(args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		var out bytes.Buffer
		err := r.Run(ctx, Command{Path: ffmpeg, Args: append([]string{"-hide_banner"}, args...), Stdout: &out, Stderr: &out})
		if err != nil {
			return nil, fmt.Errorf("transcode: ffmpeg %v: %w", args, err)
		}
		return out.Bytes(), nil
	}
	var f features
	b, err := run("-encoders")
	if err != nil {
		return f, err
	}
	f.encoders = parseEncoders(b)
	if b, err = run("-hwaccels"); err != nil {
		return f, err
	}
	f.hwaccels = parseHWAccels(b)
	if b, err = run("-filters"); err != nil {
		return f, err
	}
	f.filters = parseFilters(b)
	if f.hasFilter("scale_vt") {
		// Optional probing cannot make a formerly usable build fail startup.
		if help, helpErr := run("-h", "filter=scale_vt"); helpErr == nil {
			f.scaleVTFormat = filterHasOption(help, "format")
		}
	}
	return f, nil
}

func filterHasOption(help []byte, option string) bool {
	for _, line := range strings.Split(string(help), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 1 && fields[0] == option && strings.HasPrefix(fields[1], "<") {
			return true
		}
	}
	return false
}

// accelCandidate is one hardware encoder family.
type accelCandidate struct {
	name     string
	h264     string
	hevc     string
	hwaccel  string // matching -hwaccels entry, "" when not required
	platform []string
}

var accelCandidates = []accelCandidate{
	{name: AccelVideoToolbox, h264: "h264_videotoolbox", hevc: "hevc_videotoolbox", hwaccel: "videotoolbox", platform: []string{"darwin"}},
	{name: AccelNVENC, h264: "h264_nvenc", hevc: "hevc_nvenc", platform: []string{"linux", "windows"}},
	{name: AccelQSV, h264: "h264_qsv", hevc: "hevc_qsv", platform: []string{"linux", "windows"}},
	{name: AccelVAAPI, h264: "h264_vaapi", hevc: "hevc_vaapi", hwaccel: "vaapi", platform: []string{"linux"}},
}

// testEncodeArgs is a 1-frame lavfi encode that proves an encoder works on
// this machine (drivers present, device usable).
func testEncodeArgs(accel, encoder, device string) []string {
	args := []string{"-hide_banner", "-nostdin", "-loglevel", "error"}
	vf := ""
	switch accel {
	case AccelVAAPI:
		args = append(args, "-vaapi_device", device)
		vf = "format=nv12,hwupload"
	case AccelQSV:
		vf = "format=nv12"
	case AccelNVENC, AccelVideoToolbox:
		vf = "format=yuv420p"
	}
	args = append(args, "-f", "lavfi", "-i", "testsrc=size=320x240:rate=1", "-frames:v", "1")
	if vf != "" {
		args = append(args, "-vf", vf)
	}
	return append(args, "-c:v", encoder, "-f", "null", "-")
}

// vaapiRCProbeArgs is a 1-frame lavfi encode that probes a specific VAAPI
// rate-control mode (QVBR, VBR, CBR, CQP). Some drivers (for example Debian's
// free iHD driver or LP-only EncSliceLP entrypoints) support CQP only and
// reject QVBR/VBR with "Driver does not support QVBR RC mode".
func vaapiRCProbeArgs(encoder, device, rcMode string) []string {
	args := []string{"-hide_banner", "-nostdin", "-loglevel", "error", "-vaapi_device", device,
		"-f", "lavfi", "-i", "testsrc=size=320x240:rate=1", "-frames:v", "1",
		"-vf", "format=nv12,hwupload", "-c:v", encoder}
	switch rcMode {
	case "QVBR":
		args = append(args, "-rc_mode", "QVBR", "-global_quality", "23", "-b:v", "1000k", "-maxrate", "1500k", "-bufsize", "2000k")
	case "VBR":
		args = append(args, "-rc_mode", "VBR", "-b:v", "1000k", "-maxrate", "1500k", "-bufsize", "2000k")
	case "CBR":
		args = append(args, "-rc_mode", "CBR", "-b:v", "1000k", "-maxrate", "1000k", "-bufsize", "2000k")
	case "CQP":
		args = append(args, "-rc_mode", "CQP", "-global_quality", "23")
	}
	return append(args, "-f", "null", "-")
}

// vaapiScaleArgs is a 1-frame encode through scale_vaapi, the GPU scaler
// real VAAPI sessions use. Some drivers encode but reject video
// processing ("the requested VAProfile is not supported"), which the
// encoder-only test cannot see.
func vaapiScaleArgs(encoder, device string) []string {
	return []string{"-hide_banner", "-nostdin", "-loglevel", "error", "-vaapi_device", device,
		"-f", "lavfi", "-i", "testsrc=size=320x240:rate=1", "-frames:v", "1",
		"-vf", "format=nv12,hwupload,scale_vaapi=w=160:h=120:format=nv12", "-c:v", encoder, "-f", "null", "-"}
}

// vaapiTonemapArgs is a 1-frame encode through tonemap_vaapi + scale_vaapi,
// the GPU HDR tone-mapping pipeline. Some VAAPI drivers support scale_vaapi
// but lack the HDR tone-mapping VPP kernel or OpenCL/Compute path.
func vaapiTonemapArgs(encoder, device string) []string {
	return []string{"-hide_banner", "-nostdin", "-loglevel", "error", "-vaapi_device", device,
		"-f", "lavfi", "-i", "testsrc=size=320x240:rate=1", "-frames:v", "1",
		"-vf", "format=p010le,setparams=color_primaries=bt2020:color_trc=smpte2084:colorspace=bt2020nc,hwupload,tonemap_vaapi=format=nv12:t=bt709:m=bt709:p=bt709,scale_vaapi=w=160:h=120:format=nv12",
		"-c:v", encoder, "-f", "null", "-"}
}

// hwDetector confirms hardware encoders.
type hwDetector struct {
	runner Runner
	ffmpeg string
	goos   string
	log    *slog.Logger
	// statDevice reports whether a VAAPI device node exists.
	statDevice func(string) bool
}

func (d hwDetector) testEncode(ctx context.Context, accel, encoder, device string) error {
	return d.testRun(ctx, testEncodeArgs(accel, encoder, device))
}

func (d hwDetector) testRun(ctx context.Context, args []string) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var stderr bytes.Buffer
	err := d.runner.Run(ctx, Command{Path: d.ffmpeg, Args: args, Stderr: &stderr})
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 300 {
			msg = msg[len(msg)-300:]
		}
		return fmt.Errorf("%w: %s", err, msg)
	}
	return nil
}

// probeVAAPIRCModes checks which rate-control modes (QVBR, VBR, CBR, CQP)
// a VAAPI encoder supports on device, returning the best mode for quality
// (VBV reservoir) runs and the best mode for explicit bitrate runs.
func (d hwDetector) probeVAAPIRCModes(ctx context.Context, encoder, device string) (qualityRC, bitrateRC string) {
	supported := map[string]bool{}
	for _, rc := range []string{"QVBR", "VBR", "CBR", "CQP"} {
		if err := d.testRun(ctx, vaapiRCProbeArgs(encoder, device, rc)); err == nil {
			supported[rc] = true
		}
	}
	for _, rc := range []string{"QVBR", "VBR", "CBR", "CQP"} {
		if supported[rc] {
			qualityRC = rc
			break
		}
	}
	for _, rc := range []string{"VBR", "CBR", "QVBR", "CQP"} {
		if supported[rc] {
			bitrateRC = rc
			break
		}
	}
	return qualityRC, bitrateRC
}

// detect picks the hardware accelerator per the configured mode ("auto",
// "none", or an explicit name). It always succeeds; the CPU fallback is
// AccelNone.
func (d hwDetector) detect(ctx context.Context, mode, vaapiDevice string, f features) core.HWAccel {
	cpu := core.HWAccel{Name: AccelNone}
	for _, e := range []string{"libx264", "libx265"} {
		if f.hasEncoder(e) {
			cpu.Encoders = append(cpu.Encoders, e)
		}
	}
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		mode = "auto"
	}
	if mode == AccelNone || mode == "cpu" || mode == "software" {
		d.log.Info("hardware acceleration disabled by config")
		return cpu
	}
	if vaapiDevice == "" {
		vaapiDevice = DefaultVAAPIDevice
	}
	for _, c := range accelCandidates {
		if mode == "auto" && !contains(c.platform, d.goos) {
			continue
		}
		if mode != "auto" && mode != c.name {
			continue
		}
		if !f.hasEncoder(c.h264) {
			if mode != "auto" {
				d.log.Warn("configured hardware encoder not in this ffmpeg build; using CPU", "hwaccel", c.name, "encoder", c.h264)
			}
			continue
		}
		device := ""
		if c.name == AccelVAAPI {
			device = vaapiDevice
			if d.statDevice != nil && !d.statDevice(device) {
				d.log.Info("VAAPI device missing", "device", device)
				continue
			}
		}
		if err := d.testEncode(ctx, c.name, c.h264, device); err != nil {
			d.log.Info("hardware encoder test failed", "hwaccel", c.name, "encoder", c.h264, "err", err)
			continue
		}
		hw := core.HWAccel{Name: c.name, Encoders: []string{c.h264}, Device: device}
		if c.name == AccelVAAPI {
			if qRC, bRC := d.probeVAAPIRCModes(ctx, c.h264, device); qRC != "" && (qRC != "QVBR" || bRC != "VBR") {
				hw.VAAPIQualityRC = map[string]string{c.h264: qRC}
				hw.VAAPIBitrateRC = map[string]string{c.h264: bRC}
				d.log.Info("VAAPI rate control mode selected", "encoder", c.h264, "qualityRC", qRC, "bitrateRC", bRC)
			}
			if f.hasFilter("scale_vaapi") {
				if err := d.testRun(ctx, vaapiScaleArgs(c.h264, device)); err != nil {
					hw.NoGPUScale = true
					d.log.Info("VAAPI video processing unavailable; scaling in software before the hardware encoder", "device", device, "err", err)
				} else if f.hasFilter("tonemap_vaapi") {
					if err := d.testRun(ctx, vaapiTonemapArgs(c.h264, device)); err != nil {
						hw.NoGPUTonemap = true
						d.log.Info("VAAPI HDR tone-mapping unavailable; tone-mapping in software", "device", device, "err", err)
					}
				}
			}
		}
		if f.hasEncoder(c.hevc) {
			if err := d.testEncode(ctx, c.name, c.hevc, device); err == nil {
				hw.Encoders = append(hw.Encoders, c.hevc)
				if c.name == AccelVAAPI {
					if qRC, bRC := d.probeVAAPIRCModes(ctx, c.hevc, device); qRC != "" && (qRC != "QVBR" || bRC != "VBR") {
						if hw.VAAPIQualityRC == nil {
							hw.VAAPIQualityRC = map[string]string{}
						}
						if hw.VAAPIBitrateRC == nil {
							hw.VAAPIBitrateRC = map[string]string{}
						}
						hw.VAAPIQualityRC[c.hevc] = qRC
						hw.VAAPIBitrateRC[c.hevc] = bRC
						d.log.Info("VAAPI rate control mode selected", "encoder", c.hevc, "qualityRC", qRC, "bitrateRC", bRC)
					}
				}
			} else {
				d.log.Info("hardware HEVC encoder test failed", "hwaccel", c.name, "encoder", c.hevc, "err", err)
			}
		}
		d.log.Info("hardware encoder detected", "hwaccel", hw.Name, "encoders", hw.Encoders, "device", hw.Device, "gpuScale", !hw.NoGPUScale)
		return hw
	}
	if mode != "auto" {
		d.log.Warn("configured hardware acceleration unavailable; using CPU", "hwaccel", mode)
	} else {
		d.log.Info("no hardware encoder found; using CPU", "encoders", cpu.Encoders)
	}
	return cpu
}

func deviceExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
