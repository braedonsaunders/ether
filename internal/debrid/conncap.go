package debrid

import (
	"context"
	"log/slog"
	"net"
	"sync"
	"time"
)

// defaultHostConnections caps open TCP connections per media host across
// the primary and hedge transports. Debrid CDNs count connections, not
// requests: TorBox answers the 21st connection to one host with HTTP 429
// and a five-minute pause for the whole host. Idle keep-alive connections
// count too, so two transports with generous idle pools could trip that
// limit while only a handful of reads were in flight. Twelve leaves room
// for the owner's other devices and apps on the same account.
const defaultHostConnections = 12

// After a host resets connections or answers 429, new dials to it are held
// to half the cap (never below minReducedConnections) for resetCooldown.
const (
	minReducedConnections = 2
	resetCooldown         = 3 * time.Minute
)

// connCap counts open connections per dial address and makes a dial wait,
// after asking the transports to drop idle connections, rather than exceed
// the limit.
type connCap struct {
	limit     int
	closeIdle func()
	log       *slog.Logger

	now func() time.Time

	mu      sync.Mutex
	open    map[string]int
	reduced map[string]time.Time // addr -> end of its reduced-cap cooldown
	freed   chan struct{}        // closed and replaced whenever a connection closes
}

func newConnCap(limit int) *connCap {
	return &connCap{limit: limit, now: time.Now, open: map[string]int{}, reduced: map[string]time.Time{}, freed: make(chan struct{})}
}

// limitLocked is addr's effective cap and, while it is reduced, when the
// reduction ends.
func (c *connCap) limitLocked(addr string) (int, time.Time) {
	until, ok := c.reduced[addr]
	if !ok {
		return c.limit, time.Time{}
	}
	if !c.now().Before(until) {
		delete(c.reduced, addr)
		if c.log != nil {
			c.log.Info("media host connection cap restored", "host", addr, "limit", c.limit)
		}
		return c.limit, time.Time{}
	}
	return min(c.limit, max(minReducedConnections, c.limit/2)), until
}

// reduce lowers addr's cap for resetCooldown; further resets extend it.
// Open connections are left alone; new dials wait under the lower cap.
func (c *connCap) reduce(addr string) {
	if addr == "" {
		return
	}
	c.mu.Lock()
	_, already := c.reduced[addr]
	c.reduced[addr] = c.now().Add(resetCooldown)
	lim, _ := c.limitLocked(addr)
	c.mu.Unlock()
	if !already && c.log != nil {
		c.log.Warn("media host connection cap reduced", "host", addr, "limit", lim, "cooldown", resetCooldown)
	}
}

type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

func (c *connCap) wrap(next dialFunc) dialFunc {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if err := c.acquire(ctx, addr); err != nil {
			return nil, err
		}
		conn, err := next(ctx, network, addr)
		if err != nil {
			c.release(addr)
			return nil, err
		}
		return &cappedConn{Conn: conn, release: func() { c.release(addr) }}, nil
	}
}

func (c *connCap) acquire(ctx context.Context, addr string) error {
	asked := false
	began := time.Now()
	lim := c.limit
	defer func() {
		if wait := time.Since(began); wait > 250*time.Millisecond && c.log != nil {
			c.log.Info("media connection waited for the host cap", "host", addr, "wait", wait, "limit", lim)
		}
	}()
	for {
		c.mu.Lock()
		var until time.Time
		lim, until = c.limitLocked(addr)
		if c.open[addr] < lim {
			c.open[addr]++
			c.mu.Unlock()
			return nil
		}
		freed := c.freed
		c.mu.Unlock()
		if !asked && c.closeIdle != nil {
			// Idle keep-alives are the cheapest connections to give back.
			asked = true
			go c.closeIdle()
		}
		var restored <-chan time.Time
		var t *time.Timer
		if !until.IsZero() {
			// The cap rises when the cooldown ends, even if nothing closes.
			t = time.NewTimer(until.Sub(c.now()))
			restored = t.C
		}
		select {
		case <-freed:
		case <-restored:
		case <-ctx.Done():
			if t != nil {
				t.Stop()
			}
			return ctx.Err()
		}
		if t != nil {
			t.Stop()
		}
	}
}

func (c *connCap) release(addr string) {
	c.mu.Lock()
	if c.open[addr] <= 1 {
		delete(c.open, addr)
	} else {
		c.open[addr]--
	}
	close(c.freed)
	c.freed = make(chan struct{})
	c.mu.Unlock()
}

// inUse reports open connections to addr (tests and diagnostics).
func (c *connCap) inUse(addr string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.open[addr]
}

type cappedConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *cappedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}
