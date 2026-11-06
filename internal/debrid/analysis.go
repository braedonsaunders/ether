package debrid

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

type analysisAccount struct {
	reading                             bool
	readCancel                          context.CancelFunc
	lease                               *analysisLease
	until, next, liveUntil, lastLimited time.Time
	strikes                             int
}
type analysisLease struct {
	done  <-chan struct{}
	until time.Time
}

func (l *analysisLease) active(now time.Time) bool {
	if l == nil || !now.Before(l.until) {
		return false
	}
	select {
	case <-l.done:
		return false
	default:
		return true
	}
}
func (s *Service) analysisState(id string) *analysisAccount {
	if s.analysis == nil {
		s.analysis = map[string]*analysisAccount{}
	}
	state := s.analysis[id]
	if state == nil {
		state = &analysisAccount{}
		s.analysis[id] = state
	}
	return state
}
func analysisUnavailable() error {
	return fmt.Errorf("debrid account unavailable for background analysis: %w", core.ErrBudgetExhausted)
}

// BeginAnalysis conservatively claims every configured account before resolution,
// so an alternate-account resolve cannot escape admission. IDs identify configured
// account records, never credential material. No provider call is made here.
func (s *Service) BeginAnalysis(ctx context.Context) (func(), error) {
	deadline, ok := ctx.Deadline()
	if !ok || ctx.Err() != nil {
		return nil, analysisUnavailable()
	}
	s.mu.RLock()
	entries := append([]*entry(nil), s.entries...)
	s.mu.RUnlock()
	now := s.now()
	s.analysisMu.Lock()
	defer s.analysisMu.Unlock()
	var ids []string
	for _, entry := range entries {
		entry.mu.Lock()
		id, enabled, paused := entry.cfg.ID, entry.cfg.Enabled, now.Before(entry.until)
		entry.mu.Unlock()
		if !enabled {
			continue
		}
		state := s.analysisState(id)
		if paused || now.Before(state.until) || now.Before(state.liveUntil) || state.lease.active(now) || state.reading {
			return nil, analysisUnavailable()
		}
		ids = append(ids, id)
	}
	lease := &analysisLease{done: ctx.Done(), until: now.Add(min(time.Until(deadline), 10*time.Minute))}
	for _, id := range ids {
		s.analysisState(id).lease = lease
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			s.analysisMu.Lock()
			defer s.analysisMu.Unlock()
			for _, id := range ids {
				if state := s.analysis[id]; state != nil && state.lease == lease {
					state.lease = nil
				}
			}
		})
	}, nil
}

func (s *Service) checkAnalysisAccount(id string) error {
	now := s.now()
	s.mu.RLock()
	paused := false
	for _, entry := range s.entries {
		entry.mu.Lock()
		match := entry.cfg.ID == id
		until := entry.until
		entry.mu.Unlock()
		if match && now.Before(until) {
			paused = true
			break
		}
	}
	s.mu.RUnlock()
	if paused {
		return analysisUnavailable()
	}
	s.analysisMu.Lock()
	defer s.analysisMu.Unlock()
	state := s.analysisState(id)
	if !state.lease.active(now) || now.Before(state.until) || now.Before(state.liveUntil) {
		return analysisUnavailable()
	}
	return nil
}
func (s *Service) paceAnalysis(ctx context.Context, id string) error {
	for {
		if err := s.checkAnalysisAccount(id); err != nil {
			return err
		}
		now := s.now()
		s.analysisMu.Lock()
		state := s.analysisState(id)
		wait := state.next.Sub(now)
		if wait <= 0 {
			state.next = now.Add(500 * time.Millisecond)
			s.analysisMu.Unlock()
			return nil
		}
		s.analysisMu.Unlock()
		if err := sleep(ctx, wait); err != nil {
			return err
		}
	}
}

