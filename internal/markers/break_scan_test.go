package markers

import (
	"context"
	"encoding/json"
	"github.com/braedonsaunders/ether/internal/store"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestBreakCandidateWindows(t *testing.T) {
	windows := breakCandidateWindows([]breakRun{{70, 71}, {72, 73}, {130, 130.3}}, 200)
	if len(windows) != 2 || windows[0].start != 68.5 || windows[0].end != 74.5 {
		t.Fatal(windows)
	}
	var rms rmsRuns
	rms.start = -1
	for _, line := range []string{"frame:0 pts_time:70", "lavfi.astats.Overall.RMS_level=-inf", "frame:1 pts_time:70.3", "lavfi.astats.Overall.RMS_level=-20"} {
		rms.line(line)
	}
	if len(rms.runs) != 1 || rms.runs[0].start != 70 {
		t.Fatal(rms)
	}
	for _, name := range []string{"videotoolbox", "vaapi", "qsv", "nvenc"} {
		if len(hardwareBreakArgs(core.HWAccel{Name: name})) == 0 {
			t.Fatal(name)
		}
	}
}

// An explicit optional offline cost/conformance fixture. The reference filter
// exists only here to compare the original full decode against the new strategy.
func TestBreakScanCostComparison(t *testing.T) {
	binary := os.Getenv("ETHER_BREAK_TEST_FFMPEG")
	if binary == "" || os.Getenv("ETHER_BREAK_TEST_COST") != "1" {
		t.Skip("set verified ffmpeg and ETHER_BREAK_TEST_COST=1")
	}
	type result struct {
		Fixture                                    string `json:"fixture"`
		DurationMS                                 int64  `json:"durationMs"`
		Bytes                                      int64  `json:"bytes"`
		BeforeCPU, BeforeWall, AfterCPU, AfterWall int64
		Points                                     []BreakPoint
		Evidence                                   BreakEvidence
	}
	var results []result
	for _, hd := range []bool{false, true} {
		name, video, codec := "synthetic", "color=white:s=160x90:r=10:d=200", "ffv1"
		vf := "drawbox=color=black:t=fill:enable='between(t,70,70.9)+between(t,130,130.9)'"
		if hd {
			name = "hd-h264"
			video = "testsrc2=s=320x180:r=30:d=200"
			codec = "libopenh264"
			vf = "scale=1280:720," + vf
		}
		file := filepath.Join(t.TempDir(), name+".mkv")
		args := []string{"-v", "error", "-nostdin", "-threads", "1", "-filter_threads", "1", "-f", "lavfi", "-i", video, "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=8000:duration=200", "-vf", vf, "-af", "volume='if(between(t,70,71),0,1)':eval=frame", "-c:v", codec, "-g", "60", "-threads", "1", "-c:a", "pcm_s16le"}
		if hd {
			args = append(args, "-b:v", "2000k")
		}
		args = append(args, file)
		if out, err := exec.Command(binary, args...).CombinedOutput(); err != nil {
			t.Fatalf("fixture %s: %v %s", name, err, out)
		}
		stat, err := os.Stat(file)
		if err != nil {
			t.Fatal(err)
		}
		sourceCodec := codec
		if hd {
			sourceCodec = "h264"
		}
		in := BreakInput{URL: file, Info: core.MediaInfo{Container: "mkv", Duration: 200 * time.Second, Size: stat.Size(), Streams: []core.StreamInfo{{Type: "video", Codec: sourceCodec}, {Type: "audio", Codec: "pcm_s16le"}}}}
		analyzer := BreakAnalyzer{FFmpeg: binary}
		refArgs := []string{"-hide_banner", "-nostdin", "-nostats", "-threads", "1", "-filter_threads", "1", "-i", file, "-t", strconv.FormatFloat(in.Info.Duration.Seconds(), 'f', 3, 64), "-map", "0:v:0", "-map", "0:a:0", "-vf", "fps=10,scale=160:90,blackdetect=d=0.3:pix_th=0.08:pic_th=0.98,scdet=threshold=8", "-af", "silencedetect=noise=-45dB:d=0.2", "-threads", "1", "-f", "null", "-"}
		began := time.Now()
		signals, _, cpu, err := analyzer.runBreakCommand(t.Context(), refArgs, false, in.Info.Duration.Seconds())
		beforeWall := time.Since(began)
		if err != nil {
			t.Fatal(err)
		}
		baseline := combineBreakSignals(signals, in.Info.Duration)
		e := newBreakEvidence(in)
		points, err := analyzer.decodeEvidence(t.Context(), in, &e)
		if err != nil {
			t.Fatal(err)
		}
		if len(baseline) != 1 || len(points) != 1 || points[0].AtMS < baseline[0].AtMS-200 || points[0].AtMS > baseline[0].AtMS+200 {
			t.Fatalf("precision changed: before=%+v after=%+v", baseline, points)
		}
		e.Complete = true
		e.Points = points
		r := result{name, in.Info.Duration.Milliseconds(), stat.Size(), cpu.Milliseconds(), beforeWall.Milliseconds(), e.CPUMS, e.WallMS, points, e}
		results = append(results, r)
		t.Logf("%s CPU ms %d -> %d, wall ms %d -> %d, mode=%s; CPU sec/video-hour %.3f -> %.3f", name, r.BeforeCPU, r.AfterCPU, r.BeforeWall, r.AfterWall, e.Mode, float64(r.BeforeCPU)*3.6e3/float64(r.DurationMS), float64(r.AfterCPU)*3.6e3/float64(r.DurationMS))
	}
	if dir := os.Getenv("ETHER_BREAK_TEST_ARTIFACTS"); dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		b, _ := json.MarshalIndent(results, "", "  ")
		if err := os.WriteFile(filepath.Join(dir, "scan-cost.json"), b, 0644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBreakTruncatedSuccessIsPartial(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	binary := filepath.Join(t.TempDir(), "truncated-ffmpeg")
	script := "#!/bin/sh\nprintf '%s\\n' 'black_start:70 black_end:71' 'silence_start:69.9' 'silence_end:71.1' 'frame=1290' 'out_time_us=129000000' 'progress=end' >&2\nexit 0\n"
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	db, err := store.OpenMemory(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var evidence BreakEvidence
	a := BreakAnalyzer{DB: db, FFmpeg: binary, OnEvidence: func(_ context.Context, _ BreakInput, e BreakEvidence) { evidence = e }}
	in := BreakInput{ReleaseKey: "cut-source", Revision: "cut-revision", Info: core.MediaInfo{Duration: 1497455 * time.Millisecond, Size: 1024, Streams: []core.StreamInfo{{Type: "audio"}}}}
	points, err := a.Analyze(t.Context(), in)
	if err == nil || evidence.Complete || !evidence.Partial || evidence.CoverageComplete || evidence.DecodedThroughMS != 129000 {
		t.Fatal("truncated exit0 accepted", err, evidence)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM markers_breaks_cache`).Scan(&count); err != nil || count != 0 {
		t.Fatal("truncated empty/full result persisted", count, err)
	}
	if len(points) != 1 {
		t.Fatal("verified earlier point was not retained as partial", points)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM markers_breaks_observed`).Scan(&count); err != nil || count != 1 {
		t.Fatal("partial facts not stored separately", count, err)
	}
}

func TestBreakHardwareFailureFallsBackToSoftware(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	binary := filepath.Join(t.TempDir(), "hardware-ffmpeg")
	script := "#!/bin/sh\nfor arg do\n if [ \"$arg\" = '-hwaccel' ]; then exit 1; fi\ndone\nprintf '%s\\n' 'black_start:70 black_end:71' 'silence_start:69.9' 'silence_end:71.1' 'frame=1800' 'out_time_us=180000000' 'progress=end' >&2\n"
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	a := BreakAnalyzer{FFmpeg: binary, Hardware: func() core.HWAccel { return core.HWAccel{Name: "vaapi", Device: "unavailable"} }}
	in := BreakInput{Info: core.MediaInfo{Duration: 3 * time.Minute}}
	evidence := newBreakEvidence(in)
	points, err := a.decodeEvidence(t.Context(), in, &evidence)
	if err != nil || len(points) != 1 || evidence.Backend != "software-fallback" || !evidence.CoverageComplete {
		t.Fatal(points, err, evidence)
	}
}
