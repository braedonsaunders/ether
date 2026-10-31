package playback

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func sourceResponsibilities(m *Manager) (int, int) {
	m.sourceAccounting.mu.Lock()
	defer m.sourceAccounting.mu.Unlock()
	return m.sourceAccounting.opens, m.sourceAccounting.owners
}

func requireSourceResponsibilities(t *testing.T, m *Manager, opens, owners int) {
	t.Helper()
	a, b := sourceResponsibilities(m)
	if a != opens || b != owners {
		t.Fatalf("source charges=(%d,%d), want (%d,%d)", a, b, opens, owners)
	}
}

func waitPayloadActor(t *testing.T, p *payloadStore) {
	t.Helper()
	waitFor(t, func() bool { p.mu.Lock(); defer p.mu.Unlock(); return !p.releaseRunning })
}

// Exercise real Engine.Open/MediaSource.Close via the manager's source seam.
// The three boundaries cover a rolled-back DELETE, an uncertain successful
// commit, and filesystem removal whose acknowledgement was lost.
func TestOwnedSourceCleanupLiveRetry(t *testing.T) {
	for _, stage := range []string{"delete", "commit", "remove"} {
		t.Run(stage, func(t *testing.T) {
			h := newOwnedHarness(t, []*fakeRelease{mkv("A", 10000, time.Minute)}, []string{"A"})
			e := &ownedTestEngine{Engine: h.engine}
			h.m.deps.Engine = e
			p := h.m.resume.payload
			var outage atomic.Bool
			outage.Store(true)
			injected := errors.New("cleanup acknowledgement lost")
			hook := func() error {
				if outage.Load() {
					return injected
				}
				return nil
			}
			repair := func() { outage.Store(false) }
			switch stage {
			case "delete":
				repair = blockResumeOwnerCleanup(t, p.db)
			case "commit":
				p.afterRetireCommit = hook
			case "remove":
				p.afterRemove = hook
			}
			t.Cleanup(repair)
			life := newSourceLifetime(1, "plex", "retry")
			nzb := []byte("nzb-A")
			hash := nzbHash(nzb)
			path := filepath.Join(p.dir, hash+".nzb")
			if err := os.WriteFile(path, nzb, 0600); err != nil {
				t.Fatal(err)
			}
			src, finish, err := h.m.openSource(context.Background(), life, &core.Resolution{Release: core.Release{ID: "A"}, NZB: nzb})
			if err != nil {
				t.Fatal(err)
			}
			life.seal()
			finish(src.Close())
			for i := 0; i < 8; i++ {
				finish(nil)
			}
			requireOwnedPending(t, life)
			requireSourceResponsibilities(t, h.m, 0, 1)
			p.mu.Lock()
			queued, callbacks, active, witness := len(p.retired), len(p.releasedAfter), p.active, p.file != nil
			var exact payloadOwner
			for _, o := range p.retired {
				exact = o
			}
			p.mu.Unlock()
			// active counts every payload actor. Pin recovery can hold one
			// extra slot beside the admission that failed cleanup still owns.
			// Queue, callback and witness are the protection; active == 0 is the loss.
			if queued != 1 || callbacks != 1 || active < 1 || active > 2 || !witness {
				t.Fatalf("lost protection: queue=%d callbacks=%d active=%d witness=%v", queued, callbacks, active, witness)
			}
			if stage == "delete" && payloadRows(t, p, "playback_payload_owners", p.incarnation) != 1 {
				t.Fatal("failed DELETE lost durable owner")
			}
			if stage != "delete" && payloadRows(t, p, "playback_payload_deleting", p.incarnation) != 1 {
				t.Fatal("uncertain cleanup lost marker")
			}
			// A live incarnation cannot be reconciled away by another manager.
			other, err := openPayloadStore(context.Background(), p.db, p.dir)
			if err != nil {
				t.Fatal(err)
			}
			defer other.close()
			requireOwnedPending(t, life)
			repair()
			waitRetirementTest(t, life.Done(), "live cleanup repair")
			if err := life.Err(); err != nil {
				t.Fatal(err)
			}
			waitPayloadActor(t, p)
			requireSourceResponsibilities(t, h.m, 0, 0)
			if e.closes.Load() != 1 {
				t.Fatalf("backend Close repeated: %d", e.closes.Load())
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("payload remains: %v", err)
			}
			// Requeue of an already released exact ref cannot delete its replacement.
			newer := payloadReserve(t, p, hash)
			var duplicate atomic.Int32
			p.queueReleaseAfter(exact, func() { duplicate.Add(1) })
			waitPayloadActor(t, p)
			if duplicate.Load() != 0 || payloadRows(t, p, "playback_payload_owners", p.incarnation) != 1 {
				t.Fatal("old completion affected newer owner")
			}
			if err := p.release(context.Background(), newer); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Verify the public Prepare/Retire path as well as the low-level finish seam.
func TestOwnedSourceCleanupPublicRetirement(t *testing.T) {
	h := newOwnedHarness(t, []*fakeRelease{mkv("A", 10000, time.Minute)}, []string{"A"})
	e := &ownedTestEngine{Engine: h.engine}
	h.m.deps.Engine = e
	ps, outcome, err := h.m.PrepareSurfaceSource(context.Background(), owner, movieKey, defaultCaps("retry-public"), "", false)
	if err != nil {
		t.Fatal(err)
	}
	repair := blockResumeOwnerCleanup(t, h.m.resume.db)
	t.Cleanup(repair)
	if err := h.m.RetireSource(ps.ID, outcome); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { a, _ := sourceResponsibilities(h.m); return a == 0 })
	requireOwnedPending(t, outcome)
	requireSourceResponsibilities(t, h.m, 0, 1)
	repair()
	waitRetirementTest(t, outcome.Done(), "public retirement repaired")
	if err := outcome.Err(); err != nil {
		t.Fatal(err)
	}
	waitPayloadActor(t, h.m.resume.payload)
	requireSourceResponsibilities(t, h.m, 0, 0)
	if e.closes.Load() != 1 {
		t.Fatal("backend closed more than once")
	}
}

func TestOwnedSourceCleanupShutdownAndCloseFailure(t *testing.T) {
	for _, backendFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "pending-cleanup", true: "backend-failure"}[backendFailure], func(t *testing.T) {
			h := newOwnedHarness(t, []*fakeRelease{mkv("A", 10000, time.Minute)}, []string{"A"})
			e := &ownedTestEngine{Engine: h.engine}
			if backendFailure {
				e.closeErr = errors.New("backend close failed")
			}
			h.m.deps.Engine = e
			ps, outcome, err := h.m.PrepareSurfaceSource(context.Background(), owner, movieKey, defaultCaps("retry-shutdown"), "", false)
			if err != nil {
				t.Fatal(err)
			}
			p := h.m.resume.payload
			repair := blockResumeOwnerCleanup(t, p.db)
			t.Cleanup(repair)
			h.m.Stop(ps.ID)
			if backendFailure {
				waitRetirementTest(t, outcome.Done(), "failed backend outcome")
				if !errors.Is(outcome.Err(), e.closeErr) {
					t.Fatal(outcome.Err())
				}
			} else {
				requireOwnedPending(t, outcome)
			}
			if err := h.m.Close(); err == nil {
				t.Fatal("shutdown hid protected source responsibility")
			}
			waitPayloadActor(t, p)
			p.mu.Lock()
			queued, callbacks, active, witness := len(p.retired), len(p.releasedAfter), p.active, p.file != nil
			p.mu.Unlock()
			want := 1
			if backendFailure {
				want = 0
			}
			if queued != want || callbacks != want || active != 1 || !witness {
				t.Fatalf("shutdown protection queue=%d callback=%d active=%d witness=%v", queued, callbacks, active, witness)
			}
			opens := 0
			if backendFailure {
				opens = 1
			}
			requireSourceResponsibilities(t, h.m, opens, 1)
			if payloadRows(t, p, "playback_payload_owners", p.incarnation) != 1 {
				t.Fatal("shutdown dropped payload owner")
			}
			repair()
			if !backendFailure {
				requireOwnedPending(t, outcome)
			}
			if e.closes.Load() != 1 {
				t.Fatal("shutdown retried backend close")
			}
		})
	}
}

