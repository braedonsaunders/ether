package markers

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.Register("markers_breaks", 1, `CREATE TABLE markers_breaks_cache(release_key TEXT NOT NULL, version INTEGER NOT NULL, duration_ms INTEGER NOT NULL, points TEXT NOT NULL, analyzed_at INTEGER NOT NULL, PRIMARY KEY(release_key,version));`)
}

const BreakConfidence = 0.90
const breakVersion = 2

type BreakPoint struct {
	AtMS       int64   `json:"atMs"`
	Source     string  `json:"source"`
	Confidence float64 `json:"confidence"`
}

// BreakInput is leased exclusively for background analysis. ReleaseKey must bind
// the source revision, payload, file and duration, never just a catalogue key.
// Close releases the source even when a cached analysis is reused.
type BreakInput struct {
	ShouldYield       func() bool                 `json:"-"`
	Drain             func(context.Context) error `json:"-"`
	ProviderStatsBase *[3]int64                   `json:"-"`
	budgetDay         string
	Key               core.RatingKey `json:"key"`
	ReleaseKey        string
	ReleaseID         string
	Revision          string
	URL               string                       `json:"-"`
	Info              core.MediaInfo               `json:"-"`
	Close             func()                       `json:"-"`
	BytesRead         func() int64                 `json:"-"`
	ReadErrors        func() int64                 `json:"-"`
	ProviderStats     func() (int64, int64, int64) `json:"-"`
}

type BreakAnalyzer struct {
	DB         *sql.DB
	FFmpeg     string
	OnCost     func(video, cpu, wall time.Duration)
	ScanPolicy func() BreakScanPolicy
	Hardware   func() core.HWAccel
	// OnEvidence is a trusted local diagnostic observer. The source lease remains
	// open during this synchronous call; observers must not close or retain it.
	// Nil performs no export and installs no HTTP endpoint.
	OnEvidence func(context.Context, BreakInput, BreakEvidence)
}

func (a *BreakAnalyzer) Analyze(ctx context.Context, in BreakInput) (points []BreakPoint, err error) {
	evidence := newBreakEvidence(in)
	defer func() {
		if in.ProviderStats != nil {
			requests, bytes, failures := in.ProviderStats()
			base := [3]int64{}
			if in.ProviderStatsBase != nil {
				base = *in.ProviderStatsBase
			}
			evidence.ProviderStatsKnown = true
			evidence.ProviderRequests = requests - base[0]
			evidence.ProviderBytes = bytes - base[1]
			evidence.ProviderErrors = failures - base[2]
			if evidence.ProviderErrors > 0 {
				err = fmt.Errorf("act-break provider read failed")
				evidence.Partial = evidence.Decoded
			}
		}
		if a.OnEvidence != nil {
			evidence.Complete = err == nil && !evidence.Partial
			if err != nil && evidence.Decoded {
				evidence.Partial = true
				evidence.CoverageComplete = false
			}
			evidence.Points = append([]BreakPoint(nil), points...)
			a.OnEvidence(ctx, in, evidence)
		}
	}()

	if in.ReleaseKey == "" || in.Info.Duration <= 0 || in.Info.Estimated {
		return nil, fmt.Errorf("act breaks require an exact source")
	}
	if in.Drain != nil {
		drain, stop := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		drainErr := in.Drain(drain)
		stop()
		if drainErr != nil {
			return nil, fmt.Errorf("act-break source preparation did not settle")
		}
	}
	if breakInputFailed(in) {
		return nil, fmt.Errorf("act-break source preparation failed")
	}
	cached, found, full, lookupErr := a.cachedBreakFacts(ctx, in)
	if lookupErr != nil {
		return nil, lookupErr
	}
	if found {
		evidence.CacheHit = true
		evidence.Partial = !full
		return cached, nil
	}
	points = ChapterBreaks(in.Info)
	if len(points) == 0 {
		if a.FFmpeg == "" || !hasBreakAudio(in.Info) {
			return nil, fmt.Errorf("act-break decoder unavailable")
		}
		in.budgetDay = time.Now().UTC().Format("2006-01-02")
		reservation, reserveErr := a.reserveScan(ctx, in)
		if reserveErr != nil {
			return nil, reserveErr
		}
		evidence.ReservedBytes = reservation
		before := int64(0)
		if in.BytesRead != nil {
			before = in.BytesRead()
		}
		beforeErrors := int64(0)
		if in.ReadErrors != nil {
			beforeErrors = in.ReadErrors()
		}
		beforeRequests, beforeWire, beforeSourceErrors := int64(0), int64(0), int64(0)
		if in.ProviderStats != nil {
			beforeRequests, beforeWire, beforeSourceErrors = in.ProviderStats()
		}
		points, err = a.decodeEvidence(ctx, in, &evidence)
		if in.Drain != nil {
			drain, stop := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
			drainErr := in.Drain(drain)
			stop()
			if drainErr != nil {
				err = fmt.Errorf("act-break source did not settle")
			}
		}
		if in.ProviderStats != nil {
			requests, bytes, failures := in.ProviderStats()
			evidence.ProviderRequests = requests - beforeRequests
			evidence.ProviderBytes = bytes - beforeWire
			evidence.ProviderErrors = failures - beforeSourceErrors
			evidence.ProviderStatsKnown = true
			if evidence.ProviderErrors > 0 {
				err = fmt.Errorf("act-break source read failed")
			}
		}
		if in.ReadErrors != nil && in.ReadErrors() > beforeErrors {
			err = fmt.Errorf("act-break source read failed")
		}
		if in.BytesRead != nil {
			evidence.ReadBytesKnown = true
			evidence.ReadBytes = max(int64(0), in.BytesRead()-before)
		}
		accounting, stop := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		_, _ = a.DB.ExecContext(accounting, `UPDATE markers_breaks_budget SET read_bytes=read_bytes+? WHERE day=?`, evidence.ReadBytes, in.budgetDay)
		stop()
		if err != nil {
			if len(points) > 0 {
				partial, stop := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
				_ = a.ObservePoints(partial, in, points)
				stop()
			}
			return points, err
		}
	}
	if breakInputFailed(in) {
		return points, fmt.Errorf("act-break source read failed")
	}
	b, err := json.Marshal(points)
	if err != nil {
		return nil, err
	}
	_, err = a.DB.ExecContext(ctx, `INSERT INTO markers_breaks_cache(release_key,version,duration_ms,points,analyzed_at) VALUES(?,?,?,?,?) ON CONFLICT(release_key,version) DO UPDATE SET duration_ms=excluded.duration_ms,points=excluded.points,analyzed_at=excluded.analyzed_at`, in.ReleaseKey, breakVersion, in.Info.Duration.Milliseconds(), string(b), time.Now().Unix())
	return points, err
}

