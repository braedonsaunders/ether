package livetv

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/transcode"
)

// A registry is node-local. Personal channels include viewerID in their key;
// ordinary channels use zero, so all viewers share the same input clock.
type broadcastKey struct {
	channelID string
	viewerID  int64
}
type broadcastRegistry struct {
	mu       sync.Mutex
	outputs  map[broadcastKey]*broadcast
	closed   bool
	yielding atomic.Int32
}
type broadcastLease struct {
	mu        sync.Mutex
	closed    bool
	height    int
	registry  *broadcastRegistry
	broadcast *broadcast
	rendition string
	once      sync.Once
}

func (r *broadcastRegistry) acquire(ctx context.Context, key broadcastKey, id string, decision core.Decision, create func() (*broadcast, error)) (*broadcastLease, error) {
	// Keep the requested channel warm while yielding other grace-period
	// encoders. A same-channel reopen must retain its encoded live window.
	r.mu.Lock()
	existing := r.outputs[key]
	r.mu.Unlock()
	r.yieldIdle(existing)
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, core.ErrNotFound
	}
	if r.outputs == nil {
		r.outputs = map[broadcastKey]*broadcast{}
	}
	b := r.outputs[key]
	if b != nil {
		b.mu.Lock()
		closed := b.closed
		b.mu.Unlock()
		if closed {
			delete(r.outputs, key)
			b = nil
		}
	}
	running := b != nil
	if b == nil {
		var err error
		b, err = create()
		if err != nil {
			r.mu.Unlock()
			return nil, err
		}
		b.yieldIdle = r.yieldIdle
		r.outputs[key] = b
		b.start()
	}
	r.mu.Unlock()
	if _, err := b.addRendition(ctx, id, decision); err != nil {
		return nil, err
	}
	b.logger.Debug("broadcast viewer attached", "running", running, "rendition", id)
	return &broadcastLease{registry: r, broadcast: b, rendition: id, height: decision.MaxHeight}, nil
}
func (l *broadcastLease) output() (transcode.SegmentIndex, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil, core.ErrNotFound
	}
	b := l.broadcast
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, core.ErrNotFound
	}
	if r := b.renditions[l.rendition]; r != nil {
		return r.encoder.Segments(), nil
	}
	var fallback *broadcastRendition
	for _, r := range b.renditions {
		if l.height > 0 && r.height > l.height {
			continue
		}
		if fallback == nil || r.height < fallback.height {
			fallback = r
		}
	}
	if fallback == nil {
		return nil, core.ErrNotAvailable
	}
	fallback.viewers++
	l.rendition = fallback.id
	return fallback.encoder.Segments(), nil
}
func (l *broadcastLease) close() {
	if l != nil {
		l.once.Do(func() { l.mu.Lock(); l.closed = true; l.broadcast.releaseRendition(l.rendition); l.mu.Unlock() })
	}
}
func (r *broadcastRegistry) close() {
	r.mu.Lock()
	r.closed = true
	outputs := r.outputs
	r.outputs = nil
	r.mu.Unlock()
	for _, b := range outputs {
		b.close()
	}
}
func (r *broadcastRegistry) expire(now time.Time) {
	r.mu.Lock()
	var idle []*broadcast
	for key, b := range r.outputs {
		if b.idle(now) {
			delete(r.outputs, key)
			idle = append(idle, b)
		}
	}
	r.mu.Unlock()
	for _, b := range idle {
		b.close()
	}
}

func (r *broadcastRegistry) invalidate(channelID string, all bool) {
	r.mu.Lock()
	var stale []*broadcast
	for key, b := range r.outputs {
		if all || key.channelID == channelID {
			delete(r.outputs, key)
			stale = append(stale, b)
		}
	}
	r.mu.Unlock()
	for _, b := range stale {
		b.close()
	}
}

// timelineOrigin is the timestamp of the first finished segment, retained for
// the rendition's lifetime even after that segment leaves the DVR window.
// Seq*Duration cannot recover this with variable opening GOPs.
func (l *broadcastLease) timelineOrigin(ctx context.Context) (time.Time, error) {
	l.mu.Lock()
	id := l.rendition
	closed := l.closed
	l.mu.Unlock()
	if closed {
		return time.Time{}, core.ErrNotFound
	}
	b := l.broadcast
	b.mu.Lock()
	r := b.renditions[id]
	if r == nil {
		b.mu.Unlock()
		return time.Time{}, core.ErrNotAvailable
	}
	done := r.originDone
	b.mu.Unlock()
	if done == nil {
		return time.Time{}, core.ErrNotAvailable
	}
	select {
	case <-ctx.Done():
		return time.Time{}, ctx.Err()
	case <-done:
	}
	b.mu.Lock()
	origin, err := r.origin, r.originErr
	b.mu.Unlock()
	return origin, err
}

// yieldIdle closes every output nobody is watching except the caller, so the
// stream slots they hold during their idle grace go to a channel someone is
// tuning. It reports whether slots are being freed, including closes still in
// flight from an earlier call, so the caller retries soon rather than in 15 s.
func (r *broadcastRegistry) yieldIdle(except *broadcast) bool {
	now := time.Now()
	r.mu.Lock()
	var idle []*broadcast
	for key, b := range r.outputs {
		if b != except && b.unwatched(now) {
			delete(r.outputs, key)
			idle = append(idle, b)
		}
	}
	r.mu.Unlock()
	for _, b := range idle {
		r.yielding.Add(1)
		b.logger.Debug("broadcast yielded its stream to a tuned channel")
		// close waits for the output's goroutines; never block the caller's
		// frame clock on it.
		go func() { defer r.yielding.Add(-1); b.close() }()
	}
	return len(idle) > 0 || r.yielding.Load() > 0
}
