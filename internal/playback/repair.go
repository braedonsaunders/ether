package playback

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

type repairEventKey struct {
	article int
	attempt uint64
}
type recoveryState struct {
	mu          sync.Mutex
	events      map[repairEventKey]core.RepairEvent
	completed   map[int]uint64
	monitoring  bool
	nextAttempt time.Time
}

var _ core.PlaybackTimeline = (*Manager)(nil)

func (m *Manager) PlaybackProgress(userID int64, clientID string, key core.RatingKey, position time.Duration, state string) {
	m.mu.Lock()
	var sessions []*session
	for _, s := range m.sessions {
		if s.ident.userID == userID && s.ident.key == key && (s.ident.clientID == clientID || s.caps.ClientID == clientID || strings.HasSuffix(s.ident.clientID, ":"+string(s.caps.Protocol)+clientID)) {
			sessions = append(sessions, s)
		}
	}
	m.mu.Unlock()
	now := m.now()
	for _, s := range sessions {
		s.mu.Lock()
		s.viewerPosition = max(0, position)
		s.viewerAt = time.Now()
		s.viewerState = state
		if state == "playing" || state == "paused" {
			s.lastActive = now
			s.stopAt = time.Time{}
		} else if state == "stopped" && s.stopAt.IsZero() {
			s.stopAt = now.Add(m.opts.StoppedGrace)
		}
		s.mu.Unlock()
		m.maintainReserve(s, position, state)
	}
	if state == "stopped" && len(sessions) > 0 {
		m.reg.poke()
		m.kickPins()
	}
}

// Never infer viewer position from ffmpeg's input read offset: probes routinely
// read the tail. Prefer recent authenticated client telemetry, otherwise use
// the last requested output segment as a conservative position.
func (m *Manager) repairHorizon(g *generation, event core.RepairEvent) (position, horizon time.Duration, known bool) {
	hls, ok := m.deps.HLSLookup(g.sess.id)
	if !ok || hls == nil {
		return 0, 0, false
	}
	buffered, ok := hls.(core.RecoveryBufferSource)
	if !ok {
		return 0, 0, false
	}
	b := buffered.RecoveryBuffer()
	if b.ReadyUntil <= 0 {
		return b.Position, 0, false
	}
	position = b.Position
	s := g.sess
	s.mu.Lock()
	at, p, state := s.viewerAt, s.viewerPosition, s.viewerState
	s.mu.Unlock()
	// Equal timestamps cannot establish that telemetry followed the seek on a
	// coarse clock. Keep the newer requested segment until a later update.
	if !at.IsZero() && at.After(b.SeekAt) && time.Since(at) < 30*time.Second {
		position = p
		if state == "playing" {
			position += time.Since(at)
		}
	}
	horizon = max(0, b.ReadyUntil-position)
	if event.Offset >= 0 {
		tm := g.timeMapSnapshot()
		if !tm.Valid() {
			return position, 0, false
		}
		until := tm.TimeAt(event.Offset) - position
		if event.Approximate {
			until /= 2
		}
		horizon = min(horizon, max(0, until))
	}
	return position, horizon, true
}

func (m *Manager) noteRepair(g *generation, event core.RepairEvent) core.RepairWindow {
	if g.ctx.Err() != nil || g.sess.isClosed() || g.sess.current() != g {
		return core.RepairWindow{Stop: true}
	}
	d := &g.recovery
	d.mu.Lock()
	key := repairEventKey{event.Key, event.Attempt}
	if d.events == nil {
		d.events = map[repairEventKey]core.RepairEvent{}
	}
	if d.completed == nil {
		d.completed = map[int]uint64{}
	}
	if event.Phase == "repaired" {
		if len(d.completed) >= 64 {
			clear(d.completed)
		}
		d.completed[event.Key] = max(d.completed[event.Key], event.Attempt)
		for k := range d.events {
			if k.article == event.Key && k.attempt <= event.Attempt {
				delete(d.events, k)
			}
		}
	} else if event.Phase == "obsolete" {
		delete(d.events, key)
	} else if completed, ok := d.completed[event.Key]; ok && event.Attempt <= completed {
		// A late ticker/failure from an older waiter cannot undo verified repair.
	} else if len(d.events) < 64 || d.events[key].Phase != "" {
		// A second waiter must not erase an already measured plan.
		if old := d.events[key]; event.Phase != "missing" || old.Phase == "" {
			d.events[key] = event
		}
	}
	start := !d.monitoring && len(d.events) > 0
	if start {
		d.monitoring = true
	}
	d.mu.Unlock()
	if event.Phase != "repaired" && event.Phase != "obsolete" {
		m.prepareDamageAlternate(g)
	}
	if start {
		m.mu.Lock()
		if !m.closed {
			m.wg.Add(1)
			go m.monitorRepair(g)
		}
		m.mu.Unlock()
	}
	_, horizon, known := m.repairHorizon(g, event)
	w := core.RepairWindow{}
	if known && event.Offset >= 0 && horizon > time.Second {
		w.Deadline = time.Now().Add(horizon - time.Second)
	}
	info := g.infoSnapshot()
	if info.Duration > 0 {
		lead := min(90*time.Second, max(20*time.Second, event.Estimate*2+8*time.Second))
		w.ReadAheadBytes = int64(float64(g.src.Size()) * float64(lead) / float64(info.Duration))
	}
	return w
}

func (m *Manager) repairMustSwitch(g *generation) bool {
	if g.ctx.Err() != nil || g.sess.current() != g {
		return false
	}
	d := &g.recovery
	d.mu.Lock()
	events := make([]core.RepairEvent, 0, len(d.events))
	for _, e := range d.events {
		events = append(events, e)
	}
	d.mu.Unlock()
	for _, event := range events {
		if event.Offset < 0 {
			continue
		}
		position, lead, known := m.repairHorizon(g, event)
		if !known {
			continue
		} // direct play retains its representation-safe error path
		if g.timeMapSnapshot().TimeAt(event.Offset)+2*time.Second < position {
			continue
		}
		// Leave time to open/probe and restart an alternate. A completed repair
		// removes the event, including while an alternate is being prepared.
		if event.Phase == "failed" || (event.Estimate > 0 && event.Estimate+3*time.Second >= lead) || lead <= 3*time.Second {
			return true
		}
	}
	return false
}

func (m *Manager) monitorRepair(g *generation) {
	defer m.wg.Done()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-g.ctx.Done():
			return
		case <-m.ctx.Done():
			return
		case <-tick.C:
			d := &g.recovery
			d.mu.Lock()
			if len(d.events) == 0 {
				d.monitoring = false
				d.mu.Unlock()
				return
			}
			ready := !time.Now().Before(d.nextAttempt)
			d.mu.Unlock()
			if !ready || !m.repairMustSwitch(g) || !g.backgroundFailover.CompareAndSwap(false, true) {
				continue
			}
			ctx, cancel := context.WithTimeout(g.sess.stopCtx, m.opts.FailoverTimeout)
			position, _, _ := m.repairHorizon(g, core.RepairEvent{Offset: -1})
			_, err := m.failoverWith(ctx, g.sess, g, position, failoverOpts{reason: "repair cannot beat playback deadline", followPlayer: true, quietFailure: true, preserveBuffer: true, stillNeeded: func() bool { return m.repairMustSwitch(g) }})
			cancel()
			g.backgroundFailover.Store(false)
			if err != nil {
				m.log.Debug("alternate recovery not ready; preserving original stream", "session", g.sess.id, "err", err)
			}
			d.mu.Lock()
			d.nextAttempt = time.Now().Add(3 * time.Second)
			d.mu.Unlock()
		}
	}
}
