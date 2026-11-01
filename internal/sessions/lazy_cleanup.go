package sessions

import (
	"context"
	"fmt"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
)

const maxLazyCleanupReservations = 128

type lazyCleanupEntry struct {
	session  *brokerSession
	viewerID int64
	deviceID string
	protocol frontend.Protocol
	clientID string
}

// Reserve rollback capacity before a session can be opened. A queued retry
// owns its reservation until it has retired or shutdown takes it over.
func (b *Broker) reserveLazyCleanup() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || b.lazyCleanupReserved >= maxLazyCleanupReservations {
		return false
	}
	b.lazyCleanupReserved++
	return true
}
func (b *Broker) releaseLazyCleanup() {
	b.mu.Lock()
	b.lazyCleanupReserved--
	b.mu.Unlock()
}

func (b *Broker) attemptLazyCleanup(ctx context.Context, entry lazyCleanupEntry) bool {
	unlock, err := b.gates.Acquire(ctx, fmt.Sprintf("broker/owner/%d/%s", entry.viewerID, entry.clientID))
	if err != nil {
		return false
	}
	defer unlock()
	b.mu.Lock()
	current := b.sessions[entry.session.public.ID]
	matches := current == entry.session && current.public.ViewerID == entry.viewerID && current.public.Device.ID == entry.deviceID && current.public.Protocol == entry.protocol
	b.mu.Unlock()
	if matches {
		if err := b.stopLocked(ctx, current); err != nil && b.deps.OnEventError != nil {
			b.deps.OnEventError(err)
		}
	}
	return true
}

// queueLazyCleanup transfers the caller's reservation to one broker-owned
// worker. The queue cannot overflow: every entry already reserved capacity.
func (b *Broker) queueLazyCleanup(id string, entry lazyCleanupEntry) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return false
	}
	if b.lazyCleanup == nil {
		b.lazyCleanup = make(map[string]lazyCleanupEntry)
	}
	if _, exists := b.lazyCleanup[id]; exists {
		return false
	}
	b.lazyCleanup[id] = entry
	if !b.lazyCleanupRunning {
		ctx, cancel := context.WithCancel(context.Background())
		b.lazyCleanupCancel = cancel
		b.lazyCleanupDone = make(chan struct{})
		b.lazyCleanupRunning = true
		go b.runLazyCleanup(ctx, b.lazyCleanupDone)
	}
	return true
}

func (b *Broker) runLazyCleanup(ctx context.Context, done chan struct{}) {
	defer close(done)
	last := ""
	for {
		b.mu.Lock()
		if ctx.Err() != nil || len(b.lazyCleanup) == 0 {
			b.lazyCleanupReserved -= len(b.lazyCleanup)
			b.lazyCleanup = nil
			b.lazyCleanupRunning = false
			b.lazyCleanupCancel = nil
			b.mu.Unlock()
			return
		}
		var id, first string
		for key := range b.lazyCleanup {
			if first == "" || key < first {
				first = key
			}
			if key > last && (id == "" || key < id) {
				id = key
			}
		}
		if id == "" {
			id = first
		}
		last = id
		entry := b.lazyCleanup[id]
		b.mu.Unlock()
		attempt, cancel := context.WithTimeout(ctx, b.lazyCleanupWait)
		finished := b.attemptLazyCleanup(attempt, entry)
		cancel()
		if finished {
			b.mu.Lock()
			if _, ok := b.lazyCleanup[id]; ok {
				delete(b.lazyCleanup, id)
				b.lazyCleanupReserved--
			}
			b.mu.Unlock()
			continue
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-timer.C:
		}
	}
}
