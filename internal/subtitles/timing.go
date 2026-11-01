package subtitles

import (
	"math"
	"sort"
	"time"
)

// Frame-rate ratios used when a sidecar was timed for a different video
// cadence than the release (23.976, 24, 25 and the inverses).
const (
	fps23976 = 24000.0 / 1001.0
	fps24    = 24.0
	fps25    = 25.0
)

var timingRates = []float64{
	1,
	fps23976 / fps24,
	fps24 / fps23976,
	fps23976 / fps25,
	fps25 / fps23976,
	fps24 / fps25,
	fps25 / fps24,
}

const (
	timingFrameSlack    = 42 * time.Millisecond // one 24 fps frame
	timingMinInliers    = 8
	timingMinConfidence = 0.7
	timingMaxOffset     = 15 * time.Minute
	timingBin           = 20 * time.Millisecond
)

// TimingFit is a constant offset plus a frame-rate scale that maps
// candidate cue times onto a reference clock:
//
//	ref ≈ candidate*Rate + Offset
type TimingFit struct {
	Offset     time.Duration
	Rate       float64
	Confidence float64
	Inliers    int
	Residual   time.Duration
}

// Identity reports a fit that leaves the sidecar unchanged.
func (f TimingFit) Identity() bool {
	if f.Rate == 0 {
		return true
	}
	return math.Abs(f.Rate-1) < 0.0005 && absDuration(f.Offset) < timingFrameSlack
}

// FitTiming aligns candidate cue start times to reference start times.
// Matching is language-independent: only the timing pattern is used.
func FitTiming(candidate, reference []Cue) (TimingFit, bool) {
	return FitTimingOptions(candidate, reference, FitOptions{})
}

// FitOptions tunes FitTiming. Zero values use the defaults above.
type FitOptions struct {
	MinInliers    int
	MinConfidence float64
	MaxOffset     time.Duration
	FrameSlack    time.Duration
}

func (o FitOptions) withDefaults() FitOptions {
	if o.MinInliers <= 0 {
		o.MinInliers = timingMinInliers
	}
	if o.MinConfidence <= 0 {
		o.MinConfidence = timingMinConfidence
	}
	if o.MaxOffset <= 0 {
		o.MaxOffset = timingMaxOffset
	}
	if o.FrameSlack <= 0 {
		o.FrameSlack = timingFrameSlack
	}
	return o
}

// FitTimingOptions is FitTiming with explicit thresholds (tests use a
// smaller cue count).
func FitTimingOptions(candidate, reference []Cue, opt FitOptions) (TimingFit, bool) {
	opt = opt.withDefaults()
	cStarts := cueStarts(candidate)
	rStarts := cueStarts(reference)
	if len(cStarts) < 3 || len(rStarts) < 3 {
		return TimingFit{}, false
	}
	var best TimingFit
	found := false
	for _, rate := range timingRates {
		fit, ok := fitRate(cStarts, rStarts, rate, opt)
		if !ok {
			continue
		}
		if !found || fit.Confidence > best.Confidence ||
			(fit.Confidence == best.Confidence && fit.Residual < best.Residual) {
			best, found = fit, true
		}
	}
	need := opt.MinInliers
	if n := len(cStarts); need > n {
		need = max(3, n*2/3)
	}
	if !found || best.Inliers < need || best.Confidence < opt.MinConfidence {
		return TimingFit{}, false
	}
	return best, true
}

func cueStarts(cues []Cue) []time.Duration {
	out := make([]time.Duration, 0, len(cues))
	for _, c := range cues {
		if c.End > c.Start {
			out = append(out, c.Start)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func fitRate(candidate, reference []time.Duration, rate float64, opt FitOptions) (TimingFit, bool) {
	scaled := make([]time.Duration, len(candidate))
	for i, t := range candidate {
		scaled[i] = time.Duration(float64(t) * rate)
	}
	// Histogram of pairwise offsets. Subsample only the candidate so a
	// long film stays cheap; search the full reference.
	stepC := 1
	if len(scaled) > 48 {
		stepC = (len(scaled) + 47) / 48
	}
	bins := map[int64]int{}
	for i := 0; i < len(scaled); i += stepC {
		for j := 0; j < len(reference); j++ {
			off := reference[j] - scaled[i]
			if absDuration(off) > opt.MaxOffset {
				continue
			}
			bins[int64(off/timingBin)]++
		}
	}
	if len(bins) == 0 {
		return TimingFit{}, false
	}
	type ranked struct {
		bin int64
		n   int
	}
	top := make([]ranked, 0, 8)
	for b, n := range bins {
		top = append(top, ranked{b, n})
	}
	sort.Slice(top, func(i, j int) bool {
		if top[i].n != top[j].n {
			return top[i].n > top[j].n
		}
		return abs64(top[i].bin) < abs64(top[j].bin)
	})
	if len(top) > 8 {
		top = top[:8]
	}
	var (
		offset   time.Duration
		inliers  int
		residual time.Duration
	)
	for i, cand := range top {
		off := time.Duration(cand.bin) * timingBin
		inl, res := scoreOffset(scaled, reference, off, opt.FrameSlack)
		if i == 0 || inl > inliers || (inl == inliers && res < residual) {
			offset, inliers, residual = off, inl, res
		}
	}
	need := opt.MinInliers
	if need > len(scaled) {
		need = max(3, len(scaled)*2/3)
	}
	if inliers < need {
		return TimingFit{}, false
	}
	denom := len(scaled)
	if len(reference) < denom {
		denom = len(reference)
	}
	conf := float64(inliers) / float64(denom)
	if conf > 1 {
		conf = 1
	}
	if !segmentConsistent(scaled, reference, offset, opt.FrameSlack) {
		return TimingFit{}, false
	}
	return TimingFit{Offset: offset, Rate: rate, Confidence: conf, Inliers: inliers, Residual: residual}, true
}

// segmentConsistent rejects a single-offset fit when a mid-file recut
// matches only part of the timeline. All three thirds must agree.
func segmentConsistent(scaled, reference []time.Duration, offset, slack time.Duration) bool {
	if len(scaled) < 12 {
		return true
	}
	for i := 0; i < 3; i++ {
		lo := i * len(scaled) / 3
		hi := (i + 1) * len(scaled) / 3
		if hi-lo < 3 {
			return false
		}
		inl, _ := scoreOffset(scaled[lo:hi], reference, offset, slack)
		if float64(inl) < float64(hi-lo)/3 {
			return false
		}
	}
	return true
}

func scoreOffset(scaled, reference []time.Duration, offset, slack time.Duration) (intliers int, residual time.Duration) {
	var abs []time.Duration
	j := 0
	for _, t := range scaled {
		want := t + offset
		for j+1 < len(reference) && reference[j+1] <= want {
			j++
		}
		best := absDuration(reference[j] - want)
		if j+1 < len(reference) {
			if n := absDuration(reference[j+1] - want); n < best {
				best = n
			}
		}
		if j > 0 {
			if n := absDuration(reference[j-1] - want); n < best {
				best = n
			}
		}
		if best <= slack {
			intliers++
			abs = append(abs, best)
		}
	}
	if len(abs) == 0 {
		return 0, slack
	}
	sort.Slice(abs, func(i, j int) bool { return abs[i] < abs[j] })
	return intliers, abs[len(abs)/2]
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
