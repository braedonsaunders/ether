package playback

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

// A TorBox CDN edge can collapse for tens of seconds on a fully cached
// release: the November 6 The Shining session logged repeated
// "slow media response ... bytes=1048576 err=true" took=10s/15s events and
// every "failing over" candidate died in under 300 ms ("not cached",
// "incompatible direct-play replacement: not available yet") while the
// original file stayed playable. A stall-triggered failover that finds no
// spliced replacement must continue the original release once instead of
// handing the session to a remux candidate over the same failing edge.
func TestStallFailoverContinuesOriginalRelease(t *testing.T) {
	a := mkv("A", 200000, 100*time.Second)
	h := newHarness(t, []*fakeRelease{a}, []string{"A"})
	ps := h.prepare(owner, movieKey, "stall-edge")
	s := h.m.lookup(ps.ID)
	g := s.current()
	<-g.indexReady
	g.src = retryDirectSource{g.src}

	res, err := h.m.failoverWith(context.Background(), s, g, 3*time.Minute, failoverOpts{reason: "media source stalled", strictDirect: true, stallRecovery: true})
	if err != nil || res != g {
		t.Fatalf("first stall failover: res=%v err=%v, want the original release continued", res, err)
	}
	if _, ok := h.m.Session(ps.ID); !ok {
		t.Fatal("edge-caused stall ended the session")
	}
	if h.m.RequiresRemux(ps.ID) {
		t.Fatal("stall continue switched the session to remux")
	}

	// The grace is bounded: a second sustained stall escalates as before.
	if res, err := h.m.failoverWith(context.Background(), s, g, 3*time.Minute, failoverOpts{reason: "media source stalled", strictDirect: true, stallRecovery: true}); err == nil || res == g {
		t.Fatalf("second stall failover: res=%v err=%v, want escalation without a stall continue", res, err)
	}
}

// A provider pause (rate-limit) cannot improve by waiting the same host out,
// and a non-stall failover must not consume or benefit from the grace.
func TestNonStallAndPausedSourceFailoversStayTerminal(t *testing.T) {
	a := mkv("A", 200000, 100*time.Second)
	h := newHarness(t, []*fakeRelease{a}, []string{"A"})
	ps := h.prepare(owner, movieKey, "stall-edge-terminal")
	s := h.m.lookup(ps.ID)
	g := s.current()
	<-g.indexReady
	g.src = retryDirectSource{g.src}
	if res, err := h.m.failoverWith(context.Background(), s, g, time.Second, failoverOpts{reason: "source read failed during direct play", strictDirect: true}); err == nil || res == g {
		t.Fatalf("non-stall failover: res=%v err=%v, want the terminal behaviour", res, err)
	}

	h2 := newHarness(t, []*fakeRelease{mkv("B", 200000, 100*time.Second)}, []string{"B"})
	ps2 := h2.prepare(owner, movieKey, "stall-edge-paused")
	s2 := h2.m.lookup(ps2.ID)
	g2 := s2.current()
	<-g2.indexReady
	g2.src = pausedRetrySource{g2.src}
	if res, err := h2.m.failoverWith(context.Background(), s2, g2, time.Second, failoverOpts{reason: "media source stalled", strictDirect: true, stallRecovery: true}); err == nil || res == g2 {
		t.Fatalf("paused-source stall failover: res=%v err=%v, want a pause to stay terminal", res, err)
	}
}

// End to end through streamClient, including the second-stall termination,
// lives in TestDirectStallReadBudgetEndsSession (direct_outage_replay_test.go),
// which was extended alongside this behaviour change.

// A budget-expiry continuation consumed the grace without returning a
// retryable error, so the read path itself must arm the bounded watchdog:
// the client may never issue another range request for the stalled read,
// and issue #169 demands such a session still ends. Forcing a second read
// (TestDirectStallReadBudgetEndsSession) cannot see this guarantee.
func TestStallReadBudgetContinuationArmsWatchdog(t *testing.T) {
	a := mkv("A", 200000, 100*time.Second)
	h := newHarness(t, []*fakeRelease{a}, []string{"A"})
	ps := h.prepare(owner, movieKey, "stall-watchdog")
	s := h.m.lookup(ps.ID)
	g := s.current()
	<-g.indexReady
	g.src = stalledRetrySource{g.src}
	ctx := context.WithValue(context.Background(), directReadBudgetKey{}, 20*time.Millisecond)
	if err := h.m.streamClient(ctx, io.Discard, s, 120000, 4096); !errors.Is(err, errEndResponse) {
		t.Fatalf("stalled read: %v", err)
	}
	if _, ok := h.m.Session(ps.ID); !ok {
		t.Fatal("stall grace ended the session")
	}
	g.mu.Lock()
	armed := g.retryPending
	// Invalidate the production 25s window and exercise the same
	// escalation on a short offline clock, as TestBoundedDirectOutageRecovery does.
	g.retryPending = false
	g.retryEpoch++
	g.mu.Unlock()
	if !armed {
		t.Fatal("budget-expiry continuation left the session without a recovery watchdog")
	}
	h.m.limitDirectRetryAfter(s, g, 30*time.Second, 20*time.Millisecond)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := h.m.Session(ps.ID); !ok {
			return // the watchdog escalated without a client re-request
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("stalled session with a silent client never terminated")
}

// A successful read between the grace windows must retire the watchdog
// rather than merely postpone the escalation: the second window keeps the
// epoch captured at arm time, so progress that races the re-arm is never
// swallowed by a re-armed timer.
func TestStallSecondWindowCancelledByProgress(t *testing.T) {
	a := mkv("A", 200000, 100*time.Second)
	h := newHarness(t, []*fakeRelease{a}, []string{"A"})
	ps := h.prepare(owner, movieKey, "stall-progress")
	s := h.m.lookup(ps.ID)
	g := s.current()
	<-g.indexReady
	g.src = retryDirectSource{g.src}
	h.m.limitDirectRetryAfter(s, g, 30*time.Second, 400*time.Millisecond)
	// First window: the timer's failover spends the grace and re-waits in
	// place. Poll for the spend, then simulate the client's next successful
	// read (the count path: epoch advances, pending clears).
	deadline := time.Now().Add(3 * time.Second)
	for {
		s.mu.Lock()
		used := s.stallContinues
		s.mu.Unlock()
		if used > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first recovery window did not spend the stall grace")
		}
		time.Sleep(2 * time.Millisecond)
	}
	g.mu.Lock()
	g.retryEpoch++
	g.retryPending = false
	g.mu.Unlock()
	time.Sleep(1600 * time.Millisecond) // second window would have fired at ~800ms
	if _, ok := h.m.Session(ps.ID); !ok {
		t.Fatal("recovered session was escalated after progress")
	}
	if h.m.RequiresRemux(ps.ID) {
		t.Fatal("progress-retired watchdog escalated to remux")
	}
}

