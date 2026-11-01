package playback

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// Speed-coupled down-failover (#28): when the bytes of a release arrive
// from Usenet slower than the media itself plays, the client will starve.
// Swap to a lower-bitrate candidate before that happens. The slow release
// is not broken, only slow, so it is excluded for this session only and
// never marked bad.

// fetchLatencyMin separates cache-like reads from fetched reads: a chunk
// served from the segment cache returns in well under this, a chunk that
// had to be fetched over NNTP does not. Reads faster than this carry no
// evidence about Usenet throughput and are ignored.
const fetchLatencyMin = 2 * time.Millisecond

const speedWindow = 20 // seconds of fetch evidence considered

// speedTracker accumulates fetched bytes and the wall time those fetches
// took, bucketed per second so old evidence ages out.
type speedTracker struct {
	bytes [speedWindow + 1]int64
	durNs [speedWindow + 1]int64
	secs  [speedWindow + 1]int64
}

func (t *speedTracker) add(n int64, dur time.Duration, now time.Time) {
	sec := now.Unix()
	i := int(sec % int64(len(t.bytes)))
	if i < 0 {
		i = -i
	}
	if t.secs[i] != sec {
		t.secs[i], t.bytes[i], t.durNs[i] = sec, 0, 0
	}
	t.bytes[i] += n
	t.durNs[i] += dur.Nanoseconds()
}

func (t *speedTracker) reset() { *t = speedTracker{} }

// rate is the effective fetch throughput in bytes per second over the
// window: fetched bytes divided by the time fetching them took. ok is
// false until there is at least a second of fetch time and a meaningful
// number of bytes, so a slow first chunk proves nothing.
func (t *speedTracker) rate(now time.Time) (float64, bool) {
	sec := now.Unix()
	var bytes, durNs int64
	for i := range t.bytes {
		if d := sec - t.secs[i]; d >= 0 && d <= speedWindow {
			bytes += t.bytes[i]
			durNs += t.durNs[i]
		}
	}
	if bytes < 256<<10 || durNs < int64(time.Second) {
		return 0, false
	}
	return float64(bytes) / (float64(durNs) / float64(time.Second)), true
}

// mediaBps is the playback rate of a generation in bytes per second.
func mediaBps(info core.MediaInfo, size int64) float64 {
	if info.BitrateKbps > 0 {
		return float64(info.BitrateKbps) * 1000 / 8
	}
	if size > 0 && info.Duration > 0 {
		return float64(size) / info.Duration.Seconds()
	}
	return 0
}

// noteFetch records one source read. Fast (cache-like) reads are dropped.
func (s *session) noteFetch(n int, elapsed time.Duration, now time.Time) {
	if n <= 0 || elapsed < fetchLatencyMin {
		return
	}
	s.speedMu.Lock()
	s.speed.add(int64(n), elapsed, now)
	s.speedMu.Unlock()
}

// noteReadPos tracks the read position for seek detection and for the
// timestamp a down-failover resumes at. A jump restarts the grace period:
// a seek always looks slow for a moment.
func (s *session) noteReadPos(off int64, n int, now time.Time, grace time.Duration) {
	s.speedMu.Lock()
	defer s.speedMu.Unlock()
	if s.expectPos >= 0 && off != s.expectPos {
		s.graceUntil = now.Add(grace)
		s.slowChecks = 0
		s.speed.reset()
	}
	// A tail/index probe or a previous forward seek is not the playhead.
	// Following it with an earlier media read must move this position back.
	s.lastSrcPos = off + int64(n)
	s.expectPos = off + int64(n)
}

// resetSpeed restarts speed evidence for a new generation.
func (s *session) resetSpeed(now time.Time, grace time.Duration) {
	s.speedMu.Lock()
	s.speed.reset()
	s.genSince = now
	s.graceUntil = now.Add(grace)
	s.slowChecks = 0
	s.lastSrcPos = 0
	s.expectPos = -1
	s.speedMu.Unlock()
}

// speedLoop periodically evaluates every session's fetch rate.
func (m *Manager) speedLoop() {
	t := time.NewTicker(m.opts.SpeedCheckInterval)
	defer t.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-t.C:
			m.checkSpeeds()
		}
	}
}

// checkSpeeds runs one down-failover evaluation over all live sessions.
func (m *Manager) checkSpeeds() {
	now := m.now()
	m.mu.Lock()
	list := make([]*session, 0, len(m.sessions))
	for _, s := range m.sessions {
		list = append(list, s)
	}
	m.mu.Unlock()
	for _, s := range list {
		m.checkSpeed(s, now)
	}
}

