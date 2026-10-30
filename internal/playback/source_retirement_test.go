package playback

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func newOwnedHarness(t *testing.T, releases []*fakeRelease, ids []string) *harness {
	t.Helper()
	db := store.OpenTest(t)
	return newHarness(t, releases, ids, func(h *harness, _ *Options) { h.db = db })
}

type ownedTestEngine struct {
	core.Engine
	openEntered, openRelease   chan struct{}
	closeEntered, closeRelease chan struct{}
	closeErr, openErr          error
	hookRelease                string
	once                       sync.Once
	closes                     atomic.Int32
}

func (e *ownedTestEngine) Open(ctx context.Context, id string, nzb []byte) (core.MediaSource, error) {
	if e.openEntered != nil {
		e.once.Do(func() { close(e.openEntered) })
		<-e.openRelease // deliberately ignores cancellation
	}
	src, err := e.Engine.Open(ctx, id, nzb)
	if err != nil {
		return src, err
	}
	if e.hookRelease != "" && id != e.hookRelease {
		return src, nil
	}
	return &ownedTestSource{MediaSource: src, engine: e}, e.openErr
}

type ownedTestSource struct {
	core.MediaSource
	engine *ownedTestEngine
}

func (s *ownedTestSource) Close() error {
	e := s.engine
	e.closes.Add(1)
	if e.closeEntered != nil {
		close(e.closeEntered)
	}
	if e.closeRelease != nil {
		<-e.closeRelease
	}
	err := s.MediaSource.Close()
	if e.closeErr != nil {
		return e.closeErr
	}
	return err
}
func ownedCounts(m *Manager) (int, int) {
	m.sourceAccounting.mu.Lock()
	defer m.sourceAccounting.mu.Unlock()
	return m.sourceAccounting.opens, m.sourceAccounting.cleanup
}
func requireOwnedPending(t *testing.T, outcome core.SourceRetirement) {
	t.Helper()
	assertRetirementOpen(t, outcome.Done(), "backend responsibility")
	if outcome.Err() == nil {
		t.Fatal("pending outcome reports success")
	}
}

func TestOwnedSourceRetirementCloseOutcome(t *testing.T) {
	for _, closeFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[closeFailure], func(t *testing.T) {
			h := newOwnedHarness(t, []*fakeRelease{mkv("A", 10000, time.Minute)}, []string{"A"})
			e := &ownedTestEngine{Engine: h.engine, closeEntered: make(chan struct{}), closeRelease: make(chan struct{})}
			var release sync.Once
			unblock := func() { release.Do(func() { close(e.closeRelease) }) }
			defer unblock()
			if closeFailure {
				e.closeErr = errors.New("backend close failed")
			}
			h.m.deps.Engine = e
			ps, outcome, err := h.m.PrepareSurfaceSource(context.Background(), owner, movieKey, defaultCaps("owned"), "", false)
			if err != nil {
				t.Fatal(err)
			}
			requireOwnedPending(t, outcome)
			if err := h.m.RetireSource(ps.ID, outcome); err != nil {
				t.Fatal(err)
			}
			waitRetirementTest(t, e.closeEntered, "backend close entered")
			if _, ok := h.m.Session(ps.ID); ok {
				t.Fatal("retiring source remains published")
			}
			requireOwnedPending(t, outcome)
			var wg sync.WaitGroup
			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if err := h.m.RetireSource(ps.ID, outcome); err != nil {
						t.Error(err)
					}
				}()
			}
			wg.Wait()
			unblock()
			waitRetirementTest(t, outcome.Done(), "terminal source outcome")
			if !errors.Is(outcome.Err(), e.closeErr) {
				t.Fatalf("outcome=%v want=%v", outcome.Err(), e.closeErr)
			}
			if e.closes.Load() != 1 {
				t.Fatalf("close calls=%d", e.closes.Load())
			}
			waitFor(t, func() bool { _, cleanup := ownedCounts(h.m); return cleanup == 0 })
			opens, _ := ownedCounts(h.m)
			want := 0
			if closeFailure {
				want = 1
			}
			if opens != want {
				t.Fatalf("open accounting=%d want=%d", opens, want)
			}
		})
	}
}

