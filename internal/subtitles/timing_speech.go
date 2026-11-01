package subtitles

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"time"
)

// SpeechWindow is one decoded audio slice used as a timing reference.
type SpeechWindow struct {
	Start time.Duration
	Rate  int     // samples per second
	PCM   []int16 // mono
}

// FitSpeechEnergy cross-correlates the subtitle on/off mask with a
// band-passed energy detector over a few short audio windows.
func FitSpeechEnergy(cues []Cue, windows []SpeechWindow) (TimingFit, bool) {
	return FitSpeechEnergyOptions(cues, windows, FitOptions{})
}

// FitSpeechEnergyOptions is FitSpeechEnergy with explicit thresholds.
func FitSpeechEnergyOptions(cues []Cue, windows []SpeechWindow, opt FitOptions) (TimingFit, bool) {
	opt = opt.withDefaults()
	if len(cues) < 3 || len(windows) == 0 {
		return TimingFit{}, false
	}
	var best TimingFit
	found := false
	for _, rate := range timingRates {
		var votes []time.Duration
		var confs []float64
		for _, w := range windows {
			off, conf, ok := correlateWindow(cues, w, rate, opt)
			if !ok {
				continue
			}
			votes = append(votes, off)
			confs = append(confs, conf)
		}
		if len(votes) == 0 {
			continue
		}
		offset, conf := consensusOffset(votes, confs, opt.FrameSlack)
		fit := TimingFit{Offset: offset, Rate: rate, Confidence: conf, Inliers: len(votes)}
		if !found || fit.Confidence > best.Confidence {
			best, found = fit, true
		}
	}
	if !found || best.Inliers < 2 || best.Confidence < opt.MinConfidence {
		return TimingFit{}, false
	}
	return best, true
}

func consensusOffset(votes []time.Duration, confs []float64, slack time.Duration) (time.Duration, float64) {
	bestI := 0
	bestN := 0
	var bestAbs time.Duration = 1 << 62
	for i := range votes {
		n := 0
		var abs time.Duration
		for j := range votes {
			d := absDuration(votes[i] - votes[j])
			if d <= slack*4 {
				n++
				abs += d
			}
		}
		if n > bestN || (n == bestN && abs < bestAbs) {
			bestI, bestN, bestAbs = i, n, abs
		}
	}
	var sum float64
	for i := range votes {
		if absDuration(votes[i]-votes[bestI]) <= slack*4 {
			sum += confs[i]
		}
	}
	return votes[bestI], sum / float64(len(votes))
}

func correlateWindow(cues []Cue, w SpeechWindow, rate float64, opt FitOptions) (time.Duration, float64, bool) {
	if w.Rate <= 0 || len(w.PCM) < w.Rate {
		return 0, 0, false
	}
	frame := w.Rate / 50 // 20 ms
	if frame < 80 {
		frame = 80
	}
	speech := speechMask(w.PCM, frame)
	if len(speech) < 8 {
		return 0, 0, false
	}
	winDur := time.Duration(len(w.PCM)) * time.Second / time.Duration(w.Rate)
	cue := cueMask(cues, w.Start, winDur, rate, len(speech))
	prefix := make([]int64, len(speech)+1)
	for i, v := range speech {
		prefix[i+1] = prefix[i] + int64(v)
	}
	maxLag := int(opt.MaxOffset / (time.Second / time.Duration(50)))
	if maxLag > len(speech)-4 {
		maxLag = len(speech) - 4
	}
	if maxLag < 1 {
		return 0, 0, false
	}
	var bestLag int
	var bestScore int64
	have := false
	for lag := -maxLag; lag <= maxLag; lag++ {
		score := maskScore(prefix, cue, lag)
		if !have || score > bestScore {
			bestLag, bestScore, have = lag, score, true
		}
	}
	if !have || bestScore <= 0 {
		return 0, 0, false
	}
	on := 0
	for _, v := range cue {
		on += int(v)
	}
	if on == 0 {
		return 0, 0, false
	}
	speechOn := prefix[len(speech)]
	den := float64(int64(on) + speechOn)
	conf := 0.0
	if den > 0 {
		conf = 2 * float64(bestScore) / den
	}
	if conf > 1 {
		conf = 1
	}
	// lag > 0: cue frame i matched speech at i-lag (earlier), so the
	// sidecar is late and the correction offset is negative.
	offset := -time.Duration(bestLag) * (time.Second / 50)
	return offset, conf, conf >= opt.MinConfidence/2
}

