package markers

import (
	"bufio"
	"context"
	"fmt"
	"math"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

const tinyLuma = "scale=160:90:flags=fast_bilinear:out_range=limited,format=yuv420p,extractplanes=y,format=gray,setparams=range=limited"
const breakRMSFilter = "aformat=sample_rates=8000:channel_layouts=mono,asetnsamples=n=160:p=1,astats=metadata=1:reset=1:measure_perchannel=none:measure_overall=RMS_level,ametadata=print:key=lavfi.astats.Overall.RMS_level"

type breakScanPass struct {
	start, end float64
	sparse     bool
}
type rmsRuns struct {
	at, start, last float64
	runs            []breakRun
}

var rmsPTS = regexp.MustCompile(`pts_time:([0-9.]+)`)

func (r *rmsRuns) line(line string) {
	if match := rmsPTS.FindStringSubmatch(line); len(match) > 0 {
		r.at, _ = strconv.ParseFloat(match[1], 64)
	}
	const key = "lavfi.astats.Overall.RMS_level="
	if i := strings.Index(line, key); i >= 0 {
		value := strings.TrimSpace(line[i+len(key):])
		rms, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return
		}
		quiet := !math.IsNaN(rms) && rms <= -45
		if quiet && r.start < 0 {
			r.start = r.at
		}
		if !quiet && r.start >= 0 {
			if r.at-r.start >= .16 {
				r.runs = append(r.runs, breakRun{r.start, r.at})
			}
			r.start = -1
		}
		r.last = r.at + .02
	}
}
func (r *rmsRuns) finish() {
	if r.start >= 0 && r.last-r.start >= .16 {
		r.runs = append(r.runs, breakRun{r.start, r.last})
	}
	r.start = -1
}

func breakCandidateWindows(runs []breakRun, duration float64) []breakRun {
	var windows []breakRun
	for _, r := range runs {
		if r.end < 59 || r.start > duration-59 {
			continue
		}
		window := breakRun{math.Max(0, r.start-1.5), math.Min(duration, r.end+1.5)}
		if len(windows) > 0 && window.start <= windows[len(windows)-1].end+1 {
			windows[len(windows)-1].end = math.Max(windows[len(windows)-1].end, window.end)
		} else {
			windows = append(windows, window)
		}
	}
	return windows
}
func appendBreakSignals(dst *breakSignals, src breakSignals, offset float64) {
	for _, r := range src.black {
		dst.black = append(dst.black, breakRun{r.start + offset, r.end + offset})
	}
	for _, r := range src.silent {
		dst.silent = append(dst.silent, breakRun{r.start + offset, r.end + offset})
	}
	for _, at := range src.scenes {
		dst.scenes = append(dst.scenes, at+offset)
	}
	for _, event := range src.sceneEvidence {
		event.StartMS += int64(math.Round(offset * 1000))
		event.EndMS += int64(math.Round(offset * 1000))
		dst.sceneEvidence = append(dst.sceneEvidence, event)
	}
}
func hardwareBreakArgs(hw core.HWAccel) []string {
	name := hw.Name
	if name == "nvenc" {
		name = "cuda"
	}
	switch name {
	case "videotoolbox", "vaapi", "qsv", "cuda":
		out := []string{"-hwaccel", name}
		if hw.Device != "" {
			out = append(out, "-hwaccel_device", hw.Device)
		}
		return out
	}
	return nil
}