func TestOwnedSourceResponsibilityCapacityAndRollback(t *testing.T) {
	h := newOwnedHarness(t, []*fakeRelease{mkv("A", 10000, time.Minute)}, []string{"A"})
	life := newSourceLifetime(1, "plex", "capacity")
	tickets := make([]*sourceOpenTicket, 0, maxOwnedSourceResponsibilities)
	for i := 0; i < maxOwnedSourceResponsibilities; i++ {
		ticket, err := h.m.reserveSourceOpen(life)
		if err != nil {
			t.Fatal(err)
		}
		ticket.physical(nil)
		tickets = append(tickets, ticket)
	}
	before := h.engine.openCount()
	if _, _, err := h.m.openSource(context.Background(), life, &core.Resolution{Release: core.Release{ID: "A"}, NZB: []byte("capacity")}); !errors.Is(err, core.ErrNotAvailable) {
		t.Fatalf("capacity=%v", err)
	}
	if h.engine.openCount() != before {
		t.Fatal("saturated responsibility called backend")
	}
	life.seal()
	requireOwnedPending(t, life)
	var wg sync.WaitGroup
	for _, ticket := range tickets {
		wg.Add(1)
		go func(ticket *sourceOpenTicket) {
			defer wg.Done()
			ticket.physical(nil)
			ticket.settle(nil)
			ticket.settle(nil)
		}(ticket)
	}
	wg.Wait()
	waitRetirementTest(t, life.Done(), "bounded responsibility drain")
	requireSourceResponsibilities(t, h.m, 0, 0)
	// Failed admission before backend work rolls both charges back exactly once.
	refused := newSourceLifetime(0, "plex", "invalid")
	if _, _, err := h.m.openSource(context.Background(), refused, &core.Resolution{Release: core.Release{ID: "A"}, NZB: []byte("invalid")}); err == nil {
		t.Fatal("invalid payload owner accepted")
	}
	refused.seal()
	waitRetirementTest(t, refused.Done(), "admission rollback")
	requireSourceResponsibilities(t, h.m, 0, 0)
}

