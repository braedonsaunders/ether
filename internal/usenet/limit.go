package usenet

import (
	"context"
	"errors"
	"math/rand/v2"
	"regexp"
	"strconv"
	"time"
)

// Server-side connection limits are learned, never configured. A refusal
// (400/502 greeting, or 481/482 naming connections) caps the pool:
//
//   - When the server states its limit N ("connection limit (N)", "max N
//     connections"), the pool never runs above N minus ~10% headroom (at
//     least one slot), leaving room for sockets the server is still tearing
//     down. That ceiling only ever goes down for the life of the pool.
//   - Whether or not N is stated, the pool is also capped at what the server
//     was carrying when it refused, and widens by one connection per
//     limitTiming.widen while requests succeed, until the next refusal or
//     the configured count (or ceiling) is reached.
//
// A refusal shortly after Ether itself closed a socket (a cancelled fetch
// closes its connection) is the server still counting that socket, not a
// limit: the dial is retried after a short pause instead.
//
// Failed handshakes pace the next dial on this pool: refusals by a fixed
// pause, other failures exponentially (until the circuit breaker takes
// over). Dials are never limited in number and never paced before a failure,
// so parallel cold connections keep first frame fast.
//
// The learned state lives in memory only. A fresh process relearns it with
// a single refusal, while a persisted cap could strand capacity long after
// the owner upgraded the account or another client stopped sharing it.

// limitTiming holds the learning and pacing intervals. Zero fields take the
// defaults below; tests shorten them.
type limitTiming struct {
	widen         time.Duration // unknown limit: one more connection per step
	teardownGrace time.Duration // refusals this soon after a local close are not limits
	refusalPause  time.Duration // dial pause after a refusal
	backoffMin    time.Duration // dial pause after a first failed handshake, doubling
	backoffMax    time.Duration
}

const (
	defaultLimitWiden    = 5 * time.Second
	defaultTeardownGrace = 2 * time.Second
	defaultRefusalPause  = 500 * time.Millisecond
	defaultDialBackoff   = 500 * time.Millisecond
	defaultDialBackoffMx = time.Minute
)

func (t *limitTiming) withDefaults() {
	if t.widen <= 0 {
		t.widen = defaultLimitWiden
	}
	if t.teardownGrace <= 0 {
		t.teardownGrace = defaultTeardownGrace
	}
	if t.refusalPause <= 0 {
		t.refusalPause = defaultRefusalPause
	}
	if t.backoffMin <= 0 {
		t.backoffMin = defaultDialBackoff
	}
	if t.backoffMax < t.backoffMin {
		t.backoffMax = max(defaultDialBackoffMx, t.backoffMin)
	}
}

// connLimit is a pool's learned limit and dial pacing (guarded by pool.mu).
type connLimit struct {
	stated    int       // limit the server stated in a refusal; 0 = never stated
	ceiling   int       // stated minus headroom, only ever lowered; 0 = none
	cap       int       // throttle at what the server carried; 0 = none
	widenAt   time.Time // next one-connection step of cap
	dialFails int       // consecutive failed handshakes (not refusals)
	dialAfter time.Time // no new handshake starts before this

	// Teardown grace: sockets Ether closed in the window starting at
	// closeWindow, and refusals excused in it. Each close excuses at most
	// two refusals (the redial and its retry), so constant local closes
	// cannot hide a real limit.
	lastClose   time.Time
	closeWindow time.Time
	closes      int
	excused     int
}

