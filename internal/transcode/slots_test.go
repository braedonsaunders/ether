package transcode

import (
	"context"
	"errors"
	"github.com/braedonsaunders/ether/internal/config"
	"testing"
	"time"
)

// Priority, newest-holder preemption, exact accounting, and viewer precedence
// are one shared admission invariant (including late releases after preemption).
func TestSlotPriority(t *testing.T) {
	m := newTestManager(t, newFakeRunner(), config.Transcode{MaxTranscodes: 2})
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	old, oldRelease, err := m.AcquireTranscodeSlot(ctx, SlotBackground)
	if err != nil {
		t.Fatal(err)
	}
	defer oldRelease()
	newest, newRelease, err := m.AcquireTranscodeSlot(ctx, SlotBackground)
	if err != nil {
		t.Fatal(err)
	}
	defer newRelease()
	viewer, viewerRelease, err := m.AcquireTranscodeSlot(ctx, SlotViewer)
	if err != nil {
		t.Fatal(err)
	}
	defer viewerRelease()
	if !errors.Is(newest.Err(), context.Canceled) || old.Err() != nil || viewer.Err() != nil {
		t.Fatal("viewer did not preempt only newest background holder")
	}
	newRelease()
	newRelease()
	m.mu.Lock()
	count := m.encodingLocked("")
	m.mu.Unlock()
	if count != 2 {
		t.Fatalf("late release changed budget: %d", count)
	}
	// Fill with viewers, then queue a viewer and background behind them.
	_, otherRelease, err := m.AcquireTranscodeSlot(ctx, SlotViewer)
	if err != nil {
		t.Fatal(err)
	}
	defer otherRelease()
	if old.Err() != context.Canceled {
		t.Fatal("second viewer did not preempt older background")
	}
	viewerAcquired := make(chan func(), 1)
	go func() {
		_, r, e := m.AcquireTranscodeSlot(ctx, SlotViewer)
		if e == nil {
			viewerAcquired <- r
		}
	}()
	for {
		m.mu.Lock()
		waiting := m.slotViewers
		m.mu.Unlock()
		if waiting > 0 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		default:
			time.Sleep(time.Millisecond)
		}
	}
	backgroundAcquired := make(chan func(), 1)
	go func() {
		_, r, e := m.AcquireTranscodeSlot(ctx, SlotBackground)
		if e == nil {
			backgroundAcquired <- r
		}
	}()
	viewerRelease()
	var queuedRelease func()
	select {
	case queuedRelease = <-viewerAcquired:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case release := <-backgroundAcquired:
		release()
		t.Fatal("background overtook waiting viewer")
	default:
	}
	queuedRelease()
	select {
	case release := <-backgroundAcquired:
		release()
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

// Imminent masters retain one bounded lane even while every continuous output
// slot is occupied. Ordinary work still waits and viewing never preempts prep.
func TestLivePreparationLaneUnderContinuousLoad(t *testing.T) {
	m := newTestManager(t, newFakeRunner(), config.Transcode{MaxTranscodes: 1})
	ctx := t.Context()
	_, viewerRelease, err := m.AcquireTranscodeSlot(ctx, SlotViewer)
	if err != nil {
		t.Fatal(err)
	}
	defer viewerRelease()
	short, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if _, release, err := m.AcquireTranscodeSlot(short, SlotBackground); err == nil {
		release()
		t.Fatal("ordinary work bypassed continuous budget")
	}
	prep, release, err := m.AcquireTranscodeSlot(ctx, SlotLivePrep)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	short2, cancel2 := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel2()
	if _, r, err := m.AcquireTranscodeSlot(short2, SlotLivePrep); err == nil {
		r()
		t.Fatal("live prep exceeded one job")
	}
	m.mu.Lock()
	viewerCount, prepCount := m.encodingLocked(""), m.livePrepEncodes
	m.preemptBackgroundLocked()
	m.mu.Unlock()
	if prep.Err() != nil || viewerCount != 1 || prepCount != 1 {
		t.Fatalf("prep was preempted or charged to viewers: %v %d %d", prep.Err(), viewerCount, prepCount)
	}
	release()
	release()
	_, again, err := m.AcquireTranscodeSlot(ctx, SlotLivePrep)
	if err != nil {
		t.Fatal(err)
	}
	again()
}
