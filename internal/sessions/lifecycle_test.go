package sessions

import (
	"context"
	"errors"
	"testing"
)

func TestLifecycleAcquisitionCancellation(t *testing.T) {
	l := newPlaybackLifecycle()
	unlock, err := l.acquire(context.Background(), "one")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = l.acquire(ctx, "one"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	// An independent owner never waits on this gate.
	other, err := l.acquire(context.Background(), "other")
	if err != nil {
		t.Fatal(err)
	}
	other()
	unlock()
	unlock()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.gates) != 0 {
		t.Fatalf("gates leaked: %d", len(l.gates))
	}
}