func TestPayloadReleaseCallbackImmediateAndDeduplicated(t *testing.T) {
	p := payloadFixture(t)
	o := payloadReserve(t, p, nzbHash([]byte("immediate")))
	if err := p.release(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	callback := func() {
		// All locks are available and queue ownership was removed before callback.
		o.state.mu.Lock()
		o.state.mu.Unlock()
		if err := p.enter(context.Background()); err != nil {
			panic(err)
		}
		p.leave()
		p.mu.Lock()
		_, queued := p.retired[o.state.id]
		_, retained := p.releasedAfter[o.state.id]
		p.mu.Unlock()
		if queued || retained {
			panic("callback still owned by queue")
		}
		calls.Add(1)
	}
	if p.queueReleaseAfter(payloadOwner{}, callback) {
		t.Fatal("invalid enqueue accepted")
	}
	p.queueReleaseAfter(o, callback)
	waitPayloadActor(t, p)
	p.queueReleaseAfter(o, callback)
	waitPayloadActor(t, p)
	if calls.Load() != 1 {
		t.Fatalf("completion calls=%d", calls.Load())
	}
	p.close()
	late := payloadOwner{state: &payloadOwnerState{id: "late"}}
	p.queueReleaseAfter(late, callback)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.releaseRunning || len(p.retired) != 1 || len(p.releasedAfter) != 1 {
		t.Fatal("shutdown enqueue lost obligation or started actor")
	}
}

func TestOwnedSourceCleanupSharedAndNewerOwner(t *testing.T) {
	h := newOwnedHarness(t, []*fakeRelease{mkv("A", 10000, time.Minute)}, []string{"A"})
	e := &ownedTestEngine{Engine: h.engine}
	h.m.deps.Engine = e
	p := h.m.resume.payload
	ctx := context.Background()
	nzb := []byte("nzb-A")
	res := &core.Resolution{Release: core.Release{ID: "A"}, NZB: nzb}
	first, second := newSourceLifetime(1, "plex", "one"), newSourceLifetime(1, "silo", "two")
	a, finishA, err := h.m.openSource(ctx, first, res)
	if err != nil {
		t.Fatal(err)
	}
	b, finishB, err := h.m.openSource(ctx, second, res)
	if err != nil {
		t.Fatal(err)
	}
	first.seal()
	second.seal()
	repair := blockResumeOwnerCleanup(t, p.db)
	t.Cleanup(repair)
	// A shared ref can settle without dropping the shared durable anchor.
	finishA(a.Close())
	waitRetirementTest(t, first.Done(), "shared reference settled")
	requireSourceResponsibilities(t, h.m, 1, 1)
	if payloadRows(t, p, "playback_payload_owners", p.incarnation) != 1 {
		t.Fatal("first release dropped shared owner")
	}
	finishB(b.Close())
	requireOwnedPending(t, second)
	requireSourceResponsibilities(t, h.m, 0, 1)
	// A different live incarnation owns the same hash while old cleanup retries.
	newer, err := openPayloadStore(ctx, p.db, p.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer newer.close()
	next := payloadReserve(t, newer, nzbHash(nzb))
	repair()
	waitRetirementTest(t, second.Done(), "shared final cleanup repaired")
	waitPayloadActor(t, p)
	if first.Err() != nil || second.Err() != nil {
		t.Fatal("shared retirement failed")
	}
	requireSourceResponsibilities(t, h.m, 0, 0)
	if payloadRows(t, p, "playback_payload_owners", newer.incarnation) != 1 {
		t.Fatal("old cleanup removed newer owner")
	}
	for i := 0; i < 8; i++ {
		finishA(nil)
		finishB(nil)
	}
	requireSourceResponsibilities(t, h.m, 0, 0)
	if e.closes.Load() != 1 {
		t.Fatalf("backend closes=%d", e.closes.Load())
	}
	if err := newer.release(ctx, next); err != nil {
		t.Fatal(err)
	}
}

func TestOwnedSourceCleanupUnknownAdmissionStaysProtected(t *testing.T) {
	h := newOwnedHarness(t, []*fakeRelease{mkv("A", 10000, time.Minute)}, []string{"A"})
	p := h.m.resume.payload
	p.afterOwnerCommit = func() error { return errors.New("lost admission acknowledgement") }
	life := newSourceLifetime(1, "plex", "uncertain")
	before := h.engine.openCount()
	_, _, err := h.m.openSource(context.Background(), life, &core.Resolution{Release: core.Release{ID: "A"}, NZB: []byte("unknown admission")})
	if !errors.Is(err, errPayloadCommitUnknown) {
		t.Fatalf("admission error=%v", err)
	}
	life.seal()
	waitRetirementTest(t, life.Done(), "unknown admission terminal error")
	if life.Err() == nil {
		t.Fatal("unknown admission authorized retirement")
	}
	requireSourceResponsibilities(t, h.m, 0, 1)
	if h.engine.openCount() != before {
		t.Fatal("uncertain payload admission called backend")
	}
	// Startup pin reconcile runs a recovery actor that shares this counter.
	// Wait until that actor has finished so the count is only the admission.
	waitFor(t, func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return !p.recoveryRunning && p.active == 1
	})
	p.mu.Lock()
	queued, active := len(p.retired), p.active
	p.mu.Unlock()
	if queued != 0 || active != 1 || payloadRows(t, p, "playback_payload_owners", p.incarnation) != 1 {
		t.Fatal("unknown admission lost protection or queued deletion")
	}
}

// Enqueue from the completion edge must remain owned by the same actor. This
// also exercises a callback that reenters store accounting without lock cycles.
func TestPayloadReleaseCallbackReentrantEnqueue(t *testing.T) {
	p := payloadFixture(t)
	a := payloadReserve(t, p, nzbHash([]byte("callback A")))
	b := payloadReserve(t, p, nzbHash([]byte("callback B")))
	done := make(chan struct{})
	p.queueReleaseAfter(a, func() { p.queueReleaseAfter(b, func() { close(done) }) })
	waitRetirementTest(t, done, "callback reentrant enqueue")
	waitPayloadActor(t, p)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.active != 0 || len(p.retired) != 0 || len(p.releasedAfter) != 0 {
		t.Fatal("completion edge leaked actor ownership")
	}
}