// connLimitPatterns find a stated connection limit in refusal text.
var connLimitPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)connection\s+limit\s*(?:of|is|=|:)?\s*\(?\s*(\d+)`),
	regexp.MustCompile(`(?i)too\s+many\s+(?:connections|sessions)\s*\(\s*(\d+)\s*\)`),
	regexp.MustCompile(`(?i)max(?:imum)?\s+(?:of\s+)?(\d+)\s+(?:\w+\s+){0,2}(?:connections|sessions)`),
	regexp.MustCompile(`(?i)(?:connections|sessions)\s+(?:limit|max(?:imum)?)\s*(?:of|is|=|:)?\s*\(?\s*(\d+)`),
	regexp.MustCompile(`(?i)limit(?:ed)?\s+(?:of|to)\s+(\d+)\s+(?:\w+\s+){0,2}(?:connections|sessions)`),
	regexp.MustCompile(`(?i)(\d+)\s+(?:\w+\s+){0,2}(?:connections|sessions)\s+(?:allowed|permitted|max(?:imum)?)`),
}

// parseConnLimit returns the connection limit stated in msg, or 0.
func parseConnLimit(msg string) int {
	for _, re := range connLimitPatterns {
		m := re.FindStringSubmatch(msg)
		if m == nil {
			continue
		}
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 && n <= 10_000 {
			return n
		}
	}
	return 0
}

// statedConnLimit extracts a stated limit from a refusal error chain.
func statedConnLimit(err error) int {
	var pe *ProtocolError
	if errors.As(err, &pe) {
		return parseConnLimit(pe.Msg)
	}
	return 0
}

// limitCeiling is the most connections Ether runs against a stated limit.
func limitCeiling(stated int) int {
	return max(1, stated-max(1, stated/10))
}

// effectiveLocked is the connection count the semaphore runs at: the target,
// lowered by a learned ceiling and an unknown-limit cap. p.mu must be held.
func (p *pool) effectiveLocked() int {
	n := p.targetLocked()
	if c := p.limit.ceiling; c > 0 {
		n = min(n, c)
	}
	if c := p.limit.cap; c > 0 {
		n = min(n, c)
	}
	return max(n, 1)
}

// limitedLocked reports that the server holds this pool below its target.
func (p *pool) limitedLocked() bool {
	return p.effectiveLocked() < p.targetLocked()
}

// applySize resizes the semaphore to the effective size. resizeMu orders
// concurrent resizes so the last computed size is the one applied.
func (p *pool) applySize() (int, bool) {
	p.resizeMu.Lock()
	defer p.resizeMu.Unlock()
	p.mu.Lock()
	n := p.effectiveLocked()
	p.mu.Unlock()
	return n, p.sem.setSize(n, p.o.PrefetchFraction)
}

// noteClose records that Ether closed n of this pool's sockets.
func (p *pool) noteClose(n int) {
	p.mu.Lock()
	p.noteCloseLocked(n)
	p.mu.Unlock()
}

// noteCloseLocked is noteClose with p.mu held.
func (p *pool) noteCloseLocked(n int) {
	now := time.Now()
	if now.Sub(p.limit.closeWindow) >= p.o.limits.teardownGrace {
		p.limit.closeWindow, p.limit.closes, p.limit.excused = now, 0, 0
	}
	p.limit.closes += n
	p.limit.lastClose = now
}

// teardownLocked reports whether a refusal now is excused as the server
// still tearing down a socket Ether closed, and consumes the excuse.
// p.mu must be held.
func (p *pool) teardownLocked(now time.Time) bool {
	l := &p.limit
	if l.lastClose.IsZero() || now.Sub(l.lastClose) >= p.o.limits.teardownGrace || l.excused >= 2*l.closes {
		return false
	}
	l.excused++
	return true
}

// refused handles a dial the server turned away as over its connection
// limit. live is what the server is carrying for this pool (the refused
// dial excluded). It reports whether the caller should wait for a slot and
// retry rather than count the provider as failing.
func (p *pool) refused(err error, live int) bool {
	t := &p.o.limits
	now := time.Now()
	p.mu.Lock()
	teardown := p.teardownLocked(now)
	if teardown {
		p.limit.dialAfter = later(p.limit.dialAfter, now.Add(t.refusalPause))
	}
	p.mu.Unlock()
	if teardown {
		p.teardownRefusals.Add(1)
		p.log.Debug("usenet: connection refused while the server tears down a closed one; retrying", "err", err)
		return true
	}
	p.limitRefusals.Add(1)
	stated := statedConnLimit(err)
	p.mu.Lock()
	if stated > 0 {
		c := limitCeiling(stated)
		if p.limit.ceiling == 0 || c < p.limit.ceiling {
			p.limit.ceiling = c
			p.limit.stated = stated
		}
	}
	if live > 0 {
		p.limit.cap = live
		p.limit.widenAt = now.Add(t.widen)
	}
	p.limit.dialAfter = later(p.limit.dialAfter, now.Add(t.refusalPause))
	configured := p.targetLocked()
	p.mu.Unlock()
	if n, changed := p.applySize(); changed {
		p.log.Info("usenet: server allows fewer connections than configured; adapting",
			"configured", configured, "using", n, "server_limit", stated)
	}
	return live > 0
}

// widenLocked steps an unknown-limit cap up by one after a success, at most
// once per widen interval. It reports whether the cap moved; the caller then
// applies the size outside p.mu. p.mu must be held.
func (p *pool) widenLocked(now time.Time) bool {
	if p.limit.cap == 0 || now.Before(p.limit.widenAt) {
		return false
	}
	p.limit.cap++
	p.limit.widenAt = now.Add(p.o.limits.widen)
	top := p.targetLocked()
	if c := p.limit.ceiling; c > 0 {
		top = min(top, c)
	}
	if p.limit.cap >= top {
		p.limit.cap = 0
	}
	return true
}

// paceDial waits until a handshake may start on this pool. The circuit
// breaker's half-open probe skips it (see dialNew): once the breaker has
// tripped, its own backoff gates dials, and stacking both would only stretch
// recovery. The breaker alone misses one case this covers: new dials failing
// while command successes on existing connections keep resetting it.
func (p *pool) paceDial(ctx context.Context) error {
	p.mu.Lock()
	wait := time.Until(p.limit.dialAfter)
	p.mu.Unlock()
	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-p.stop:
		return ErrPoolClosed
	}
}

// dialSucceeded clears handshake backoff.
func (p *pool) dialSucceeded() {
	p.mu.Lock()
	p.limit.dialFails = 0
	p.limit.dialAfter = time.Time{}
	p.mu.Unlock()
}

// dialFailed backs off the next handshake exponentially, with jitter.
func (p *pool) dialFailed() {
	t := &p.o.limits
	p.mu.Lock()
	defer p.mu.Unlock()
	p.limit.dialFails++
	d := t.backoffMin
	for i := 1; i < p.limit.dialFails && d < t.backoffMax; i++ {
		d = cappedDouble(d, t.backoffMin, t.backoffMax)
	}
	d += time.Duration(rand.Float64() * float64(d) / 8)
	p.limit.dialAfter = later(p.limit.dialAfter, time.Now().Add(min(d, t.backoffMax)))
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