func speechMask(pcm []int16, frame int) []uint8 {
	if frame <= 0 {
		frame = 160
	}
	n := len(pcm) / frame
	if n == 0 {
		return nil
	}
	energy := make([]float64, n)
	for i := 0; i < n; i++ {
		var sum float64
		prev := 0.0
		for j := 0; j < frame; j++ {
			s := float64(pcm[i*frame+j])
			// One-pole high-pass ~300 Hz at 16 kHz, then energy.
			hp := s - prev
			prev = s
			sum += hp * hp
		}
		energy[i] = sum / float64(frame)
	}
	sorted := append([]float64(nil), energy...)
	sortFloats(sorted)
	median := sorted[len(sorted)/2]
	maxE := sorted[len(sorted)-1]
	// Noise floor of the quieter half, not p80 (which caps speech at ~20%).
	quiet := sorted[:max(1, len(sorted)/2)]
	floor := quiet[len(quiet)/2]
	thresh := floor * 4
	if thresh < maxE*0.02 {
		thresh = maxE * 0.02
	}
	if median > 0 && thresh < median*1.5 {
		thresh = median * 1.5
	}
	if thresh <= 0 && maxE > 0 {
		thresh = maxE * 0.08
	}
	out := make([]uint8, n)
	for i, e := range energy {
		if e > thresh {
			out[i] = 1
		}
	}
	return out
}

func cueMask(cues []Cue, winStart, winDur time.Duration, rate float64, frames int) []uint8 {
	out := make([]uint8, frames)
	if frames == 0 || winDur <= 0 {
		return out
	}
	for _, c := range cues {
		start := time.Duration(float64(c.Start)*rate) - winStart
		end := time.Duration(float64(c.End)*rate) - winStart
		if end <= 0 || start >= winDur {
			continue
		}
		if start < 0 {
			start = 0
		}
		if end > winDur {
			end = winDur
		}
		a := int(start * time.Duration(frames) / winDur)
		b := int(end * time.Duration(frames) / winDur)
		if a < 0 {
			a = 0
		}
		if b > frames {
			b = frames
		}
		for i := a; i < b; i++ {
			out[i] = 1
		}
	}
	return out
}

func maskScore(speechPrefix []int64, cue []uint8, lag int) int64 {
	var sum int64
	for i, v := range cue {
		if v == 0 {
			continue
		}
		j := i - lag
		if j < 0 || j+1 >= len(speechPrefix) {
			continue
		}
		sum += speechPrefix[j+1] - speechPrefix[j]
	}
	return sum
}

func sortFloats(v []float64) {
	for i := 1; i < len(v); i++ {
		x, j := v[i], i-1
		for j >= 0 && v[j] > x {
			v[j+1] = v[j]
			j--
		}
		v[j+1] = x
	}
}

// DecodeAudioWindows reads a few short windows of the first audio track
// with ffmpeg from a local file or HTTP URL. Skipped when ffmpeg is absent.
func DecodeAudioWindows(ctx context.Context, ffmpegPath, input string, duration time.Duration, windows int, span time.Duration) ([]SpeechWindow, error) {
	return decodeAudioWindows(ctx, ffmpegPath, input, nil, 0, duration, windows, span)
}

// DecodeAudioWindowsReader is DecodeAudioWindows from a held source reader.
func DecodeAudioWindowsReader(ctx context.Context, ffmpegPath string, r io.ReaderAt, size int64, duration time.Duration, windows int, span time.Duration) ([]SpeechWindow, error) {
	return decodeAudioWindows(ctx, ffmpegPath, "", r, size, duration, windows, span)
}

