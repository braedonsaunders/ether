package playback

import (
	"context"
	"sort"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// pinLive protects a release served by a live session. Pins are
// reference-counted here so several sessions on one release share a pin.
func (m *Manager) pinLive(releaseID string) {
	if m.deps.Cache == nil || releaseID == "" {
		return
	}
	m.pinMu.Lock()
	defer m.pinMu.Unlock()
	m.livePins[releaseID]++
	if m.livePins[releaseID] == 1 {
		if err := m.deps.Cache.Pin(releaseID, PinReasonLive); err != nil {
			m.log.Warn("cache pin failed", "release", releaseID, "err", err)
		}
	}
}

func (m *Manager) unpinLive(releaseID string) {
	if m.deps.Cache == nil || releaseID == "" {
		return
	}
	m.pinMu.Lock()
	defer m.pinMu.Unlock()
	n, ok := m.livePins[releaseID]
	if !ok {
		return
	}
	if n > 1 {
		m.livePins[releaseID] = n - 1
		return
	}
	delete(m.livePins, releaseID)
	if err := m.deps.Cache.Unpin(releaseID, PinReasonLive); err != nil {
		m.log.Warn("cache unpin failed", "release", releaseID, "err", err)
	}
}

// kickPins asks the pin loop to reconcile soon.
func (m *Manager) kickPins() {
	select {
	case m.pinKick <- struct{}{}:
	default:
	}
}

func (m *Manager) pinLoop() {
	// Resume records expire whether or not a segment cache is configured;
	// pins are only managed when there is one.
	if m.resume == nil {
		<-m.ctx.Done()
		return
	}
	if m.ctx.Err() == nil {
		m.reconcileAndLog()
	}
	t := time.NewTicker(m.opts.PinInterval)
	defer t.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-t.C:
		case <-m.pinKick:
			// Debounce bursts of session ends. Cancellation ends the wait;
			// a fired debounce must not start another reconcile after stop.
			select {
			case <-m.ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
		if m.ctx.Err() != nil {
			return
		}
		m.reconcileAndLog()
	}
}

func (m *Manager) reconcileAndLog() {
	ctx, cancel := context.WithTimeout(m.ctx, time.Minute)
	defer cancel()
	if err := m.ReconcilePins(ctx); err != nil && m.ctx.Err() == nil {
		m.log.Warn("pin reconcile failed", "err", err)
	}
}

// Defaults for the in-progress pin policy (config.Cache).
const (
	defaultPinMaxAge  = 30 * 24 * time.Hour
	defaultPinPercent = 50
)

// pinPolicy returns how long an in-progress title stays pinned without
// watch activity and the share of the cache budget in-progress pins may use.
func (m *Manager) pinPolicy() (maxAge time.Duration, percent int) {
	c := m.config().Cache
	maxAge, percent = c.InProgressPinMaxAge.D(), c.InProgressPinPercent
	if maxAge <= 0 {
		maxAge = defaultPinMaxAge
	}
	if percent <= 0 {
		percent = defaultPinPercent
	}
	if percent > 100 {
		percent = 100
	}
	return maxAge, percent
}

// releaseSizer is implemented by caches that can report a release's cached
// bytes (segcache.Cache).
type releaseSizer interface {
	ReleaseBytes(releaseID string) int64
}

// pinLister is implemented by caches that can list their pins
// (segcache.Cache): release id -> pin reasons.
type pinLister interface {
	Pins() map[string][]string
}

// pinCandidate is a release some user is part-way through.
type pinCandidate struct {
	id     string
	active time.Time // most recent watch activity across its users
	size   int64     // media size from the resume record (fallback estimate)
}

// ReconcilePins makes the cache's in-progress pins match the resume
// records of titles in progress for any user: each such user's release is
// pinned, releases no longer in progress are unpinned, and records past
// Options.ResumeRetention that are neither in progress nor live are
// deleted. Only titles watched within Cache.InProgressPinMaxAge are pinned,
// and pins together may hold at most Cache.InProgressPinPercent of the
// cache budget (most recently watched first), so abandoned half-watched
// titles cannot hold the cache over budget forever. The first pass also
// unpins stale pins from a previous run. Resume expiry runs even without a
// cache.
func (m *Manager) ReconcilePins(ctx context.Context) error {
	if m.resume == nil {
		return nil
	}
	m.waitResumeSaves()
	m.resume.payload.scheduleRecovery()
	recs, err := m.resume.all(ctx)
	if err != nil {
		return err
	}
	inProgress := map[core.RatingKey]bool{}
	if m.deps.State != nil {
		keys, err := m.deps.State.InProgressAny(ctx)
		if err != nil {
			return err
		}
		for _, k := range keys {
			inProgress[k] = true
		}
	}
	live := map[identity]bool{}
	liveRel := map[string]bool{}
	m.mu.Lock()
	for _, s := range m.sessions {
		live[identity{userID: s.ident.userID, key: s.ident.key}] = true
		if g := s.current(); g != nil {
			liveRel[g.releaseID()] = true
		}
	}
	m.mu.Unlock()

	now := m.now()
	maxAge, percent := m.pinPolicy()
	cands := map[string]*pinCandidate{}
	var expired []resumeRecord
	for _, rec := range recs {
		if err := ctx.Err(); err != nil {
			return err
		}
		progress := false
		active := rec.updatedAt
		if inProgress[rec.key] {
			st, err := m.deps.State.Get(ctx, rec.userID, rec.key)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				// Unknown: keep the pin rather than risk evicting a resume.
				progress = true
			} else {
				progress = st.ViewOffset > 0
				for _, t := range []time.Time{st.UpdatedAt, st.LastViewedAt} {
					if t.After(active) {
						active = t
					}
				}
			}
		}
		if progress {
			c := cands[rec.releaseID]
			if c == nil {
				c = &pinCandidate{id: rec.releaseID}
				cands[rec.releaseID] = c
			}
			if active.After(c.active) {
				c.active = active
			}
			if rec.size > c.size {
				c.size = rec.size
			}
			continue
		}
		if !live[identity{userID: rec.userID, key: rec.key}] && now.Sub(rec.updatedAt) > m.opts.ResumeRetention {
			expired = append(expired, rec)
		}
	}

	if m.deps.Cache != nil {
		m.applyPins(recs, m.selectPins(cands, now, maxAge, percent), liveRel)
	}

	for _, rec := range expired {
		if err := m.resume.delete(ctx, rec.userID, rec.key); err != nil {
			return err
		}
	}
	return nil
}

