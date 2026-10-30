// Package trickplay fences shared thumbnail generation by source lifetime.
package trickplay

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

var ErrCapacity = errors.New("trickplay: holder capacity reached")
var ErrClosed = errors.New("trickplay: owner closed")

// Owner is closed only by the runtime. Borrowed providers expose no Close.
// Backend Ensure and Cancel must return promptly and must not call back into Owner.
type Owner struct {
	mu      sync.Mutex
	backend core.TrickplayProvider
	view    *borrowed
	holders map[string]int
	count   int
	closed  bool
}

func New(backend core.TrickplayProvider) *Owner {
	o := &Owner{backend: backend, holders: make(map[string]int)}
	o.view = &borrowed{TrickplayProvider: backend, owner: o}
	return o
}

type borrowed struct {
	core.TrickplayProvider
	owner *Owner
}

func (o *Owner) Borrow() core.TrickplayProvider {
	if o == nil || o.backend == nil {
		return nil
	}
	return o.view
}
func (p *borrowed) AcquireTrickplay(ctx context.Context, src core.TrickplaySource) (core.TrickplayLease, error) {
	o := p.owner
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return nil, ErrClosed
	}
	if src.ReleaseID == "" || src.InputURL == "" {
		return nil, errors.New("trickplay: missing source")
	}
	if o.count >= 4096 || (o.holders[src.ReleaseID] == 0 && len(o.holders) >= 1024) {
		return nil, ErrCapacity
	}
	o.holders[src.ReleaseID]++
	o.count++
	o.backend.EnsureTrickplay(ctx, src)
	return &lease{owner: o, release: src.ReleaseID}, nil
}

// Unleased Ensure cannot establish a lifetime on a shared provider.
func (p *borrowed) EnsureTrickplay(context.Context, core.TrickplaySource) {}
func (p *borrowed) CancelTrickplay(id string) {
	o := p.owner
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.closed && o.holders[id] == 0 {
		o.cancel(id)
	}
}
func (o *Owner) cancel(id string) {
	if c, ok := o.backend.(core.TrickplayCanceler); ok {
		c.CancelTrickplay(id)
	}
}
func (p *borrowed) PreviewInterval() time.Duration {
	if g, ok := p.TrickplayProvider.(interface{ PreviewInterval() time.Duration }); ok {
		return g.PreviewInterval()
	}
	return 10 * time.Second
}
func (p *borrowed) PreviewWidth() int {
	if g, ok := p.TrickplayProvider.(interface{ PreviewWidth() int }); ok {
		return g.PreviewWidth()
	}
	return 320
}
func (o *Owner) Close() error {
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return nil
	}
	o.closed = true
	o.holders = make(map[string]int)
	o.count = 0
	o.mu.Unlock()
	if c, ok := o.backend.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

type lease struct {
	owner    *Owner
	release  string
	released bool
}

func (l *lease) Ensure(ctx context.Context, src core.TrickplaySource) {
	o := l.owner
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.closed && !l.released && src.ReleaseID == l.release {
		o.backend.EnsureTrickplay(ctx, src)
	}
}
func (l *lease) Release(cancelIfIdle bool) {
	o := l.owner
	o.mu.Lock()
	defer o.mu.Unlock()
	if l.released {
		return
	}
	l.released = true
	if o.closed {
		return
	}
	o.count--
	o.holders[l.release]--
	if o.holders[l.release] == 0 {
		delete(o.holders, l.release)
		if cancelIfIdle {
			o.cancel(l.release)
		}
	}
}

// Holder serializes a consumer's source changes and terminal release. A captured
// holder remains closed even when an in-flight request outlives its session.
// It is safe to publish a pointer under a session-table lock and call Update or
// Close outside that lock. Never copy a Holder after first use.
type Holder struct {
	mu      sync.Mutex
	lease   core.TrickplayLease
	release string
	closed  bool
}

func (h *Holder) Update(ctx context.Context, p core.TrickplayProvider, src core.TrickplaySource) error {
	return h.UpdateIf(ctx, p, src, nil)
}

// UpdateIf revalidates the live source after taking the holder barrier.
func (h *Holder) UpdateIf(ctx context.Context, p core.TrickplayProvider, src core.TrickplaySource, valid func() bool) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || (valid != nil && !valid()) {
		return nil
	}
	leaser, ok := p.(core.TrickplayLeaser)
	if !ok {
		return errors.New("trickplay: holder requires a leased provider")
	}
	if h.lease != nil && h.release == src.ReleaseID {
		h.lease.Ensure(ctx, src)
		if valid != nil && !valid() {
			h.lease.Release(true)
			h.lease = nil
			h.release = ""
		}
		return nil
	}
	next, err := leaser.AcquireTrickplay(ctx, src)
	if err != nil {
		return err
	}
	if valid != nil && !valid() {
		next.Release(true)
		return nil
	}
	old := h.lease
	h.lease = next
	h.release = src.ReleaseID
	if old != nil {
		old.Release(true)
	}
	return nil
}
func (h *Holder) Close(cancelIfIdle bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	if h.lease != nil {
		h.lease.Release(cancelIfIdle)
		h.lease = nil
	}
}
