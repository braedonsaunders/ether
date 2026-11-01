package usenet

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Provider states reported in statistics.
const (
	StateOK       = "ok"
	StateBackoff  = "backoff"     // connection failures; retrying after a delay
	StateAuthFail = "auth_failed" // disabled; re-probed after a growing backoff or on a settings change
	StateClosed   = "closed"
)

// breaker is a per-provider circuit breaker. Authentication failures open it
// until a single background probe (see pool.authProbe) logs in again; probes
// run after authMin, doubling per failed probe up to authMax, so a provider
// whose auth service has a transient outage heals by itself while bad
// credentials cost one login attempt per backoff. A new pool (created when
// settings change) starts closed.
// Two ladders share one reported backoff. Connect failures (dial, TLS,
// greeting, transport-level handshake) pause new dials only: sockets that
// already authenticated stay usable. Command failures on those sockets
// (BODY 502, resets, timeouts) pause the provider entirely, idle sockets
// included. A successful dial followed by any completed command — STAT, or
// even a 430 — resets the connect ladder, so a health check can dial
// concurrently again. Only a BODY success resets the command ladder, so a
// provider whose STAT works and whose BODY does not stays tripped. The
// first open of either ladder stays at minBackoff (1s). A successful login
// alone does not reset either ladder.
type tripCause uint8

const (
	tripNone tripCause = iota
	tripConnect
	tripCommand
)

type breaker struct {
	mu            sync.Mutex
	now           func() time.Time
	minBackoff    time.Duration
	maxBackoff    time.Duration
	failThreshold int

	authMin, authMax time.Duration

	changed      chan struct{}
	networkProbe bool // at most one dial tests an expired connect backoff
	cause        tripCause

	connectFailures int
	connectUntil    time.Time
	commandFailures int
	commandUntil    time.Time

	authErr   error
	authFails int       // consecutive failed logins (initial failure + probes)
	authUntil time.Time // next auth probe is due
	probing   bool      // an auth probe is in flight
	lastErr   error
	lastAt    time.Time
}

func newBreaker(minB, maxB time.Duration, threshold int, now func() time.Time) *breaker {
	if threshold < 1 {
		threshold = 1
	}
	return &breaker{now: now, minBackoff: minB, maxBackoff: maxB, failThreshold: threshold,
		authMin: DefaultAuthRetryMin, authMax: DefaultAuthRetryMax, changed: make(chan struct{})}
}

// authDisabled reports a credential lockout. That refuses every socket.
func (b *breaker) authDisabled() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.authErr != nil {
		return fmt.Errorf("%w: %v", ErrProviderDisabled, b.authErr)
	}
	return nil
}

// idleAllowed reports whether an already-authenticated socket may be used.
// Command backoff means those sockets are the failing ones. Connect backoff
// does not: only new dials wait.
func (b *breaker) idleAllowed() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.authErr != nil {
		return fmt.Errorf("%w: %v", ErrProviderDisabled, b.authErr)
	}
	if now := b.now(); now.Before(b.commandUntil) {
		return fmt.Errorf("%w for %s: %v", ErrProviderBackoff, b.commandUntil.Sub(now).Round(time.Millisecond), b.lastErr)
	}
	return nil
}

// allow returns nil if a new dial may be attempted now.
func (b *breaker) allow() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.authErr != nil {
		return fmt.Errorf("%w: %v", ErrProviderDisabled, b.authErr)
	}
	if until := b.blockedUntil(); b.now().Before(until) {
		return fmt.Errorf("%w for %s: %v", ErrProviderBackoff, until.Sub(b.now()).Round(time.Millisecond), b.lastErr)
	}
	return nil
}

// admit reserves a single half-open dial after a connect backoff. Command
// failures do not serialize dials once their window has passed: a STAT
// health check must be able to fan out. allow remains a read-only check.
func (b *breaker) admit() (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.authErr != nil {
		return false, fmt.Errorf("%w: %v", ErrProviderDisabled, b.authErr)
	}
	if b.now().Before(b.blockedUntil()) || b.networkProbe {
		return false, ErrProviderBackoff
	}
	if b.connectFailures >= b.failThreshold {
		b.networkProbe = true
		return true, nil
	}
	return false, nil
}

func (b *breaker) releaseProbe() {
	b.mu.Lock()
	b.networkProbe = false
	b.signalLocked()
	b.mu.Unlock()
}

// connectOK resets the connect ladder. A completed command (STAT, including
// 430, or BODY) proves the path that a probe dial just opened. It does not
// forgive command failures on sockets that were already up.
func (b *breaker) connectOK() {
	b.mu.Lock()
	b.connectFailures = 0
	b.connectUntil = time.Time{}
	b.networkProbe = false
	if b.cause == tripConnect {
		b.cause = tripNone
	}
	b.signalLocked()
	b.mu.Unlock()
}

// success resets the command ladder after a BODY that actually returned an
// article. The exchange also proves the provider is reachable, so the
// connect ladder clears with it.
func (b *breaker) success() {
	b.mu.Lock()
	b.commandFailures = 0
	b.commandUntil = time.Time{}
	b.connectFailures = 0
	b.connectUntil = time.Time{}
	b.networkProbe = false
	b.cause = tripNone
	b.signalLocked()
	b.mu.Unlock()
}