func decodeAudioWindows(ctx context.Context, ffmpegPath, input string, r io.ReaderAt, size int64, duration time.Duration, windows int, span time.Duration) ([]SpeechWindow, error) {
	if ffmpegPath == "" {
		ffmpegPath = "ffmpeg"
	}
	if windows < 3 {
		windows = 3
	}
	if windows > 6 {
		windows = 6
	}
	if span <= 0 {
		span = 8 * time.Second
	}
	if span > 12*time.Second {
		span = 12 * time.Second
	}
	if duration <= 0 {
		duration = 30 * time.Minute
	}
	starts := spreadWindows(duration, windows, span)
	out := make([]SpeechWindow, 0, len(starts))
	for _, start := range starts {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		pcm, rate, err := decodeWindow(ctx, ffmpegPath, input, r, size, start, span)
		if err != nil {
			continue
		}
		out = append(out, SpeechWindow{Start: start, Rate: rate, PCM: pcm})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no audio windows decoded")
	}
	return out, nil
}

func spreadWindows(duration time.Duration, n int, span time.Duration) []time.Duration {
	if n < 2 {
		n = 2
	}
	if duration <= 0 {
		return []time.Duration{0, 0}
	}
	if duration <= span {
		step := duration / time.Duration(n)
		out := make([]time.Duration, n)
		for i := 0; i < n; i++ {
			out[i] = time.Duration(i) * step
		}
		return out
	}
	// 15s cinema margin only on long titles so an 8s clip still yields
	// at least two windows.
	margin := duration / 10
	if duration >= 3*time.Minute && margin < 15*time.Second {
		margin = 15 * time.Second
	}
	if margin*2+span >= duration {
		margin = 0
	}
	usable := duration - span - 2*margin
	if usable < 0 {
		usable = duration - span
		margin = 0
	}
	out := make([]time.Duration, n)
	for i := 0; i < n; i++ {
		out[i] = margin + time.Duration(int64(usable)*int64(i)/int64(n-1))
	}
	return out
}

func decodeWindow(ctx context.Context, ffmpeg, input string, r io.ReaderAt, size int64, start, span time.Duration) ([]int16, int, error) {
	const maxPCM = 512 << 10
	args := []string{
		"-hide_banner", "-loglevel", "error", "-nostdin",
		"-probesize", "1048576", "-analyzeduration", "2000000",
		"-ss", fmt.Sprintf("%.3f", start.Seconds()),
		"-t", fmt.Sprintf("%.3f", span.Seconds()),
	}
	if r != nil {
		args = append(args, "-i", "pipe:0")
	} else {
		args = append(args, "-i", input)
	}
	args = append(args,
		"-map", "0:a:0?", "-ac", "1", "-ar", "16000",
		"-af", "highpass=f=300,lowpass=f=3400",
		"-fs", fmt.Sprintf("%d", maxPCM),
		"-f", "s16le", "pipe:1",
	)
	cmd := exec.CommandContext(ctx, ffmpeg, args...)
	if r != nil {
		limit := size
		if limit <= 0 || limit > sourceReadBudget {
			limit = sourceReadBudget
		}
		cmd.Stdin = io.LimitReader(io.NewSectionReader(r, 0, size), limit)
	}
	raw, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return nil, 0, fmt.Errorf("ffmpeg: %s", trimErr(ee.Stderr))
		}
		return nil, 0, err
	}
	if len(raw) > maxPCM {
		raw = raw[:maxPCM]
	}
	if len(raw) < 2 {
		return nil, 0, fmt.Errorf("empty audio window")
	}
	pcm := make([]int16, len(raw)/2)
	for i := range pcm {
		pcm[i] = int16(raw[i*2]) | int16(raw[i*2+1])<<8
	}
	return pcm, 16000, nil
}

func trimErr(b []byte) string {
	s := string(b)
	if len(s) > 240 {
		return s[len(s)-240:]
	}
	return s
}