var actLabel = regexp.MustCompile(`(?i)^(act\s*[0-9ivx]+|act[ -]?break\s*[0-9]*|commercial[ -]?break\s*[0-9]*|part\s*[0-9]+)$`)

// Generic chapter numbers, intros and credits are deliberately not evidence.
func ChapterBreaks(info core.MediaInfo) []BreakPoint {
	var out []BreakPoint
	for _, ch := range info.Chapters {
		if actLabel.MatchString(strings.TrimSpace(ch.Title)) && ch.Start >= 60*time.Second && ch.Start < info.Duration-60*time.Second {
			out = append(out, BreakPoint{ch.Start.Milliseconds(), "chapters", 0.99})
		}
	}
	return SelectBreaks(out, info.Duration.Milliseconds(), 256, 60000)
}

type breakRun struct{ start, end float64 }
type breakSignals struct {
	progressMS    int64
	frames        int64
	progressEnd   bool
	black, silent []breakRun
	scenes        []float64
	sceneEvidence []BreakSignal
	sceneScore    *float64
}

func combineBreakSignals(s breakSignals, duration time.Duration) []BreakPoint {
	var out []BreakPoint
	for _, b := range s.black {
		length := b.end - b.start
		if length < .3 || length > 3 {
			continue
		}
		for _, audio := range s.silent {
			overlap := math.Min(b.end, audio.end) - math.Max(b.start, audio.start)
			if overlap < .8*length {
				continue
			}
			confidence := .91
			for _, at := range s.scenes {
				if math.Abs(at-b.start) <= .6 || math.Abs(at-b.end) <= .6 {
					confidence = .96
					break
				}
			}
			out = append(out, BreakPoint{int64(math.Round((b.start + b.end) * 500)), "detected", confidence})
			break
		}
	}
	return SelectBreaks(out, duration.Milliseconds(), 256, 60000)
}