// selectPins picks which in-progress releases to pin: those with activity
// within maxAge, most recent first, while their cached bytes fit within
// percent of the cache budget. A release that does not fit is skipped so
// smaller, older ones may still be kept.
func (m *Manager) selectPins(cands map[string]*pinCandidate, now time.Time, maxAge time.Duration, percent int) map[string]bool {
	list := make([]*pinCandidate, 0, len(cands))
	for _, c := range cands {
		if now.Sub(c.active) <= maxAge {
			list = append(list, c)
		}
	}
	sort.Slice(list, func(i, j int) bool {
		if !list[i].active.Equal(list[j].active) {
			return list[i].active.After(list[j].active)
		}
		return list[i].id < list[j].id
	})
	_, budget := m.deps.Cache.Usage()
	limit := budget / 100 * int64(percent)
	sizer, _ := m.deps.Cache.(releaseSizer)
	desired := make(map[string]bool, len(list))
	var used int64
	for _, c := range list {
		if budget > 0 {
			size := c.size
			if sizer != nil {
				size = sizer.ReleaseBytes(c.id)
			}
			if used+size > limit {
				m.log.Debug("in-progress pin skipped: pin share of cache budget full", "release", c.id, "bytes", size)
				continue
			}
			used += size
		}
		desired[c.id] = true
	}
	return desired
}

// applyPins pins and unpins releases so the in-progress pins equal desired.
func (m *Manager) applyPins(recs []resumeRecord, desired, liveRel map[string]bool) {
	m.pinMu.Lock()
	defer m.pinMu.Unlock()
	for id := range desired {
		if !m.progressPins[id] {
			if err := m.deps.Cache.Pin(id, PinReasonInProgress); err != nil {
				m.log.Warn("cache pin failed", "release", id, "err", err)
				continue
			}
			m.progressPins[id] = true
		}
	}
	for id := range m.progressPins {
		if !desired[id] {
			if err := m.deps.Cache.Unpin(id, PinReasonInProgress); err != nil {
				m.log.Warn("cache unpin failed", "release", id, "err", err)
				continue
			}
			delete(m.progressPins, id)
		}
	}
	if !m.pinsSwept {
		// Pins may outlive the process; drop ours that are no longer wanted.
		for _, rec := range recs {
			if !desired[rec.releaseID] {
				_ = m.deps.Cache.Unpin(rec.releaseID, PinReasonInProgress)
			}
			if !liveRel[rec.releaseID] && m.livePins[rec.releaseID] == 0 {
				_ = m.deps.Cache.Unpin(rec.releaseID, PinReasonLive)
			}
		}
		// Also drop our pins on releases that no longer have any resume
		// record (e.g. expired while the cache was unavailable).
		if pl, ok := m.deps.Cache.(pinLister); ok {
			for id, reasons := range pl.Pins() {
				for _, r := range reasons {
					switch {
					case r == PinReasonInProgress && !desired[id]:
						_ = m.deps.Cache.Unpin(id, r)
					case r == PinReasonLive && !liveRel[id] && m.livePins[id] == 0:
						_ = m.deps.Cache.Unpin(id, r)
					}
				}
			}
		}
		m.pinsSwept = true
	}
}
