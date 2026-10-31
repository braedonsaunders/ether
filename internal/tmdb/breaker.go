package tmdb

import (
	"errors"
	"sync"
	"time"
)

// ErrUnavailable is returned without contacting TMDB while the circuit
// breaker is open: recent requests failed at the transport or with
// exhausted 429/5xx retries.
var ErrUnavailable = errors.New("tmdb: temporarily unreachable")

// After breakerThreshold consecutive failed requests (each already retried)
// the client fails fast for breakerCooldown, then lets one request probe.
// Without it an outage costs every caller the full retry ladder per title,
// and a page that touches many titles waits for all of them.
const (
	breakerThreshold = 3
	breakerCooldown  = 30 * time.Second
)

type breaker struct {
	mu        sync.Mutex
	failures  int
	openUntil time.Time
}

func (b *breaker) open(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return now.Before(b.openUntil)
}

func (b *breaker) success() {
	b.mu.Lock()
	b.failures, b.openUntil = 0, time.Time{}
	b.mu.Unlock()
}

func (b *breaker) failure(now time.Time) {
	b.mu.Lock()
	b.failures++
	if b.failures >= breakerThreshold {
		b.openUntil = now.Add(breakerCooldown)
	}
	b.mu.Unlock()
}