// cappedDouble doubles d, clamped to [lo, hi].
func cappedDouble(d, lo, hi time.Duration) time.Duration {
	// lo>hi or d already past the cap collapses to hi; d>hi/2 returns hi before doubling, so d*2 cannot overflow.
	if lo > hi || d > hi/2 {
		return hi
	}
	return min(max(d*2, lo), hi)
}

// connFailure records a failed dial, TLS handshake, greeting, or transport
// setup. Established sockets are not at fault.
func (b *breaker) connFailure(err error) time.Duration {
	return b.open(&b.connectFailures, &b.connectUntil, tripConnect, err)
}

// commandFailure records a failure of a command on a socket that was
// already up: BODY 502, a reset, or a timeout.
func (b *breaker) commandFailure(err error) time.Duration {
	return b.open(&b.commandFailures, &b.commandUntil, tripCommand, err)
}

func (b *breaker) open(failures *int, until *time.Time, cause tripCause, err error) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	*failures++
	b.lastErr, b.lastAt = err, b.now()
	if *failures < b.failThreshold {
		return 0
	}
	d := b.minBackoff
	for i := b.failThreshold; i < *failures && d < b.maxBackoff; i++ {
		d = cappedDouble(d, b.minBackoff, b.maxBackoff)
	}
	d = min(d, b.maxBackoff)
	*until = b.now().Add(d)
	b.cause = cause
	b.signalLocked()
	return d
}

// authFailure disables the provider after a rejected login and returns the
// delay until the next probe. Failures reported while already disabled and
// not probing (concurrent dials racing the first one) do not grow the
// backoff.
func (b *breaker) authFailure(err error) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	b.lastErr, b.lastAt = err, now
	if b.authErr != nil && !b.probing {
		return max(b.authUntil.Sub(now), 0)
	}
	b.probing = false
	b.authErr = err
	b.authFails++
	d := b.authMin
	for i := 1; i < b.authFails && d < b.authMax; i++ {
		d = cappedDouble(d, b.authMin, b.authMax)
	}
	d = min(d, b.authMax)
	b.authUntil = now.Add(d)
	return d
}

// authProbeDue reports whether an auth-disabled provider is due a login
// probe, and if so claims it so only one runs at a time.
func (b *breaker) authProbeDue() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.authErr == nil || b.probing || b.now().Before(b.authUntil) {
		return false
	}
	b.probing = true
	return true
}

// authRecovered re-enables the provider after a successful login probe.
func (b *breaker) authRecovered() {
	b.mu.Lock()
	b.authErr, b.authFails, b.authUntil, b.probing = nil, 0, time.Time{}, false
	b.mu.Unlock()
}

// authProbeInconclusive ends a probe that failed for a reason other than
// the credentials (e.g. the network is down); it is retried after authMin
// without growing the backoff.
func (b *breaker) authProbeInconclusive(err error) {
	b.mu.Lock()
	b.probing = false
	b.authUntil = b.now().Add(b.authMin)
	b.lastErr, b.lastAt = err, b.now()
	b.mu.Unlock()
}

// noteError records a non-tripping error for statistics.
func (b *breaker) noteError(err error) {
	b.mu.Lock()
	b.lastErr, b.lastAt = err, b.now()
	b.mu.Unlock()
}

func (b *breaker) state() (state string, until time.Time, lastErr error, lastAt time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch {
	case b.authErr != nil:
		return StateAuthFail, b.authUntil, b.lastErr, b.lastAt
	case b.now().Before(b.blockedUntil()) || b.connectFailures > 0 || b.commandFailures >= b.failThreshold:
		state = StateBackoff
	default:
		state = StateOK
	}
	return state, b.blockedUntil(), b.lastErr, b.lastAt
}

func (b *breaker) blockedUntil() time.Time {
	until := b.connectUntil
	if b.commandUntil.After(until) {
		until = b.commandUntil
	}
	return until
}

// signalLocked wakes requests whose last usable provider is recovering.
func (b *breaker) signalLocked() {
	close(b.changed)
	b.changed = make(chan struct{})
}

func (b *breaker) recoveryDelay() (time.Duration, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.authErr != nil || ((b.commandFailures >= b.failThreshold || b.connectFailures >= b.failThreshold) && isTimeoutError(b.lastErr)) {
		return 0, false
	}
	return max(0, b.blockedUntil().Sub(b.now())), true
}

// waitReady does not claim a probe. The connection admission path still grants
// exactly one half-open probe; peers sleep again until it completes.
func (b *breaker) waitReady(ctx context.Context) error {
	for {
		b.mu.Lock()
		if b.authErr != nil {
			err := b.authErr
			b.mu.Unlock()
			return fmt.Errorf("%w: %v", ErrProviderDisabled, err)
		}
		wait := b.blockedUntil().Sub(b.now())
		ch := b.changed
		b.mu.Unlock()
		if wait <= 0 {
			return nil
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctxErr(ctx)
		case <-ch:
			t.Stop()
		case <-t.C:
			return nil
		}
	}
}