func (m *Manager) checkSpeed(s *session, now time.Time) {
	g := s.current()
	if g == nil || s.isClosed() {
		return
	}
	s.mu.Lock()
	decision, downs, misses, viewerState := s.decision, s.downFailovers, s.downMisses, s.viewerState
	s.mu.Unlock()
	if downs+misses >= m.opts.MaxDownFailovers || viewerState == "paused" || viewerState == "stopped" {
		return
	}
	hls, hasHLS := m.deps.HLSLookup(s.id)
	if retry, ok := g.src.(core.DirectPlayRetrier); ok && retry.RetryDirectPlay() && !hasHLS && decision == core.DecisionDirectPlay {
		// Byte-range players do not renegotiate when delivery slows. Keep
		// their representation intact; bitrate adaptation belongs to an
		// actual HLS/DASH session, where SwapInput can restart the muxer.
		return
	}
	// Safe swap paths only: an HLS/DASH session restarts ffmpeg at the
	// mapped time; a direct-play session goes through the #5 logic
	// (exact-size splice, TS splice, or a switch to remux).
	if !hasHLS && decision != core.DecisionDirectPlay {
		return
	}
	info := g.infoSnapshot()
	media := mediaBps(info, g.src.Size())
	if media <= 0 {
		return
	}

	blocked := g.reads.longest(now)
	s.speedMu.Lock()
	fetch, ok := s.speed.rate(now)
	age := now.Sub(s.genSince)
	settled := !now.Before(s.graceUntil.Add(-max(0, m.opts.SpeedGrace-6*time.Second)))
	grace := now.Before(s.graceUntil) || age < m.opts.SpeedGrace
	// A source that stopped delivering bytes cannot produce completed-read
	// samples. A live blocked read is evidence; a quiet/paused encoder is not.
	if blocked >= 6*time.Second {
		fetch, ok = 0, true
	}
	if !ok {
		s.speedMu.Unlock()
		return
	}
	ratio := fetch / media
	if ratio < 1 {
		s.slowChecks++
	} else {
		s.slowChecks = 0
	}
	slow := s.slowChecks
	at := g.timeMapSnapshot().TimeAt(s.lastSrcPos)
	s.speedMu.Unlock()
	if age >= 8*time.Second {
		if observer, ok := m.deps.Resolver.(core.SourceDeliveryObserver); ok {
			observer.ObserveSourceDelivery(g.res.Release, fetch)
		}
	}
	// Begin bounded preparation before the buffer is empty. A source slightly
	// faster than average still needs headroom for bursts, but never switch it
	// solely for lacking that headroom.
	if hasHLS && settled && age >= 6*time.Second && ratio < 1.2 {
		m.prepareSpeedAlternate(g)
	}
	_, buffer, bufferKnown := m.repairHorizon(g, core.RepairEvent{Offset: -1})
	urgent := hasHLS && settled && bufferKnown && age >= 6*time.Second && slow >= 2 && ratio < 1 && bufferRunway(buffer, ratio) <= 25*time.Second
	if !urgent && (grace || ratio >= m.opts.SpeedThreshold || slow < m.opts.SpeedSlowChecks) {
		return
	}
	if position, ok := hls.(core.PlaybackPosition); ok {
		at = position.RequestedPosition()
	}
	s.speedMu.Lock()
	s.slowChecks = 0
	s.speedMu.Unlock()
	m.log.Warn("source slower than playback; down-failing over",
		"session", s.id, "key", s.ident.key, "release", g.res.Release.Name,
		"fetchBps", int64(fetch), "mediaBps", int64(media), "at", at,
		"buffer", buffer, "bufferKnown", bufferKnown, "runway", bufferRunway(buffer, ratio), "blockedRead", blocked)
	m.backgroundDownFailover(s, g, at)
}

// noteFailedDownFailover records a down-failover that found nothing
// slower. The releases it opened stay excluded. Once failed and
// successful down-failovers together reach MaxDownFailovers, the
// session stops. Closing the source ends the client read, which is
// the error a player can see; the timeline would otherwise stay
// "playing" while bytes trickle in.
func (m *Manager) noteFailedDownFailover(s *session, tried []string) {
	if m.opts.MaxDownFailovers < 0 || s == nil {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	for _, id := range tried {
		if id != "" && !slices.Contains(s.tried, id) {
			s.tried = append(s.tried, id)
		}
	}
	s.downMisses++
	giveUp := s.downFailovers+s.downMisses >= m.opts.MaxDownFailovers
	s.mu.Unlock()
	if !giveUp {
		return
	}
	m.log.Error("playback starved; no lower-bitrate release worked", "session", s.id, "key", s.ident.key)
	m.Stop(s.id)
}

// backgroundDownFailover runs a speed-triggered failover off the monitor.
func (m *Manager) backgroundDownFailover(s *session, g *generation, at time.Duration) {
	if !g.backgroundFailover.CompareAndSwap(false, true) {
		return
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		g.backgroundFailover.Store(false)
		return
	}
	m.wg.Add(1)
	m.mu.Unlock()
	go func() {
		defer m.wg.Done()
		defer g.backgroundFailover.Store(false)
		ctx, cancel := context.WithTimeout(m.ctx, m.opts.FailoverTimeout)
		defer cancel()
		// Stopping the session ends the swap; retiring g does not.
		if s.stopCtx != nil {
			stop := context.AfterFunc(s.stopCtx, cancel)
			defer stop()
		}
		if _, err := m.failoverWith(ctx, s, g, at, failoverOpts{
			reason:       "source slower than playback",
			followPlayer: true, preserveBuffer: true,
			markBad: false,
			down:    true,
		}); err != nil && ctx.Err() == nil && !errors.Is(err, errRequireRemux) {
			m.log.Error("down-failover failed", "session", s.id, "err", err)
		}
	}()
}
