package transcode

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestParseEncoders(t *testing.T) {
	m := parseEncoders([]byte(sampleEncoders))
	for _, want := range []string{"libx264", "libx265", "h264_videotoolbox", "h264_qsv", "aac", "webvtt"} {
		if !m[want] {
			t.Errorf("missing %s", want)
		}
	}
	for _, not := range []string{"=", "Video", "------"} {
		if m[not] {
			t.Errorf("legend parsed as encoder: %s", not)
		}
	}
}

func TestParseHWAccelsAndFilters(t *testing.T) {
	h := parseHWAccels([]byte("Hardware acceleration methods:\nvdpau\ncuda\nvaapi\nqsv\n\n"))
	if !reflect.DeepEqual(h, map[string]bool{"vdpau": true, "cuda": true, "vaapi": true, "qsv": true}) {
		t.Errorf("hwaccels %v", h)
	}
	f := parseFilters([]byte(sampleFilters))
	for _, want := range []string{"zscale", "tonemap", "scale_vt", "overlay", "subtitles", "buffer"} {
		if !f[want] {
			t.Errorf("filter %s missing", want)
		}
	}
	if f["="] || f["Timeline"] {
		t.Error("legend parsed as filter")
	}
}

func detector(fr *fakeRunner, goos string) hwDetector {
	return hwDetector{runner: fr, ffmpeg: "ffmpeg", goos: goos, log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		statDevice: func(string) bool { return true }}
}

func TestDetectHW(t *testing.T) {
	f := testFeatures()
	ctx := context.Background()
	tests := []struct {
		name   string
		goos   string
		mode   string
		fail   []string
		vaapi  string
		want   core.HWAccel
		noDev  bool
		encs   string
		checks func(t *testing.T, fr *fakeRunner)
	}{
		{name: "mac auto", goos: "darwin", mode: "auto",
			want: core.HWAccel{Name: AccelVideoToolbox, Encoders: []string{"h264_videotoolbox", "hevc_videotoolbox"}}},
		{name: "mac hevc test fails", goos: "darwin", mode: "", fail: []string{"hevc_videotoolbox"},
			want: core.HWAccel{Name: AccelVideoToolbox, Encoders: []string{"h264_videotoolbox"}}},
		{name: "linux auto: nvenc fails, qsv works", goos: "linux", mode: "auto", fail: []string{"h264_nvenc"},
			want: core.HWAccel{Name: AccelQSV, Encoders: []string{"h264_qsv"}}},
		{name: "linux auto: only vaapi works", goos: "linux", mode: "auto", fail: []string{"h264_nvenc", "h264_qsv"}, vaapi: "/dev/dri/renderD129",
			want: core.HWAccel{Name: AccelVAAPI, Encoders: []string{"h264_vaapi"}, Device: "/dev/dri/renderD129"},
			checks: func(t *testing.T, fr *fakeRunner) {
				last := fr.runCalls[len(fr.runCalls)-1]
				if argValue(last, "-vaapi_device") != "/dev/dri/renderD129" || !strings.Contains(joined(last), "hwupload") {
					t.Errorf("vaapi test encode args %v", last)
				}
			}},
		{name: "linux vaapi default device missing", goos: "linux", mode: "vaapi", noDev: true,
			want: core.HWAccel{Name: AccelNone, Encoders: []string{"libx264", "libx265"}}},
		{name: "explicit none", goos: "darwin", mode: "none",
			want: core.HWAccel{Name: AccelNone, Encoders: []string{"libx264", "libx265"}}},
		{name: "explicit nvenc on linux", goos: "linux", mode: "nvenc",
			want: core.HWAccel{Name: AccelNVENC, Encoders: []string{"h264_nvenc", "hevc_nvenc"}}},
		{name: "explicit but failing", goos: "linux", mode: "qsv", fail: []string{"h264_qsv"},
			want: core.HWAccel{Name: AccelNone, Encoders: []string{"libx264", "libx265"}}},
		{name: "windows auto skips videotoolbox and vaapi", goos: "windows", mode: "auto", fail: []string{"h264_nvenc", "h264_qsv"},
			want: core.HWAccel{Name: AccelNone, Encoders: []string{"libx264", "libx265"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fr := newFakeRunner()
			fr.failEncode = map[string]bool{}
			for _, e := range tt.fail {
				fr.failEncode[e] = true
			}
			d := detector(fr, tt.goos)
			if tt.noDev {
				d.statDevice = func(string) bool { return false }
			}
			got := d.detect(ctx, tt.mode, tt.vaapi, f)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %+v want %+v", got, tt.want)
			}
			if tt.checks != nil {
				tt.checks(t, fr)
			}
		})
	}
}

func TestTestEncodeArgs(t *testing.T) {
	a := testEncodeArgs(AccelVideoToolbox, "h264_videotoolbox", "")
	mustContain(t, a, "-f lavfi -i testsrc=size=320x240:rate=1 -frames:v 1", "-c:v h264_videotoolbox -f null -")
}

