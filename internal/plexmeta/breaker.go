package plexmeta

import (
	"sync"
	"time"
)

// breaker opens after consecutive provider errors or a 429 and stays open
// for a cooldown (or Retry-After). Request paths never consult it.
type breaker struct {
	mu        sync.Mutex
	failures  int
	threshold int
	cooldown  time.Duration
	openUntil time.Time
	now       func() time.Time
}

func (b *breaker) Open() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.now().Before(b.openUntil)
}

func (b *breaker) Success() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures = 0
}

func (b *breaker) Failure(retryAfter time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures++
	wait := b.cooldown
	if retryAfter > wait {
		wait = retryAfter
	}
	if retryAfter > 0 || b.failures >= b.threshold {
		b.openUntil = b.now().Add(wait)
		if retryAfter > 0 {
			b.failures = 0
		}
	}
}