// The grace must not permanently lose a prefetched, openable but
// unspliceable fallback: the handoff is deferred one window, and must
// still happen. Dropping the fallback generation closes it; the next
// failover re-dials it from the
// alternates — this test pins that the deferred handoff is a real guarantee,
// not an assumption, and records the deliberate policy: under a stalled
// provider network the original release outranks an unspliceable fallback.
func TestStallGraceDefersThenHandsOffOpenableFallback(t *testing.T) {
	a := mkv("A", 200000, 100*time.Second)
	b := mkv("B", 150000, 100*time.Second) // opens, but not spliceable: remux-only
	h := newHarness(t, []*fakeRelease{a, b}, []string{"A", "B"})
	ps := h.prepare(owner, movieKey, "stall-fallback")
	s := h.m.lookup(ps.ID)
	g := s.current()
	<-g.indexReady
	g.src = retryDirectSource{g.src}
	if res, err := h.m.failoverWith(context.Background(), s, g, 3*time.Minute, failoverOpts{reason: "media source stalled", strictDirect: true, stallRecovery: true}); err != nil || res != g {
		t.Fatalf("stall failover with openable fallback: res=%v err=%v, want the original release continued", res, err)
	}
	if h.m.RequiresRemux(ps.ID) {
		t.Fatal("stall continue handed off before the grace window expired")
	}
	res, err := h.m.failoverWith(context.Background(), s, g, 3*time.Minute, failoverOpts{reason: "media source stalled", strictDirect: true, stallRecovery: true})
	if !errors.Is(err, errRequireRemux) || res == nil || res == g {
		t.Fatalf("second stall failover: res=%v err=%v, want the re-offered fallback to take over as a remux handoff", res, err)
	}
	if !h.m.RequiresRemux(ps.ID) {
		t.Fatal("fallback takeover did not mark the session for remux")
	}
	if _, ok := h.m.Session(ps.ID); !ok {
		t.Fatal("fallback takeover ended the session")
	}
}

// Progress that arrives after the entry check but before the grace branch
// must not burn the grace — and the early return must not strand the
// fallback it owns: it drops the opened fallback generation on the way
// out, mirroring the surrounding cancellation paths.
func TestStallProgressDuringSearchRetiresGraceAndFallback(t *testing.T) {
	a := mkv("A", 200000, 100*time.Second)
	b := mkv("B", 150000, 100*time.Second) // openable, unspliceable: kept as fallback
	h := newHarness(t, []*fakeRelease{a, b}, []string{"A", "B"})
	ps := h.prepare(owner, movieKey, "stall-search-progress")
	s := h.m.lookup(ps.ID)
	g := s.current()
	<-g.indexReady
	g.src = stalledRetrySource{g.src}
	// The client resumes (stillNeeded false) only after the candidate
	// search opened and preflighted B. The first stillNeeded call is the
	// entry check; the second is the grace branch's — reached with
	// fallbackGen in hand, exactly the scenario the branch guards.
	calls := 0
	stillNeeded := func() bool {
		calls++
		return calls < 2
	}
	res, err := h.m.failoverWith(context.Background(), s, g, 3*time.Minute, failoverOpts{reason: "media source stalled", strictDirect: true, stallRecovery: true, stillNeeded: stillNeeded})
	if err != nil || res != g {
		t.Fatalf("failover with progress during search: res=%v err=%v, want the original release kept", res, err)
	}
	s.mu.Lock()
	spent := s.stallContinues
	s.mu.Unlock()
	if spent != 0 {
		t.Fatal("progress during the search spent the stall grace")
	}
	if h.m.RequiresRemux(ps.ID) {
		t.Fatal("progress during the search switched the session to remux")
	}
	var opened *fakeSource
	for _, src := range h.engine.sources {
		if src.rel.ID == "B" {
			opened = src
		}
	}
	if opened == nil {
		t.Fatal("candidate search never opened the fallback release")
	}
	eventually(t, "fallback source closed after progress retired recovery", func() bool { return opened.closed.Load() })
}