func TestOwnedSourceRetirementPriorGeneration(t *testing.T) {
	h := newOwnedHarness(t, []*fakeRelease{mkv("A", 10000, time.Minute), mkv("B", 10000, time.Minute)}, []string{"A", "B"})
	e := &ownedTestEngine{Engine: h.engine, hookRelease: "A", closeEntered: make(chan struct{}), closeRelease: make(chan struct{})}
	var once sync.Once
	unblock := func() { once.Do(func() { close(e.closeRelease) }) }
	defer unblock()
	h.m.deps.Engine = e
	ps := h.prepare(owner, movieKey, "prior")
	outcome, _ := h.m.SourceRetirement(ps.ID)
	old := h.m.lookup(ps.ID).current()
	if !old.acquire() {
		t.Fatal("failed old generation acquire")
	}
	var released sync.Once
	releaseOld := func() { released.Do(old.release) }
	defer releaseOld()
	next, err := h.m.Failover(context.Background(), ps.ID, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if next.Resolution.Release.ID != "B" {
		t.Fatal("replacement not B")
	}
	if err := h.m.RetireSource(ps.ID, outcome); err != nil {
		t.Fatal(err)
	}
	requireOwnedPending(t, outcome)
	releasedDone := make(chan struct{})
	go func() { defer close(releasedDone); releaseOld() }()
	waitRetirementTest(t, e.closeEntered, "prior Close")
	requireOwnedPending(t, outcome)
	unblock()
	waitRetirementTest(t, releasedDone, "prior release")
	waitRetirementTest(t, outcome.Done(), "all generations")
	if err := outcome.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestOwnedSourceRetirementSharedFlight(t *testing.T) {
	for _, allCancel := range []bool{false, true} {
		t.Run(map[bool]string{false: "one-cancels", true: "all-cancel"}[allCancel], func(t *testing.T) {
			h := newOwnedHarness(t, []*fakeRelease{mkv("A", 10000, time.Minute)}, []string{"A"})
			e := &ownedTestEngine{Engine: h.engine, openEntered: make(chan struct{}), openRelease: make(chan struct{})}
			var once sync.Once
			unblock := func() { once.Do(func() { close(e.openRelease) }) }
			defer unblock()
			h.m.deps.Engine = e
			type result struct {
				ps      *core.PlaySession
				outcome core.SourceRetirement
				err     error
			}
			start := func(ctx context.Context) <-chan result {
				done := make(chan result, 1)
				go func() {
					ps, o, err := h.m.PrepareSurfaceSource(ctx, owner, movieKey, defaultCaps("shared"), "", false)
					done <- result{ps, o, err}
				}()
				return done
			}
			ctx1, cancel1 := context.WithCancel(context.Background())
			defer cancel1()
			ctx2, cancel2 := context.WithCancel(context.Background())
			defer cancel2()
			one := start(ctx1)
			waitRetirementTest(t, e.openEntered, "shared backend Open")
			two := start(ctx2)
			waitFor(t, func() bool {
				h.m.mu.Lock()
				defer h.m.mu.Unlock()
				for _, f := range h.m.flights {
					if f.waiters == 2 {
						return true
					}
				}
				return false
			})
			cancel1()
			first := <-one
			if !errors.Is(first.err, context.Canceled) || first.outcome == nil {
				t.Fatalf("cancelled result=%+v", first)
			}
			requireOwnedPending(t, first.outcome)
			if allCancel {
				cancel2()
			}
			var second result
			if allCancel {
				second = <-two
				requireOwnedPending(t, second.outcome)
			}
			unblock()
			if !allCancel {
				second = <-two
				if second.err != nil {
					t.Fatal(second.err)
				}
				if err := h.m.RetireSource(second.ps.ID, second.outcome); err != nil {
					t.Fatal(err)
				}
			}
			if first.outcome != second.outcome {
				t.Fatal("shared callers lost exact flight identity")
			}
			waitRetirementTest(t, first.outcome.Done(), "shared flight retirement")
			if err := first.outcome.Err(); err != nil {
				t.Fatal(err)
			}
			if h.engine.openCount() != 1 {
				t.Fatalf("opens=%d", h.engine.openCount())
			}
		})
	}
}

func TestOwnedSourceRetirementLateFailedOpen(t *testing.T) {
	h := newOwnedHarness(t, []*fakeRelease{mkv("A", 10000, time.Minute)}, []string{"A"})
	e := &ownedTestEngine{Engine: h.engine, openErr: errors.New("failed with acquired source"), closeEntered: make(chan struct{}), closeRelease: make(chan struct{})}
	var once sync.Once
	unblock := func() { once.Do(func() { close(e.closeRelease) }) }
	defer unblock()
	h.m.deps.Engine = e
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan core.SourceRetirement, 1)
	go func() {
		_, o, _ := h.m.PrepareSurfaceSource(ctx, owner, movieKey, defaultCaps("late"), "", false)
		done <- o
	}()
	waitRetirementTest(t, e.closeEntered, "failed Open acquired source Close")
	cancel()
	outcome := <-done
	if outcome == nil {
		t.Fatal("failed preparation lost responsibility")
	}
	requireOwnedPending(t, outcome)
	unblock()
	waitRetirementTest(t, outcome.Done(), "failed Open source closed")
	if err := outcome.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestOwnedSourceRetirementAdmissionAndReplacement(t *testing.T) {
	h := newOwnedHarness(t, []*fakeRelease{mkv("A", 10000, time.Minute)}, []string{"A"})
	ps := h.prepare(owner, movieKey, "replace")
	outcome, _ := h.m.SourceRetirement(ps.ID)
	h.m.sourceAccounting.mu.Lock()
	h.m.sourceAccounting.cleanup = maxSourceCleanup
	h.m.sourceAccounting.mu.Unlock()
	if err := h.m.RetireSource(ps.ID, outcome); !errors.Is(err, core.ErrNotAvailable) {
		t.Fatalf("capacity=%v", err)
	}
	if _, ok := h.m.Session(ps.ID); !ok {
		t.Fatal("saturation detached source")
	}
	h.m.sourceAccounting.mu.Lock()
	h.m.sourceAccounting.cleanup = 0
	h.m.sourceAccounting.mu.Unlock()
	h.m.Stop(ps.ID)
	waitRetirementTest(t, outcome.Done(), "legacy stop")
	newPS := h.prepare(owner, movieKey, "replace")
	if err := h.m.RetireSource(newPS.ID, outcome); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.m.Session(newPS.ID); !ok {
		t.Fatal("stale outcome detached replacement")
	}
	h.m.sourceAccounting.mu.Lock()
	h.m.sourceAccounting.opens = maxOwnedSourceOpens
	h.m.sourceAccounting.mu.Unlock()
	before := h.engine.openCount()
	_, failed, err := h.m.PrepareSurfaceSource(context.Background(), owner, movie2, defaultCaps("saturated"), "", false)
	if err == nil || failed == nil {
		t.Fatal("source admission did not refuse")
	}
	waitRetirementTest(t, failed.Done(), "refused source admission")
	if h.engine.openCount() != before {
		t.Fatal("saturated source called backend")
	}
	h.m.sourceAccounting.mu.Lock()
	h.m.sourceAccounting.opens = 1
	h.m.sourceAccounting.mu.Unlock()
}

func TestOwnedSourceRetirementShutdownRetainsBlockedClose(t *testing.T) {
	h := newOwnedHarness(t, []*fakeRelease{mkv("A", 10000, time.Minute)}, []string{"A"})
	e := &ownedTestEngine{Engine: h.engine, closeEntered: make(chan struct{}), closeRelease: make(chan struct{})}
	var once sync.Once
	unblock := func() { once.Do(func() { close(e.closeRelease) }) }
	defer unblock()
	h.m.deps.Engine = e
	ps := h.prepare(owner, movieKey, "shutdown")
	outcome, _ := h.m.SourceRetirement(ps.ID)
	if err := h.m.RetireSource(ps.ID, outcome); err != nil {
		t.Fatal(err)
	}
	waitRetirementTest(t, e.closeEntered, "stalled shutdown Close")
	closed := make(chan error, 1)
	go func() { closed <- h.m.Close() }()
	select {
	case err := <-closed:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Close did not report unfinished cleanup: %v", err)
		}
	case <-time.After(3 * time.Second):
		unblock()
		<-closed
		t.Fatal("manager shutdown waited indefinitely for backend Close")
	}
	requireOwnedPending(t, outcome)
	opens, cleanup := ownedCounts(h.m)
	if opens != 1 || cleanup != 1 {
		t.Fatalf("shutdown dropped accounting: opens=%d cleanup=%d", opens, cleanup)
	}
	unblock()
	waitRetirementTest(t, outcome.Done(), "late shutdown source completion")
	waitFor(t, func() bool { _, cleanup := ownedCounts(h.m); return cleanup == 0 })
	if err := outcome.Err(); err != nil {
		t.Fatal(err)
	}
}
