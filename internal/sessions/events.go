package sessions

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/braedonsaunders/ether/internal/clusterbus"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

const eventTopic = "sessions:event"

// EventBus distributes viewer-scoped events locally and across cluster nodes.
// ActiveViewer must return the current active viewer, not a cached credential.
type EventBus struct {
	bus          clusterbus.Bus
	activeViewer func(context.Context, int64) (*core.Viewer, error)
	origin       string
	mu           sync.Mutex
	subs         map[*eventSubscriber]struct{}
	unsubscribe  func()
	closed       bool
}

type eventSubscriber struct {
	viewer  int64
	owner   bool
	pending chan frontend.Event
	done    chan struct{}
}

// NewEventBus subscribes to cluster events when bus is non-nil. Call Close
// when the application shuts down.
func NewEventBus(bus clusterbus.Bus, activeViewer func(context.Context, int64) (*core.Viewer, error)) *EventBus {
	b := &EventBus{bus: bus, activeViewer: activeViewer, origin: clusterbus.NewOrigin(), subs: make(map[*eventSubscriber]struct{})}
	if bus != nil {
		b.unsubscribe = bus.Subscribe(eventTopic, b.receive)
	}
	return b
}

func (b *EventBus) authorized(ctx context.Context, id int64, owner bool) bool {
	if id <= 0 || b.activeViewer == nil {
		return false
	}
	v, err := b.activeViewer(ctx, id)
	return err == nil && v != nil && v.ID == id && (!owner || (v.IsOwner && !v.Delegated))
}

// Subscribe ends on cancellation, revoked access, bus shutdown or a slow
// subscriber overflowing its buffer. A closed channel tells clients to resync.
func (b *EventBus) Subscribe(ctx context.Context, scope frontend.EventScope) (<-chan frontend.Event, error) {
	if scope.Viewer == nil || !b.authorized(ctx, scope.Viewer.ID, scope.Owner) {
		return nil, core.ErrUnauthorized
	}
	s := &eventSubscriber{viewer: scope.Viewer.ID, owner: scope.Owner, pending: make(chan frontend.Event, 64), done: make(chan struct{})}
	out := make(chan frontend.Event)
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		close(out)
		return out, nil
	}
	b.subs[s] = struct{}{}
	b.mu.Unlock()
	go func() {
		defer close(out)
		defer b.remove(s)
		for {
			select {
			case <-ctx.Done():
				return
			case <-s.done:
				return
			case event, ok := <-s.pending:
				if !ok || !b.authorized(ctx, s.viewer, s.owner) {
					return
				}
				select {
				case out <- event:
				case <-ctx.Done():
					return
				case <-s.done:
					return
				}
			}
		}
	}()
	return out, nil
}

func (b *EventBus) remove(s *eventSubscriber) {
	b.mu.Lock()
	if _, ok := b.subs[s]; ok {
		delete(b.subs, s)
		close(s.done)
	}
	b.mu.Unlock()
}

func (b *EventBus) deliver(event frontend.Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for s := range b.subs {
		if !s.owner && s.viewer != event.ViewerID {
			continue
		}
		select {
		case s.pending <- event:
		default:
			delete(b.subs, s)
			close(s.done)
		}
	}
}

// Publish delivers locally and sends an origin-tagged event to other nodes.
func (b *EventBus) Publish(ctx context.Context, event frontend.Event) error {
	b.deliver(event)
	if b.bus == nil {
		return nil
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}
	return b.bus.Publish(ctx, eventTopic, clusterbus.Tag(b.origin, payload))
}
func (b *EventBus) receive(msg []byte) {
	origin, payload := clusterbus.Untag(msg)
	if origin == b.origin {
		return
	}
	var event frontend.Event
	if json.Unmarshal(payload, &event) == nil {
		b.deliver(event)
	}
}
func (b *EventBus) Close() {
	if b.unsubscribe != nil {
		b.unsubscribe()
	}
	b.mu.Lock()
	if !b.closed {
		b.closed = true
		for s := range b.subs {
			delete(b.subs, s)
			close(s.done)
		}
	}
	b.mu.Unlock()
}
