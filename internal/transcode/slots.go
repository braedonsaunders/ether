package transcode

import (
	"context"
	"errors"
	"sync"
	"time"
)

// SlotPriority separates interactive viewing from cancellable render jobs.
type SlotPriority uint8

const (
	SlotViewer SlotPriority = iota
	SlotBackground
	// SlotLivePrep is a single reserved lane for imminent channel masters.
	// It cannot be preempted by a viewer or consume the viewer encoder budget.
	SlotLivePrep
)

type transcodeSlot struct {
	priority SlotPriority
	cancel   context.CancelFunc
	released bool
}

// AcquireTranscodeSlot reserves the shared encoder budget. Run the job under
// jobCtx, and always release after its process has stopped. Viewers preempt the
// newest background holder; preemption cancels jobCtx and reclaims its charge
// immediately. Background callers wait behind every queued viewer.
func (m *Manager) AcquireTranscodeSlot(ctx context.Context, p SlotPriority) (context.Context, func(), error) {
	return m.acquirePrioritySlot(ctx, p, true)
}

func (m *Manager) acquirePrioritySlot(ctx context.Context, p SlotPriority, wait bool) (jobCtx context.Context, release func(), err error) {
	if p != SlotViewer && p != SlotBackground && p != SlotLivePrep {
		return nil, nil, errors.New("transcode: invalid slot priority")
	}
	m.mu.Lock()
	if m.slotChanged == nil {
		m.slotChanged = make(chan struct{})
	}
	if p == SlotViewer {
		m.slotViewers++
	}
	defer func() {
		if p == SlotViewer {
			m.slotViewers--
			m.notifySlotsLocked()
		}
		m.mu.Unlock()
	}()
	// Legacy encoder lifetimes update their encoding state independently. The
	// bounded fallback poll observes those transitions without coupling locks.
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if m.closed {
			return nil, nil, errors.New("transcode: manager closed")
		}
		full := m.cfg.MaxTranscodes > 0 && m.encodingLocked("") >= m.cfg.MaxTranscodes
		if p == SlotLivePrep {
			full = m.livePrepEncodes >= 1
		}
		if full && p == SlotViewer {
			m.preemptBackgroundLocked()
			full = m.cfg.MaxTranscodes > 0 && m.encodingLocked("") >= m.cfg.MaxTranscodes
		}
		if !full && (p == SlotViewer || p == SlotLivePrep || m.slotViewers == 0) {
			jobCtx = ctx
			var cancel context.CancelFunc
			if p == SlotBackground || p == SlotLivePrep {
				jobCtx, cancel = context.WithCancel(ctx)
			}
			holder := &transcodeSlot{priority: p, cancel: cancel}
			m.slotHolders = append(m.slotHolders, holder)
			if p == SlotLivePrep {
				m.livePrepEncodes++
			} else {
				m.downloadEncodes++
			}
			var once sync.Once
			release = func() { once.Do(func() { m.mu.Lock(); m.releaseSlotLocked(holder); m.mu.Unlock() }) }
			return jobCtx, release, nil
		}
		if !wait {
			return nil, nil, ErrTooManyTranscodes
		}
		changed := m.slotChanged
		m.mu.Unlock()
		select {
		case <-ctx.Done():
		case <-m.stop:
		case <-changed:
		case <-ticker.C:
		}
		m.mu.Lock()
	}
}
func (m *Manager) notifySlotsLocked() {
	if m.slotChanged != nil {
		close(m.slotChanged)
		m.slotChanged = make(chan struct{})
	}
}
func (m *Manager) releaseSlotLocked(h *transcodeSlot) {
	if h.released {
		return
	}
	h.released = true
	if h.cancel != nil {
		h.cancel()
	}
	if h.priority == SlotLivePrep {
		m.livePrepEncodes--
	} else {
		m.downloadEncodes--
	}
	for i, v := range m.slotHolders {
		if v == h {
			m.slotHolders = append(m.slotHolders[:i], m.slotHolders[i+1:]...)
			break
		}
	}
	m.notifySlotsLocked()
}
func (m *Manager) preemptBackgroundLocked() bool {
	for i := len(m.slotHolders) - 1; i >= 0; i-- {
		h := m.slotHolders[i]
		if h.priority == SlotBackground && !h.released {
			m.releaseSlotLocked(h)
			return true
		}
	}
	return false
}
