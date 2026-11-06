package markers

import (
	"math"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
)

// BreakSignal contains measured source-media times. Score is the actual scdet
// percent when supplied by ffmpeg, never a synthesized confidence. It is absent
// for black/silence intervals and chapters, which have no measured score.
type BreakSignal struct {
	StartMS      int64    `json:"startMs"`
	EndMS        int64    `json:"endMs"`
	Score        *float64 `json:"score,omitempty"`
	ChapterMatch bool     `json:"chapterMatch,omitempty"`
}

// BreakEvidence intentionally contains neither source grants nor media. A cache
// hit reports CacheHit=true, Decoded=false and cached accepted points; historical
// decode candidates are unavailable unless a local observer saved them earlier.
// Complete=false candidates are partial and must not enter quality denominators.
type BreakEvidence struct {
	ProviderStatsKnown bool           `json:"providerStatsKnown"`
	ProviderRequests   int64          `json:"providerRequests"`
	ProviderBytes      int64          `json:"providerBytes"`
	ProviderErrors     int64          `json:"providerErrors"`
	DecodedThroughMS   int64          `json:"decodedThroughMs"`
	CoverageComplete   bool           `json:"coverageComplete"`
	Mode               string         `json:"mode,omitempty"`
	Backend            string         `json:"backend,omitempty"`
	AudioCandidates    []BreakSignal  `json:"audioCandidates,omitempty"`
	KeyframeBlack      []BreakSignal  `json:"keyframeBlack,omitempty"`
	CandidateWindows   []BreakSignal  `json:"candidateWindows,omitempty"`
	ReadBytesKnown     bool           `json:"readBytesKnown"`
	Partial            bool           `json:"partial"`
	ReservedBytes      int64          `json:"reservedBytes"`
	ReadBytes          int64          `json:"readBytes"`
	Version            int            `json:"version"`
	Key                core.RatingKey `json:"key"`
	DurationMS         int64          `json:"durationMs"`
	CacheHit           bool           `json:"cacheHit"`
	Decoded            bool           `json:"decoded"`
	Complete           bool           `json:"complete"`
	Chapters           []BreakSignal  `json:"chapters"`
	Black              []BreakSignal  `json:"black"`
	Silence            []BreakSignal  `json:"silence"`
	Scenes             []BreakSignal  `json:"scenes"`
	Points             []BreakPoint   `json:"points"`
	CPUMS              int64          `json:"cpuMs"`
	WallMS             int64          `json:"wallMs"`
}

func newBreakEvidence(in BreakInput) BreakEvidence {
	e := BreakEvidence{Version: breakVersion, Key: in.Key, DurationMS: in.Info.Duration.Milliseconds()}
	for _, c := range in.Info.Chapters {
		e.Chapters = append(e.Chapters, BreakSignal{StartMS: c.Start.Milliseconds(), EndMS: c.End.Milliseconds(), ChapterMatch: actLabel.MatchString(strings.TrimSpace(c.Title))})
	}
	return e
}
func breakRunEvidence(runs []breakRun) []BreakSignal {
	out := make([]BreakSignal, 0, len(runs))
	for _, r := range runs {
		out = append(out, BreakSignal{StartMS: int64(math.Round(r.start * 1000)), EndMS: int64(math.Round(r.end * 1000))})
	}
	return out
}