func (a *BreakAnalyzer) decodeStrategy(ctx context.Context, in BreakInput, evidence *BreakEvidence) ([]BreakPoint, error) {
	started := time.Now()
	var cpu time.Duration
	defer func() {
		evidence.WallMS = time.Since(started).Milliseconds()
		if a.OnCost != nil {
			a.OnCost(in.Info.Duration, cpu, time.Since(started))
		}
	}()
	hw := core.HWAccel{}
	if a.Hardware != nil {
		hw = a.Hardware()
	}
	evidence.Backend = "software"
	if len(hardwareBreakArgs(hw)) > 0 {
		evidence.Backend = hw.Name
	}
	run := func(pass breakScanPass) (breakSignals, []breakRun, error) {
		signals, rms, cost, err := a.runBreakPass(ctx, in, pass, hw)
		evidence.DecodedThroughMS = max(evidence.DecodedThroughMS, int64(math.Round(pass.start*1000))+signals.progressMS)
		cpu += cost
		evidence.CPUMS = cpu.Milliseconds()
		if err != nil && len(hardwareBreakArgs(hw)) > 0 && ctx.Err() == nil {
			hw = core.HWAccel{}
			evidence.Backend = "software-fallback"
			signals, rms, cost, err = a.runBreakPass(ctx, in, pass, hw)
			cpu += cost
			evidence.CPUMS = cpu.Milliseconds()
		}
		evidence.DecodedThroughMS = max(evidence.DecodedThroughMS, int64(math.Round(pass.start*1000))+signals.progressMS)
		if err != nil {
			evidence.CoverageComplete = false
		}
		return signals, rms, err
	}
	evidence.CoverageComplete = true
	var combined breakSignals
	defer func() {
		evidence.Black = breakRunEvidence(combined.black)
		evidence.Silence = breakRunEvidence(combined.silent)
		evidence.Scenes = combined.sceneEvidence
	}()
	duration := in.Info.Duration.Seconds()
	seekable := in.Info.Container == "mkv" || in.Info.Container == "mp4"
	if video := in.Info.Video(); video != nil {
		switch video.Codec {
		case "ffv1", "rawvideo", "mjpeg", "prores":
			seekable = false
		}
	}
	if !seekable {
		evidence.Mode = "full-luma"
		signals, _, err := run(breakScanPass{end: duration})
		combined = signals
		evidence.Decoded = true
		if err != nil {
			combined = completedBreakPrefix(combined)
			return combineBreakSignals(combined, in.Info.Duration), err
		}
		evidence.Decoded = true
		return combineBreakSignals(combined, in.Info.Duration), nil
	}
	evidence.Mode = "audio-keyframe-confirm"
	sparse, rms, err := run(breakScanPass{end: duration, sparse: true})
	evidence.Decoded = true
	evidence.AudioCandidates = breakRunEvidence(rms)
	evidence.KeyframeBlack = breakRunEvidence(sparse.black)
	if err != nil {
		return nil, err
	}
	windows := breakCandidateWindows(rms, duration)
	coverage := 0.0
	for _, window := range windows {
		coverage += window.end - window.start
	}
	// Silence-rich material gets one confirmation pass, avoiding a seek storm.
	if len(windows) > 64 || coverage > duration*.4 {
		windows = []breakRun{{0, duration}}
	}
	evidence.CandidateWindows = breakRunEvidence(windows)
	for _, window := range windows {
		signals, _, err := run(breakScanPass{start: window.start, end: window.end})
		if err != nil {
			signals = completedBreakPrefix(signals)
		}
		appendBreakSignals(&combined, signals, window.start)
		if err != nil {
			return combineBreakSignals(combined, in.Info.Duration), err
		}
	}
	return combineBreakSignals(combined, in.Info.Duration), nil
}

