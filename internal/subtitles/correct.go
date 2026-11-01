package subtitles

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/transcode"
)

const (
	correctBackgroundBudget = 12 * time.Second
	correctRetryBase        = 30 * time.Second
	correctRetryMax         = 15 * time.Minute
)

// ReleaseAccess is enough of a playing session to measure a sidecar.
// Reader and URL must belong to that session's lifetime; Enrich must
// not pass a prepare-generation handle.
type ReleaseAccess struct {
	Reader   io.ReaderAt
	Size     int64
	URL      string
	Duration time.Duration
	Info     *core.MediaInfo
	Source   string
}

type correction struct {
	Offset     time.Duration `json:"offset"`
	Rate       float64       `json:"rate"`
	Confidence float64       `json:"confidence"`
	Inliers    int           `json:"inliers"`
	Applied    bool          `json:"applied"`
	Measured   bool          `json:"measured"`
	At         time.Time     `json:"at"`
	Err        string        `json:"err,omitempty"`
	Attempts   int           `json:"attempts,omitempty"`
	RetryAfter time.Time     `json:"retryAfter,omitempty"`
}

func (c correction) fit() TimingFit {
	return TimingFit{Offset: c.Offset, Rate: c.Rate, Confidence: c.Confidence, Inliers: c.Inliers}
}

func sourceKey(access ReleaseAccess) string {
	if s := strings.TrimSpace(access.Source); s != "" {
		return sanitizeSource(s)
	}
	if access.URL != "" {
		sum := sha1.Sum([]byte(access.URL))
		return hex.EncodeToString(sum[:8])
	}
	return ""
}

func sanitizeSource(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' || r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
		if b.Len() >= 80 {
			break
		}
	}
	if b.Len() == 0 {
		sum := sha1.Sum([]byte(s))
		return hex.EncodeToString(sum[:8])
	}
	return b.String()
}

func (s *Service) correctionPath(id, source string) string {
	if s == nil || s.dir == "" || id == "" || source == "" {
		return ""
	}
	return filepath.Join(s.dir, "correct", id, source+".json")
}

func (s *Service) loadCorrection(id, source string) (correction, bool) {
	p := s.correctionPath(id, source)
	if p == "" {
		return correction{}, false
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return correction{}, false
	}
	var c correction
	if json.Unmarshal(raw, &c) != nil {
		return correction{}, false
	}
	return c, true
}

func (s *Service) storeCorrection(id, source string, c correction) {
	p := s.correctionPath(id, source)
	if p == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return
	}
	_ = writeFileAtomic(p, raw)
}

type flightGroup struct {
	mu sync.Mutex
	m  map[string]chan struct{}
}

func (g *flightGroup) run(id string, fn func()) {
	g.mu.Lock()
	if g.m == nil {
		g.m = map[string]chan struct{}{}
	}
	if _, ok := g.m[id]; ok {
		g.mu.Unlock()
		return
	}
	done := make(chan struct{})
	g.m[id] = done
	g.mu.Unlock()
	go func() {
		defer func() {
			g.mu.Lock()
			delete(g.m, id)
			g.mu.Unlock()
			close(done)
		}()
		fn()
	}()
}

// StartCorrection measures the cached sidecar against the live session's
// source. It no-ops without a source handle (no prefetch).
func (s *Service) StartCorrection(title *core.Title, id string, access ReleaseAccess) {
	if s == nil {
		return
	}
	id = strings.TrimPrefix(id, URLPrefix)
	if id == "" || strings.ContainsAny(id, "/\\") {
		return
	}
	src := sourceKey(access)
	if src == "" || (access.Reader == nil && access.URL == "") {
		return
	}
	if c, ok := s.loadCorrection(id, src); ok {
		if c.Measured || c.Applied {
			return
		}
		if c.Err != "" && !s.now().UTC().After(c.RetryAfter) {
			return
		}
	}
	s.correctFlights.run(id+"\x00"+src, func() {
		if s.speechSem != nil {
			s.speechSem <- struct{}{}
			defer func() { <-s.speechSem }()
		}
		ctx, cancel := context.WithTimeout(context.Background(), correctBackgroundBudget)
		defer cancel()
		s.measureAndStore(ctx, title, id, src, access)
	})
}

func (s *Service) measureAndStore(ctx context.Context, title *core.Title, id, source string, access ReleaseAccess) {
	if access.Reader == nil && access.URL == "" {
		return
	}
	prev, _ := s.loadCorrection(id, source)
	cues, err := s.cuesByCachedID(id)
	if err != nil || len(cues) < 3 {
		s.storeError(id, source, prev, "cues")
		return
	}
	fit, ok := s.measureFit(ctx, cues, access)
	if !ok {
		s.storeError(id, source, prev, "measure")
		return
	}
	c := correction{
		Offset: fit.Offset, Rate: fit.Rate, Confidence: fit.Confidence,
		Inliers: fit.Inliers, Measured: true, At: s.now().UTC(),
	}
	if !fit.Identity() {
		c.Applied = true
	}
	s.storeCorrection(id, source, c)
}

func (s *Service) storeError(id, source string, prev correction, reason string) {
	n := prev.Attempts + 1
	wait := correctRetryBase * time.Duration(1<<min(n-1, 5))
	if wait > correctRetryMax {
		wait = correctRetryMax
	}
	s.storeCorrection(id, source, correction{
		Err: reason, Attempts: n, RetryAfter: s.now().UTC().Add(wait), At: s.now().UTC(),
	})
}

func (s *Service) cuesByCachedID(id string) ([]Cue, error) {
	id = strings.TrimPrefix(id, URLPrefix)
	if s.dir == "" || id == "" || strings.ContainsAny(id, "/\\") {
		return nil, core.ErrNotFound
	}
	raw, err := os.ReadFile(filepath.Join(s.dir, id+".srt"))
	if err != nil {
		return nil, err
	}
	return ParseCues(string(raw))
}

func (s *Service) measureFit(ctx context.Context, cues []Cue, access ReleaseAccess) (TimingFit, bool) {
	if access.Reader != nil {
		ref, err := ReadEmbeddedTextCues(ctx, access.Reader, access.Size)
		if err == nil && len(ref) >= 3 {
			if fit, ok := FitTiming(cues, ref); ok {
				return fit, true
			}
		}
	}
	if ctx.Err() != nil || access.URL == "" {
		return TimingFit{}, false
	}
	ff := ffmpegPath()
	if ff == "" {
		return TimingFit{}, false
	}
	windows, err := DecodeAudioWindows(ctx, ff, access.URL, access.Duration, 4, 8*time.Second)
	if err != nil || len(windows) == 0 {
		return TimingFit{}, false
	}
	return FitSpeechEnergy(cues, windows)
}

func ffmpegPath() string {
	if p := transcode.LookPath(); p != "" {
		return p
	}
	return ""
}

func applyCorrection(cues []Cue, c correction) []Cue {
	if !c.Applied || c.Rate == 0 {
		return cues
	}
	out, err := Shift(cues, c.Offset, c.Rate)
	if err != nil || len(out) == 0 {
		return cues
	}
	return out
}
