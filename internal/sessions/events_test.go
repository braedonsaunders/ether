package sessions

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/clusterbus"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

func TestEventsAcrossNodesAndViewerScope(t *testing.T) {
	hub := clusterbus.NewHub()
	active := func(_ context.Context, id int64) (*core.Viewer, error) {
		return &core.Viewer{ID: id, IsOwner: id == 1}, nil
	}
	a := NewEventBus(hub.Node("a"), active)
	defer a.Close()
	b := NewEventBus(hub.Node("b"), active)
	defer b.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	viewer, err := b.Subscribe(ctx, frontend.EventScope{Viewer: &core.Viewer{ID: 2}})
	if err != nil {
		t.Fatal(err)
	}
	other, err := b.Subscribe(ctx, frontend.EventScope{Viewer: &core.Viewer{ID: 3}})
	if err != nil {
		t.Fatal(err)
	}
	owner, err := b.Subscribe(ctx, frontend.EventScope{Viewer: &core.Viewer{ID: 1}, Owner: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Subscribe(ctx, frontend.EventScope{Viewer: &core.Viewer{ID: 2, IsOwner: true}, Owner: true}); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("owner access: %v", err)
	}
	event := frontend.Event{Type: "UserDataChanged", ViewerID: 2, Item: 123}
	if err := a.Publish(ctx, event); err != nil {
		t.Fatal(err)
	}
	for _, ch := range []<-chan frontend.Event{viewer, owner} {
		select {
		case got := <-ch:
			if got != event {
				t.Fatalf("event = %+v", got)
			}
		case <-time.After(time.Second):
			t.Fatal("missing event")
		}
	}
	select {
	case e := <-other:
		t.Fatalf("event leaked to another viewer: %+v", e)
	case <-time.After(25 * time.Millisecond):
	}
	cancel()
	for _, ch := range []<-chan frontend.Event{viewer, other, owner} {
		select {
		case _, ok := <-ch:
			if ok {
				t.Fatal("channel still open")
			}
		case <-time.After(time.Second):
			t.Fatal("channel not closed")
		}
	}
}

func TestEventsStopAfterViewerRevocation(t *testing.T) {
	var active atomic.Bool
	active.Store(true)
	b := NewEventBus(nil, func(_ context.Context, id int64) (*core.Viewer, error) {
		if !active.Load() {
			return nil, core.ErrUnauthorized
		}
		return &core.Viewer{ID: id}, nil
	})
	defer b.Close()
	ch, err := b.Subscribe(context.Background(), frontend.EventScope{Viewer: &core.Viewer{ID: 7}})
	if err != nil {
		t.Fatal(err)
	}
	active.Store(false)
	if err := b.Publish(context.Background(), frontend.Event{ViewerID: 7}); err != nil {
		t.Fatal(err)
	}
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("revoked viewer received event")
		}
	case <-time.After(time.Second):
		t.Fatal("revoked subscriber did not close")
	}
}

func TestEventsCloseUnblocksSlowSubscriber(t *testing.T) {
	b := NewEventBus(nil, func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil })
	ch, err := b.Subscribe(context.Background(), frontend.EventScope{Viewer: &core.Viewer{ID: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Publish(context.Background(), frontend.Event{ViewerID: 1}); err != nil {
		t.Fatal(err)
	}
	b.Close()
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("close did not release subscriber")
	}
}