// Any live reader makes admission idle-only, stronger than waiting for a slot
// limit. Reads already in flight retain urgent priority and are never cancelled.
func (s *Service) noteAnalysisLive(id string) {
	s.analysisMu.Lock()
	state := s.analysisState(id)
	state.liveUntil = s.now().Add(2 * time.Minute)
	cancel := state.readCancel
	s.analysisMu.Unlock()
	if cancel != nil {
		cancel()
	}
}
func (s *Service) pauseAnalysisAccount(id string, after time.Duration) {
	now := s.now()
	s.analysisMu.Lock()
	state := s.analysisState(id)
	if now.Sub(state.lastLimited) > time.Hour {
		state.strikes = 0
	}
	state.strikes = min(state.strikes+1, 7)
	state.lastLimited = now
	delay := max(after, 30*time.Second)
	if delay <= 30*time.Minute {
		delay = min(30*time.Minute, delay*time.Duration(1<<uint(state.strikes-1)))
	}
	var entropy [2]byte
	_, _ = rand.Read(entropy[:])
	jitter := time.Duration(binary.LittleEndian.Uint16(entropy[:])%1000) * time.Millisecond
	state.until = maxTimeAnalysis(state.until, now.Add(delay+jitter))
	s.analysisMu.Unlock()
	// Reuse existing account-wide cluster backoff broadcasts, not new lease infra.
	s.mu.RLock()
	var account *entry
	for _, candidate := range s.entries {
		if candidate.cfg.ID == id {
			account = candidate
			break
		}
	}
	s.mu.RUnlock()
	if account != nil {
		s.observe(account, &rateLimitError{retryAfter: delay + jitter})
	}
}
func maxTimeAnalysis(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

type analysisReadKey struct{}

func isAnalysisRead(ctx context.Context) bool {
	v, _ := ctx.Value(analysisReadKey{}).(bool)
	return v || core.IsBackgroundAnalysis(ctx)
}

type analysisCounters struct {
	requests, bytes, errors atomic.Int64
	active                  atomic.Int64
}

func (s *httpSource) ReadAtAnalysisContext(ctx context.Context, p []byte, off int64) (int, error) {
	payload, _ := s.f.payload()
	if s.f.e.svc == nil {
		return 0, analysisUnavailable()
	}
	if err := s.f.e.svc.checkAnalysisAccount(payload.Provider); err != nil {
		return 0, err
	}
	return s.read(context.WithValue(ctx, analysisReadKey{}, true), p, off, fetchPrefetch, false)
}
func (s *httpSource) AnalysisStats() (int64, int64, int64) {
	c := &s.f.analysis
	return c.requests.Load(), c.bytes.Load(), c.errors.Load()
}

type analysisBody struct {
	ctx context.Context
	io.ReadCloser
	counters *analysisCounters
	totals   *analysisCounters
}

func (b *analysisBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.counters.bytes.Add(int64(n))
	if b.totals != nil {
		b.totals.bytes.Add(int64(n))
	}
	if err != nil && err != io.EOF && !errors.Is(err, context.Canceled) && (b.ctx == nil || b.ctx.Err() == nil) {
		b.counters.errors.Add(1)
		if b.totals != nil {
			b.totals.errors.Add(1)
		}
	}
	return n, err
}

// AnalysisContext marks trusted background preparation so probes as well as
// decoder ranges share account admission, pacing and prefetch priority.
func AnalysisContext(ctx context.Context) context.Context {
	return core.WithBackgroundAnalysis(ctx)
}
func (s *Service) AnalysisStats() (int64, int64, int64) {
	return s.analysisTotals.requests.Load(), s.analysisTotals.bytes.Load(), s.analysisTotals.errors.Load()
}

type analysisActiveBody struct {
	io.ReadCloser
	once sync.Once
	done func()
}

func (b *analysisActiveBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.done)
	return err
}
func (s *Service) DrainAnalysis(ctx context.Context) error {
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for s.analysisTotals.active.Load() > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
	return nil
}

func (s *Service) beginAnalysisRange(ctx context.Context, id string) (context.Context, func(), error) {
	for {
		if err := s.checkAnalysisAccount(id); err != nil {
			return nil, nil, err
		}
		s.analysisMu.Lock()
		state := s.analysisState(id)
		if !state.reading {
			readCtx, cancel := context.WithCancel(ctx)
			state.reading = true
			state.readCancel = cancel
			s.analysisMu.Unlock()
			var once sync.Once
			return readCtx, func() {
				once.Do(func() {
					cancel()
					s.analysisMu.Lock()
					state.reading = false
					state.readCancel = nil
					s.analysisMu.Unlock()
				})
			}, nil
		}
		s.analysisMu.Unlock()
		if err := sleep(ctx, 10*time.Millisecond); err != nil {
			return nil, nil, err
		}
	}
}

// AnalysisAllowed is a local, I/O-free check for a running decoder to yield.
func (s *Service) AnalysisAllowed(provider string) bool {
	return s.checkAnalysisAccount(provider) == nil
}
