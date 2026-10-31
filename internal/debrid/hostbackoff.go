package debrid

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Per-host backoff after connection failures. A busy debrid edge node that
// keeps failing to connect or handshake gets fewer, spaced attempts instead
// of a fresh burst from every player read; other hosts are unaffected.
const (
	hostBackoffBase = 500 * time.Millisecond
	hostBackoffMax  = 30 * time.Second
)

type hostBackoff struct {
	base, max time.Duration
	now       func() time.Time
	log       *slog.Logger

	mu    sync.Mutex
	hosts map[string]*hostBackoffState
}

type hostBackoffState struct {
	failures int
	until    time.Time
}

func newHostBackoff(log *slog.Logger) *hostBackoff {
	return &hostBackoff{base: hostBackoffBase, max: hostBackoffMax, now: time.Now, log: log, hosts: map[string]*hostBackoffState{}}
}

// delay is min(max, base * 2^(n-1)) for n consecutive failures.
func (b *hostBackoff) delay(n int) time.Duration {
	if n <= 0 || b.base <= 0 {
		return 0
	}
	d := b.base
	for i := 1; i < n && d < b.max; i++ {
		d *= 2
	}
	return min(d, b.max)
}

// wait blocks until host's backoff has passed or ctx ends.
func (b *hostBackoff) wait(ctx context.Context, host string) error {
	b.mu.Lock()
	st := b.hosts[host]
	var d time.Duration
	if st != nil {
		d = st.until.Sub(b.now())
	}
	b.mu.Unlock()
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// failure records one connect/TLS/transport failure to host.
func (b *hostBackoff) failure(host string) {
	b.mu.Lock()
	st := b.hosts[host]
	if st == nil {
		st = &hostBackoffState{}
		b.hosts[host] = st
	}
	st.failures++
	d := b.delay(st.failures)
	st.until = b.now().Add(d)
	first := st.failures == 1
	b.mu.Unlock()
	if first && b.log != nil {
		b.log.Warn("debrid host backing off", "host", host, "wait", d)
	}
}

// success clears host's backoff.
func (b *hostBackoff) success(host string) {
	b.mu.Lock()
	st := b.hosts[host]
	delete(b.hosts, host)
	b.mu.Unlock()
	if st != nil && b.log != nil {
		b.log.Info("debrid host recovered", "host", host, "failures", st.failures)
	}
}

// failureCount reports host's consecutive failures (tests and diagnostics).
func (b *hostBackoff) failureCount(host string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if st := b.hosts[host]; st != nil {
		return st.failures
	}
	return 0
}

// isConnReset reports a connection the peer reset.
func isConnReset(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.ECONNRESET) {
		return true
	}
	// Windows reports WSAECONNRESET, which syscall.ECONNRESET does not match.
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "connection reset") || strings.Contains(msg, "forcibly closed")
}

// dialAddr is the host:port the transport dials for a URL (no proxy), the
// key connCap counts under.
func dialAddr(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	port := u.Port()
	if port == "" {
		port = "80"
		if strings.EqualFold(u.Scheme, "https") {
			port = "443"
		}
	}
	return net.JoinHostPort(u.Hostname(), port)
}
