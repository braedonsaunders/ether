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
)

const (
	correctBackgroundBudget = 12 * time.Second
	correctRetryBase        = 30 * time.Second
	correctRetryMax         = 15 * time.Minute
	correctServeWait        = 2 * time.Second
	sourceReadBudget        = 32 << 20
	manualSource            = "manual"
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
	FFmpeg   string
	Done     func()
}

func (a ReleaseAccess) close() {
	if a.Done != nil {
		a.Done()
	}
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

// SourceKey is the on-disk correction key for a release/source id.
// URLs and path-like values are hashed so they never become path
// components. Store and load both go through this function.
func SourceKey(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.Contains(raw, "://") || strings.ContainsAny(raw, "/\\") {
		sum := sha1.Sum([]byte(raw))
		return hex.EncodeToString(sum[:8])
	}
	return sanitizeSource(raw)
}

func sourceKey(access ReleaseAccess) string {
	if s := strings.TrimSpace(access.Source); s != "" {
		return SourceKey(s)
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

func (s *Service) loadApplied(id, source string) (correction, bool) {
	if c, ok := s.loadCorrection(id, source); ok && c.Applied {
		return c, true
	}
	if source != manualSource {
		if c, ok := s.loadCorrection(id, manualSource); ok && c.Applied {
			return c, true
		}
	}
	return correction{}, false
}

func (s *Service) loadCorrection(id, source string) (correction, bool) {
	source = SourceKey(source)
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
	source = SourceKey(source)
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

func (g *flightGroup) run(id string, fn func()) bool {
	g.mu.Lock()
	if g.m == nil {
		g.m = map[string]chan struct{}{}
	}
	if _, ok := g.m[id]; ok {
		g.mu.Unlock()
		return false
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
	return true
}

// StartCorrection measures the cached sidecar against the live session's
// source. It no-ops without a source handle (no prefetch). The first
// serve after a pick is uncorrected unless this measurement finishes
// before OpenFormatFrom's short wait.
func (s *Service) StartCorrection(ctx context.Context, title *core.Title, id string, access ReleaseAccess) {
	if ctx == nil {
		ctx = context.Background()
	}
	if s == nil {
		access.close()
		return
	}
	id = strings.TrimPrefix(id, URLPrefix)
	if id == "" || strings.ContainsAny(id, "/\\") {
		access.close()
		return
	}
	src := sourceKey(access)
	if src == "" || (access.Reader == nil && access.URL == "") {
		access.close()
		return
	}
	if c, ok := s.loadCorrection(id, src); ok {
		if c.Measured || c.Applied {
			access.close()
			return
		}
		if c.Err != "" && !s.now().UTC().After(c.RetryAfter) {
			access.close()
			return
		}
	}
	if !s.correctFlights.run(id+"\x00"+src, func() {
		defer access.close()
		if s.speechSem != nil {
			select {
			case s.speechSem <- struct{}{}:
				defer func() { <-s.speechSem }()
			case <-ctx.Done():
				return
			}
		}
		ctx, cancel := context.WithTimeout(ctx, correctBackgroundBudget)
		defer cancel()
		s.measureAndStore(ctx, title, id, src, access)
	}) {
		access.close()
	}
}

func (g *flightGroup) wait(id string, d time.Duration) bool {
	g.mu.Lock()
	ch, ok := g.m[id]
	g.mu.Unlock()
	if !ok {
		return false
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ch:
		return true
	case <-t.C:
		return false
	}
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

func (s *Service) SetFFmpeg(path string) {
	if s != nil {
		s.ffmpeg = path
	}
}

func (s *Service) measureFit(ctx context.Context, cues []Cue, access ReleaseAccess) (TimingFit, bool) {
	if access.Reader != nil {
		ref, err := ReadEmbeddedTextCues(ctx, limitReaderAt(access.Reader, sourceReadBudget), access.Size)
		if err == nil && len(ref) >= 3 {
			if fit, ok := FitTiming(cues, ref); ok {
				return fit, true
			}
		}
	}
	if ctx.Err() != nil || access.URL == "" {
		return TimingFit{}, false
	}
	ff := access.FFmpeg
	if ff == "" && s != nil {
		ff = s.ffmpeg
	}
	if ff == "" {
		return TimingFit{}, false
	}
	windows, err := DecodeAudioWindows(ctx, ff, access.URL, access.Duration, 4, 8*time.Second)
	if err != nil || len(windows) == 0 {
		return TimingFit{}, false
	}
	return FitSpeechEnergy(cues, windows)
}

type limitedReaderAt struct {
	r      io.ReaderAt
	remain *int64
}

func limitReaderAt(r io.ReaderAt, budget int64) io.ReaderAt {
	if r == nil || budget <= 0 {
		return r
	}
	n := budget
	return &limitedReaderAt{r: r, remain: &n}
}

func (l *limitedReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if l == nil || l.r == nil || l.remain == nil {
		return 0, io.EOF
	}
	left := *l.remain
	if left <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > left {
		p = p[:left]
	}
	n, err := l.r.ReadAt(p, off)
	if n > 0 {
		*l.remain -= int64(n)
	}
	return n, err
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