// SelectBreaks solves the ordered K-point spacing problem with dynamic
// programming. Confidence dominates; spacing towards ideal act lengths breaks
// ties. It returns fewer than K whenever evidence or safe spacing is lacking.
func SelectBreaks(points []BreakPoint, duration int64, k int, spacing int64) []BreakPoint {
	if k <= 0 || duration <= 0 {
		return nil
	}
	k = min(k, 256)
	spacing = max(spacing, 60000)
	ps := make([]BreakPoint, 0, len(points))
	for _, p := range points {
		if p.Confidence >= BreakConfidence && p.Confidence <= 1 && p.AtMS >= 60000 && p.AtMS <= duration-60000 {
			ps = append(ps, p)
		}
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].AtMS < ps[j].AtMS })
	if len(ps) > 256 {
		ps = ps[:256]
	}
	type state struct {
		score float64
		path  []BreakPoint
	}
	dp := make([][]state, k+1)
	for n := range dp {
		dp[n] = make([]state, len(ps))
	}
	var best state
	for n := 1; n <= k; n++ {
		for i, p := range ps {
			ideal := float64(duration) * float64(n) / float64(k+1)
			gain := p.Confidence - .01*math.Abs(float64(p.AtMS)-ideal)/float64(duration)
			if n == 1 {
				dp[n][i] = state{gain, []BreakPoint{p}}
			} else {
				for j := 0; j < i; j++ {
					prev := dp[n-1][j]
					if len(prev.path) == 0 || p.AtMS-ps[j].AtMS < spacing {
						continue
					}
					score := prev.score + gain
					if score > dp[n][i].score {
						path := append([]BreakPoint(nil), prev.path...)
						dp[n][i] = state{score, append(path, p)}
					}
				}
			}
			if dp[n][i].score > best.score {
				best = dp[n][i]
			}
		}
	}
	return best.path
}

var signalValue = regexp.MustCompile(`(black_start|black_end|silence_start|silence_end|lavfi.scd.time|lavfi.scd.score)[:=]\s*([0-9.]+)`)

func (s *breakSignals) line(line string, silenceStart *float64) {
	var black breakRun
	var hasBlack bool
	for _, v := range signalValue.FindAllStringSubmatch(line, -1) {
		at, err := strconv.ParseFloat(v[2], 64)
		if err != nil || math.IsNaN(at) || math.IsInf(at, 0) {
			continue
		}
		switch v[1] {
		case "black_start":
			black.start = at
			hasBlack = true
		case "black_end":
			black.end = at
		case "silence_start":
			*silenceStart = at
		case "silence_end":
			if *silenceStart >= 0 {
				s.silent = append(s.silent, breakRun{*silenceStart, at})
				*silenceStart = -1
			}
		case "lavfi.scd.score":
			score := at
			s.sceneScore = &score
		case "lavfi.scd.time":
			s.scenes = append(s.scenes, at)
			ms := int64(math.Round(at * 1000))
			s.sceneEvidence = append(s.sceneEvidence, BreakSignal{StartMS: ms, EndMS: ms, Score: s.sceneScore})
			s.sceneScore = nil
		}
	}
	if hasBlack && black.end > black.start {
		s.black = append(s.black, black)
	}
}
func (a *BreakAnalyzer) decode(ctx context.Context, in BreakInput) ([]BreakPoint, error) {
	evidence := newBreakEvidence(in)
	return a.decodeEvidence(ctx, in, &evidence)
}
func (a *BreakAnalyzer) decodeEvidence(ctx context.Context, in BreakInput, evidence *BreakEvidence) ([]BreakPoint, error) {
	if evidence.ReservedBytes > 0 && in.BytesRead != nil {
		limited, cancel := context.WithCancel(ctx)
		defer cancel()
		ctx = limited
		baseline := in.BytesRead()
		done := make(chan struct{})
		defer close(done)
		go func() {
			tick := time.NewTicker(50 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-done:
					return
				case <-ctx.Done():
					return
				case <-tick.C:
					if in.ShouldYield != nil && in.ShouldYield() {
						cancel()
						return
					}
					if in.BytesRead()-baseline >= evidence.ReservedBytes {
						cancel()
						return
					}
				}
			}
		}()
	}

	return a.decodeStrategy(ctx, in, evidence)
}

func hasBreakAudio(info core.MediaInfo) bool {
	for _, s := range info.Streams {
		if s.Type == "audio" {
			return true
		}
	}
	return false
}

func breakInputFailed(in BreakInput) bool {
	if in.ReadErrors != nil && in.ReadErrors() > 0 {
		return true
	}
	if in.ProviderStats != nil && in.ProviderStatsBase != nil {
		_, _, failures := in.ProviderStats()
		return failures > in.ProviderStatsBase[2]
	}
	return false
}