func TestFinder(t *testing.T) {
	dir := t.TempDir()
	mk := func(p string) string {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	noPath := func(string) (string, error) { return "", os.ErrNotExist }

	// macOS app bundle: Contents/MacOS/ether + Contents/Resources/ffmpeg
	macExe := filepath.Join(dir, "Ether.app", "Contents", "MacOS")
	ff := mk(filepath.Join(dir, "Ether.app", "Contents", "Resources", "ffmpeg"))
	fp := mk(filepath.Join(dir, "Ether.app", "Contents", "Resources", "ffprobe"))
	f := finder{exeDir: macExe, goos: "darwin", lookPath: noPath, isExec: isExecutable}
	gotF, gotP, err := f.discover("", "")
	if err != nil || gotF != ff || gotP != fp {
		t.Fatalf("bundle: %s %s %v", gotF, gotP, err)
	}

	// <exe>/bin/ on windows with .exe
	winExe := filepath.Join(dir, "win")
	wf := mk(filepath.Join(winExe, "bin", "ffmpeg.exe"))
	wp := mk(filepath.Join(winExe, "bin", "ffprobe.exe"))
	f = finder{exeDir: winExe, goos: "windows", lookPath: noPath, isExec: isExecutable}
	if gotF, gotP, err = f.discover("", ""); err != nil || gotF != wf || gotP != wp {
		t.Fatalf("windows: %s %s %v", gotF, gotP, err)
	}

	// Configured path wins; a directory is accepted.
	cf := mk(filepath.Join(dir, "custom", "ffmpeg"))
	cp := mk(filepath.Join(dir, "custom", "ffprobe"))
	f = finder{exeDir: macExe, goos: "darwin", lookPath: noPath, isExec: isExecutable}
	if gotF, gotP, err = f.discover(cf, ""); err != nil || gotF != cf || gotP != cp {
		t.Fatalf("configured: %s %s %v", gotF, gotP, err)
	}
	if gotF, _, err = f.discover(filepath.Join(dir, "custom"), ""); err != nil || gotF != cf {
		t.Fatalf("configured dir: %s %v", gotF, err)
	}
	if _, _, err = f.discover(filepath.Join(dir, "nope", "ffmpeg"), ""); err == nil {
		t.Fatal("missing configured path should fail")
	}

	// PATH fallback.
	f = finder{exeDir: filepath.Join(dir, "empty"), goos: "linux", isExec: isExecutable,
		lookPath: func(n string) (string, error) { return "/usr/bin/" + n, nil }}
	if gotF, gotP, err = f.discover("", ""); err != nil || gotF != "/usr/bin/ffmpeg" || gotP != "/usr/bin/ffprobe" {
		t.Fatalf("PATH: %s %s %v", gotF, gotP, err)
	}

	// Nothing anywhere.
	f = finder{exeDir: filepath.Join(dir, "empty"), goos: "linux", lookPath: noPath, isExec: isExecutable}
	if _, _, err = f.discover("", ""); err == nil || !strings.Contains(err.Error(), "ffmpeg not found") {
		t.Fatalf("err = %v", err)
	}
	// Linux system location is a candidate.
	if c := (finder{goos: "linux"}).candidates("ffmpeg"); !contains(c, "/usr/lib/ether/ffmpeg") {
		t.Errorf("linux candidates %v", c)
	}
}

func TestParseVersion(t *testing.T) {
	cases := map[string]string{
		"ffmpeg version 8.0.1 Copyright (c) 2000-2025 the FFmpeg developers\nbuilt with": "8.0.1",
		"ffmpeg version n7.1-12-gabc Copyright":                                          "n7.1-12-gabc",
		"ffprobe version 6.1.1-3ubuntu5 Copyright":                                       "6.1.1-3ubuntu5",
		"garbage": "",
	}
	for in, want := range cases {
		if got := parseVersion([]byte(in)); got != want {
			t.Errorf("parseVersion(%q) = %q want %q", in, got, want)
		}
	}
}

// Issue 13: an Intel HD 620 encodes with h264_vaapi but rejects
// scale_vaapi ("the requested VAProfile is not supported"). Detection
// keeps the hardware encoder and marks GPU scaling unavailable.
func TestDetectVAAPIScaleProbe(t *testing.T) {
	f := testFeatures()
	f.filters = map[string]bool{"scale_vaapi": true}
	for _, tc := range []struct {
		name        string
		scaleFails  bool
		wantNoScale bool
	}{
		{"scaler works", false, false},
		{"scaler unsupported", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fr := &fakeRunner{failEncode: map[string]bool{"h264_nvenc": true, "h264_qsv": true, "scale_vaapi": tc.scaleFails}}
			got := detector(fr, "linux").detect(context.Background(), "auto", "/dev/dri/renderD128", f)
			if got.Name != AccelVAAPI || len(got.Encoders) == 0 || got.Encoders[0] != "h264_vaapi" {
				t.Fatalf("detected %+v", got)
			}
			if got.NoGPUScale != tc.wantNoScale {
				t.Fatalf("NoGPUScale=%v want %v", got.NoGPUScale, tc.wantNoScale)
			}
			probed := false
			for _, args := range fr.runCalls {
				if strings.Contains(argValue(args, "-vf"), "scale_vaapi") && argValue(args, "-vaapi_device") == "/dev/dri/renderD128" {
					probed = true
				}
			}
			if !probed {
				t.Fatal("scale_vaapi was not probed")
			}
		})
	}
}