// runBreakPass never prints stderr, which may contain private source grants.
func (a *BreakAnalyzer) runBreakPass(ctx context.Context, in BreakInput, pass breakScanPass, hw core.HWAccel) (breakSignals, []breakRun, time.Duration, error) {
	args := []string{"-hide_banner", "-nostdin", "-nostats", "-threads", "1", "-filter_threads", "1"}
	if pass.start > 0 {
		args = append(args, "-ss", strconv.FormatFloat(pass.start, 'f', 3, 64))
	}
	args = append(args, hardwareBreakArgs(hw)...)
	if len(hardwareBreakArgs(hw)) == 0 {
		args = append(args, "-flags", "+gray")
		if video := in.Info.Video(); video != nil && video.Codec == "mpeg2video" {
			args = append(args, "-lowres", "2")
		}
	}
	if pass.sparse {
		args = append(args, "-skip_frame", "nokey")
	}
	args = append(args, "-i", in.URL, "-t", strconv.FormatFloat(pass.end-pass.start, 'f', 3, 64), "-map", "0:v:0", "-map", "0:a:0")
	vf := tinyLuma + ",fps=10,blackdetect=d=0.3:pix_th=0.08:pic_th=0.98,scdet=threshold=8"
	af := "silencedetect=noise=-45dB:d=0.2"
	if pass.sparse {
		vf = tinyLuma + ",blackdetect=d=0:pix_th=0.08:pic_th=0.98"
		af = breakRMSFilter
	}
	args = append(args, "-vf", vf, "-af", af, "-fps_mode", "passthrough", "-threads", "1", "-f", "null", "-")
	return a.runBreakCommand(ctx, args, pass.sparse, pass.end-pass.start)
}

func (a *BreakAnalyzer) runBreakCommand(ctx context.Context, args []string, sparse bool, duration float64) (breakSignals, []breakRun, time.Duration, error) {
	args = append([]string{"-progress", "pipe:2", "-stats_period", "1"}, args...)
	var cmd *exec.Cmd
	if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
		cmd = exec.CommandContext(ctx, "nice", append([]string{"-n", "10", a.FFmpeg}, args...)...)
	} else {
		cmd = exec.CommandContext(ctx, a.FFmpeg, args...)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return breakSignals{}, nil, 0, err
	}
	if err = cmd.Start(); err != nil {
		return breakSignals{}, nil, 0, err
	}
	var signals breakSignals
	silenceStart := -1.0
	rms := rmsRuns{start: -1}
	scan := bufio.NewScanner(stderr)
	scan.Buffer(make([]byte, 4096), 64<<10)
	exceeded := false
	for scan.Scan() {
		line := scan.Text()
		if strings.HasPrefix(line, "out_time_us=") {
			if value, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "out_time_us=")), 10, 64); err == nil {
				signals.progressMS = max(signals.progressMS, value/1000)
			}
		}
		if line == "progress=end" {
			signals.progressEnd = true
		}
		if strings.HasPrefix(line, "frame=") {
			if value, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "frame=")), 10, 64); err == nil {
				signals.frames = max(signals.frames, value)
			}
		}
		signals.line(line, &silenceStart)
		if sparse {
			rms.line(line)
		}
		if len(signals.black) > 4096 || len(signals.silent) > 100000 || len(signals.scenes) > 100000 || len(rms.runs) > 10000 {
			exceeded = true
			_ = cmd.Process.Kill()
			break
		}
	}
	if scan.Err() != nil {
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	cost := time.Duration(0)
	if cmd.ProcessState != nil {
		cost = cmd.ProcessState.UserTime() + cmd.ProcessState.SystemTime()
	}
	rms.finish()
	if silenceStart >= 0 {
		signals.silent = append(signals.silent, breakRun{silenceStart, math.Min(duration, float64(signals.progressMS)/1000)})
	}
	tolerance := .25
	if duration > 30 {
		tolerance = 2
	}
	covered := signals.progressEnd && signals.frames > 0 && float64(signals.progressMS) >= 1000*(duration-tolerance)
	if !sparse && signals.frames < int64(math.Floor(math.Max(0, duration-tolerance)*10)) {
		covered = false
	}
	if exceeded || scan.Err() != nil || waitErr != nil || !covered {
		return signals, rms.runs, cost, fmt.Errorf("act-break decode incomplete")
	}
	return signals, rms.runs, cost, nil
}

// A dark run ending exactly at a truncated EOF has no observed return to picture.
// Do not publish it as a partial fact; only finished earlier runs are reliable.
func completedBreakPrefix(signals breakSignals) breakSignals {
	cutoff := float64(signals.progressMS)/1000 - .25
	black := make([]breakRun, 0, len(signals.black))
	for _, run := range signals.black {
		if run.end < cutoff {
			black = append(black, run)
		}
	}
	signals.black = black
	return signals
}
