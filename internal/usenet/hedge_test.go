package usenet

import (
	"bytes"
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/usenet/nntptest"
	"github.com/braedonsaunders/ether/internal/yenc"
)

// fastHedge shrinks the hedge delays so tests run in milliseconds; the
// relationships between them match defaultHedgePolicy.
func fastHedge(f *Fetcher, floor time.Duration) {
	f.live.policy.floor = floor
	f.live.policy.overtakenFloor = floor / 2
	f.live.policy.cold = floor
	f.live.policy.recheck = 20 * time.Millisecond
}

func poolFor(t *testing.T, f *Fetcher, id string) *pool {
	t.Helper()
	f.mu.RLock()
	defer f.mu.RUnlock()
	for _, p := range f.pools {
		if p.cfg.ID == id {
			return p
		}
	}
	t.Fatalf("no pool %q", id)
	return nil
}

// stallBody stalls every BODY for ids mid-body (connection open, no bytes)
// for d, and records when each BODY arrived.
func stallBody(s *nntptest.Server, d time.Duration, ids ...string) *bodyLog {
	l := &bodyLog{}
	s.SetFault(func(cmd, arg string) nntptest.Fault {
		if cmd != "BODY" {
			return nntptest.Fault{}
		}
		l.note(arg)
		for _, id := range ids {
			if arg == id {
				return nntptest.Fault{BodyDelay: d}
			}
		}
		return nntptest.Fault{}
	})
	return l
}

type bodyLog struct {
	mu sync.Mutex
	at []time.Time
}

func (l *bodyLog) note(string) {
	l.mu.Lock()
	l.at = append(l.at, time.Now())
	l.mu.Unlock()
}

func (l *bodyLog) times() []time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]time.Time(nil), l.at...)
}

// waitGoroutines polls until the goroutine count falls back to base plus
// one fake-server handler per connection still open (idle pooled sockets).
func waitGoroutines(t *testing.T, base int, servers ...*nntptest.Server) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		want := base
		for _, s := range servers {
			want += s.Stats().Active
		}
		if runtime.NumGoroutine() <= want {
			return
		}
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<20)
			t.Fatalf("goroutines = %d, want <= %d\n%s", runtime.NumGoroutine(), want, buf[:runtime.Stack(buf, true)])
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// twoTiers returns a stalling primary-tier provider "a" and a healthy
// second-tier provider "b", both holding want under id.
func twoTiers(t *testing.T, id string, want []byte, aConns, bConns int) (a, b *nntptest.Server, pa, pb config.UsenetProvider) {
	t.Helper()
	a = nntptest.Start(t, nntptest.Options{})
	b = nntptest.Start(t, nntptest.Options{})
	addSeg(t, a, id, want)
	addSeg(t, b, id, want)
	pa, pb = a.Provider("a", aConns), b.Provider("b", bConns)
	pb.Priority = 1
	return a, b, pa, pb
}

// A stalled live BODY is hedged on the other provider once the threshold
// passes, never earlier; the hedge's bytes reach the caller, the stalled
// primary connection is cancelled and discarded, and nothing leaks.
func TestHedgeStalledLiveFetchWinsOnOtherProvider(t *testing.T) {
	want := randData(1, 200_000)
	a, b, pa, pb := twoTiers(t, "a@x", want, 2, 4)
	stallBody(a, 30*time.Second, "a@x")
	bLog := stallBody(b, 0)
	// The idle deadline is far away: only the hedge can answer quickly.
	f := newFetcher(t, []config.UsenetProvider{pa, pb}, Options{IdleReadTimeout: 10 * time.Second})
	const threshold = 150 * time.Millisecond
	fastHedge(f, threshold)
	base := runtime.NumGoroutine()

	start := time.Now()
	got, err := f.Body(context.Background(), "a@x", nil, core.PriorityLive)
	took := time.Since(start)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("hedged fetch: len=%d err=%v", len(got), err)
	}
	if took > threshold+time.Second {
		t.Fatalf("hedged fetch took %v; threshold %v", took, threshold)
	}
	sent := bLog.times()
	if len(sent) != 1 {
		t.Fatalf("hedge BODYs on b = %d, want 1", len(sent))
	}
	if early := sent[0].Sub(start); early < threshold {
		t.Fatalf("hedge sent after %v, before the %v threshold", early, threshold)
	}
	if st := statsFor(f, "b"); st.Hedges != 1 || st.HedgeWins != 1 || st.HedgeLosses != 0 {
		t.Fatalf("hedge stats on b: %+v", st)
	}
	if st := statsFor(f, "a"); st.Hedges != 0 || st.State != StateOK {
		t.Fatalf("stalled provider charged with hedges or a failure: %+v", st)
	}
	// The stalled primary is cancelled: its socket closes rather than
	// waiting out the stall, and no goroutine outlives the call.
	waitServerActive(t, a, 0)
	waitGoroutines(t, base, a, b)
	if st := statsFor(f, "a"); st.LiveConnections != 0 || st.ActiveConnections != 0 {
		t.Fatalf("stalled primary connection kept: %+v", st)
	}
	if st := statsFor(f, "b"); st.ActiveConnections != 0 || st.LiveConnections != 1 {
		t.Fatalf("hedge connection not returned to the pool: %+v", st)
	}
}

// A fast primary is never hedged: the hedge waits the full threshold.
func TestHedgeNotBeforeThreshold(t *testing.T) {
	want := randData(2, 50_000)
	a, b, pa, pb := twoTiers(t, "a@x", want, 2, 4)
	a.SetFault(func(cmd, arg string) nntptest.Fault {
		if cmd == "BODY" {
			return nntptest.Fault{Delay: 60 * time.Millisecond}
		}
		return nntptest.Fault{}
	})
	f := newFetcher(t, []config.UsenetProvider{pa, pb}, Options{})
	fastHedge(f, 250*time.Millisecond)
	for range 3 {
		got, err := f.Body(context.Background(), "a@x", nil, core.PriorityLive)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("len=%d err=%v", len(got), err)
		}
	}
	if n := b.BodyRequests("a@x"); n != 0 {
		t.Fatalf("fast primary hedged %d times", n)
	}
	if st := statsFor(f, "a"); st.LiveFetchP50 < 60*time.Millisecond {
		t.Fatalf("live p50 not recorded: %+v", st)
	}
}

// With one provider, the hedge uses a fresh connection of the same pool
// while it has spare slots.
func TestHedgeSingleProviderFreshConnection(t *testing.T) {
	s := nntptest.Start(t, nntptest.Options{})
	want := randData(3, 150_000)
	addSeg(t, s, "a@x", want)
	var bodies atomic.Int32
	s.SetFault(func(cmd, arg string) nntptest.Fault {
		if cmd == "BODY" && bodies.Add(1) == 1 {
			return nntptest.Fault{BodyDelay: 30 * time.Second}
		}
		return nntptest.Fault{}
	})
	f := newFetcher(t, []config.UsenetProvider{s.Provider("p", 4)}, Options{IdleReadTimeout: 10 * time.Second})
	fastHedge(f, 100*time.Millisecond)
	base := runtime.NumGoroutine()

	start := time.Now()
	got, err := f.Body(context.Background(), "a@x", nil, core.PriorityLive)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("len=%d err=%v", len(got), err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("single-provider hedge took %v", d)
	}
	if n := s.BodyRequests("a@x"); n != 2 {
		t.Fatalf("BODY requests = %d, want primary + hedge", n)
	}
	if st := statsFor(f, "p"); st.Hedges != 1 || st.HedgeWins != 1 {
		t.Fatalf("hedge stats: %+v", st)
	}
	waitServerActive(t, s, 1) // the stalled socket closed; the hedge's is idle
	waitGoroutines(t, base, s)
}

// No provider with a spare slot means no hedge: the fetch waits for its
// primary, which here answers after a short stall.
func TestHedgeNeedsSpareCapacity(t *testing.T) {
	want := randData(4, 100_000)
	a, b, pa, pb := twoTiers(t, "a@x", want, 1, 2)
	stallBody(a, 400*time.Millisecond, "a@x")
	f := newFetcher(t, []config.UsenetProvider{pa, pb}, Options{IdleReadTimeout: -1})
	fastHedge(f, 50*time.Millisecond)
	// b has two slots: one held elsewhere leaves none spare, and a's only
	// slot belongs to the stalled primary.
	pbPool := poolFor(t, f, "b")
	if !pbPool.sem.TryAcquire(core.PriorityLive) {
		t.Fatal("could not occupy b")
	}
	defer pbPool.sem.Release(core.PriorityLive)

	type result struct {
		data []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		data, err := f.Body(context.Background(), "a@x", nil, core.PriorityLive)
		done <- result{data, err}
	}()
	// Well past the 50ms threshold and still inside the 400ms stall. Once
	// the primary releases its slot a hedge may start in the instant before
	// its result is delivered; it is cancelled at once and not checked here.
	time.Sleep(300 * time.Millisecond)
	if n := b.BodyRequests("a@x") + a.BodyRequests("a@x"); n != 1 {
		t.Fatalf("BODY requests during the stall = %d, want only the primary", n)
	}
	for _, id := range []string{"a", "b"} {
		if st := statsFor(f, id); st.Hedges != 0 {
			t.Fatalf("hedge without spare capacity on %s: %+v", id, st)
		}
	}
	if r := <-done; r.err != nil || !bytes.Equal(r.data, want) {
		t.Fatalf("len=%d err=%v", len(r.data), r.err)
	}
}

// Readahead and prefetch lanes never hedge, and block accounts are not
// hedge targets while the primary path is still working.
func TestHedgeOnlyLiveAndNeverBackfill(t *testing.T) {
	want := randData(5, 80_000)
	for _, tc := range []struct {
		name     string
		prio     core.Priority
		backfill bool
	}{
		{"readahead", core.PriorityReadahead, false},
		{"prefetch", core.PriorityPrefetch, false},
		{"live-backfill", core.PriorityLive, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b, pa, pb := twoTiers(t, "a@x", want, 2, 4)
			pb.Backfill = tc.backfill
			stallBody(a, 300*time.Millisecond, "a@x")
			f := newFetcher(t, []config.UsenetProvider{pa, pb}, Options{IdleReadTimeout: -1})
			fastHedge(f, 30*time.Millisecond)
			got, err := f.Body(context.Background(), "a@x", nil, tc.prio)
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("len=%d err=%v", len(got), err)
			}
			if n := b.BodyRequests("a@x"); n != 0 {
				t.Fatalf("%s hedged to b %d times", tc.name, n)
			}
			if st := statsFor(f, "b"); st.Hedges != 0 {
				t.Fatalf("hedge stats: %+v", st)
			}
		})
	}
}

// The fetcher-wide cap holds concurrent hedges to one per eight primary
// slots: two stalled heads on six slots never hedge at once.
func TestHedgeGlobalCap(t *testing.T) {
	a := nntptest.Start(t, nntptest.Options{})
	b := nntptest.Start(t, nntptest.Options{})
	ids := []string{"h1@x", "h2@x"}
	want := map[string][]byte{}
	for i, id := range ids {
		want[id] = randData(int64(10+i), 60_000)
		addSeg(t, a, id, want[id])
		addSeg(t, b, id, want[id])
	}
	stallBody(a, 3*time.Second, ids...)
	b.SetFault(func(cmd, arg string) nntptest.Fault {
		if cmd == "BODY" {
			return nntptest.Fault{Delay: 300 * time.Millisecond}
		}
		return nntptest.Fault{}
	})
	pa, pb := a.Provider("a", 2), b.Provider("b", 4)
	pb.Priority = 1
	f := newFetcher(t, []config.UsenetProvider{pa, pb}, Options{IdleReadTimeout: -1})
	fastHedge(f, 50*time.Millisecond)
	if n := f.hedgeLimit(); n != 1 {
		t.Fatalf("hedge limit = %d, want 1", n)
	}
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Go(func() {
			got, err := f.Body(context.Background(), id, nil, core.PriorityLive)
			if err != nil || !bytes.Equal(got, want[id]) {
				t.Errorf("%s: len=%d err=%v", id, len(got), err)
			}
		})
	}
	wg.Wait()
	if st := b.Stats(); st.MaxActive != 1 {
		t.Fatalf("b served %d concurrent hedges, want 1", st.MaxActive)
	}
	if st := statsFor(f, "b"); st.Hedges != 2 || st.HedgeWins != 2 {
		t.Fatalf("hedges should run one after another: %+v", st)
	}
}

func TestHedgeAdmissionBounds(t *testing.T) {
	h := newLiveHedger()
	if !h.admit("x", 2) || h.admit("x", 2) {
		t.Fatal("a second hedge for one article was admitted")
	}
	if !h.admit("y", 2) || h.admit("z", 2) {
		t.Fatal("global cap not enforced")
	}
	h.release("x")
	if !h.admit("z", 2) {
		t.Fatal("released capacity not reusable")
	}
}

// A hedge winner must be a CRC-valid decode: a corrupt hedge loses and the
// primary's correct bytes are returned.
func TestHedgeCorruptResultDoesNotWin(t *testing.T) {
	want := randData(6, 120_000)
	a, b, pa, pb := twoTiers(t, "a@x", want, 2, 4)
	if err := b.Corrupt("a@x"); err != nil {
		t.Fatal(err)
	}
	stallBody(a, 400*time.Millisecond, "a@x")
	f := newFetcher(t, []config.UsenetProvider{pa, pb}, Options{IdleReadTimeout: -1})
	fastHedge(f, 50*time.Millisecond)
	got, err := f.Body(context.Background(), "a@x", nil, core.PriorityLive)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("len=%d err=%v", len(got), err)
	}
	if n := b.BodyRequests("a@x"); n != 1 {
		t.Fatalf("hedge BODYs = %d", n)
	}
	if st := statsFor(f, "b"); st.HedgeWins != 0 || st.HedgeLosses != 1 || st.HedgeWastedBytes == 0 {
		t.Fatalf("corrupt hedge accounting: %+v", st)
	}
}

// When the primary answers first, the stalled hedge is cancelled, its
// connection closed, and the loss and wasted bytes recorded.
func TestHedgeLoserCancelledWhenPrimaryWins(t *testing.T) {
	want := randData(7, 200_000)
	a, b, pa, pb := twoTiers(t, "a@x", want, 2, 4)
	stallBody(a, 250*time.Millisecond, "a@x")
	stallBody(b, 30*time.Second, "a@x")
	f := newFetcher(t, []config.UsenetProvider{pa, pb}, Options{IdleReadTimeout: 10 * time.Second})
	fastHedge(f, 50*time.Millisecond)
	base := runtime.NumGoroutine()
	got, err := f.Body(context.Background(), "a@x", nil, core.PriorityLive)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("len=%d err=%v", len(got), err)
	}
	waitServerActive(t, b, 0)
	waitGoroutines(t, base, a, b)
	st := statsFor(f, "b")
	if st.Hedges != 1 || st.HedgeLosses != 1 || st.HedgeWins != 0 || st.HedgeWastedBytes == 0 {
		t.Fatalf("loser accounting: %+v", st)
	}
	if st.State != StateOK || st.LiveConnections != 0 {
		t.Fatalf("cancelled hedge penalised its provider or kept its socket: %+v", st)
	}
}

// A later live fetch completing while the head is still pending lowers the
// hedge delay to the overtaken floor.
func TestHedgeOvertakenTriggersEarly(t *testing.T) {
	a, b, pa, pb := twoTiers(t, "head@x", randData(8, 80_000), 2, 4)
	next := randData(9, 80_000)
	addSeg(t, a, "next@x", next)
	stallBody(a, 30*time.Second, "head@x")
	f := newFetcher(t, []config.UsenetProvider{pa, pb}, Options{IdleReadTimeout: 10 * time.Second})
	f.live.policy.floor, f.live.policy.cold = 2*time.Second, 2*time.Second
	f.live.policy.overtakenFloor = 50 * time.Millisecond

	done := make(chan time.Duration, 1)
	start := time.Now()
	go func() {
		if _, err := f.Body(context.Background(), "head@x", nil, core.PriorityLive); err != nil {
			t.Error(err)
		}
		done <- time.Since(start)
	}()
	time.Sleep(20 * time.Millisecond)
	if got, err := f.Body(context.Background(), "next@x", nil, core.PriorityLive); err != nil || !bytes.Equal(got, next) {
		t.Fatalf("next: len=%d err=%v", len(got), err)
	}
	if d := <-done; d > time.Second {
		t.Fatalf("overtaken head answered after %v; the 2s stall floor was not lowered", d)
	}
	if n := b.BodyRequests("head@x"); n != 1 {
		t.Fatalf("head hedges = %d", n)
	}
}

// The caller cancelling a hedged fetch cancels both sides.
func TestHedgeCallerCancel(t *testing.T) {
	want := randData(12, 100_000)
	a, b, pa, pb := twoTiers(t, "a@x", want, 2, 4)
	stallBody(a, 30*time.Second, "a@x")
	stallBody(b, 30*time.Second, "a@x")
	f := newFetcher(t, []config.UsenetProvider{pa, pb}, Options{IdleReadTimeout: -1})
	fastHedge(f, 30*time.Millisecond)
	base := runtime.NumGoroutine()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := f.Body(ctx, "a@x", nil, core.PriorityLive); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	waitServerActive(t, a, 0)
	waitServerActive(t, b, 0)
	waitGoroutines(t, base, a, b)
	if n := b.BodyRequests("a@x"); n != 1 {
		t.Fatalf("hedges = %d", n)
	}
}

// Time to first byte with one stalled provider: without hedging the viewer
// waits for the idle read deadline (5s by default, 1s here); with it, about
// the hedge threshold.
func TestHedgeCutsStalledTimeToFirstByte(t *testing.T) {
	measure := func(off bool) time.Duration {
		want := randData(13, 300_000)
		a, _, pa, pb := twoTiers(t, "a@x", want, 2, 4)
		stallBody(a, 30*time.Second, "a@x")
		f := newFetcher(t, []config.UsenetProvider{pa, pb}, Options{IdleReadTimeout: time.Second})
		fastHedge(f, 100*time.Millisecond)
		f.live.policy.off = off
		start := time.Now()
		got, err := f.Body(context.Background(), "a@x", nil, core.PriorityLive)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("off=%v: len=%d err=%v", off, len(got), err)
		}
		return time.Since(start)
	}
	unhedged, hedged := measure(true), measure(false)
	t.Logf("time to first byte with a stalled provider: unhedged %v, hedged %v", unhedged, hedged)
	if unhedged < time.Second {
		t.Fatalf("unhedged fetch beat the idle deadline: %v", unhedged)
	}
	if hedged > 600*time.Millisecond {
		t.Fatalf("hedged fetch took %v, want about the 100ms threshold", hedged)
	}
}

// BenchmarkHedgeStalledTTFB measures time to first byte against a provider
// stalled mid-body with production defaults (5s idle deadline, cold 1s
// hedge threshold). Run with -benchtime=1x.
func BenchmarkHedgeStalledTTFB(b *testing.B) {
	for _, mode := range []struct {
		name string
		off  bool
	}{{"unhedged", true}, {"hedged", false}} {
		b.Run(mode.name, func(b *testing.B) {
			want := randData(14, 700_000)
			sa := nntptest.Start(b, nntptest.Options{})
			sb := nntptest.Start(b, nntptest.Options{})
			pa, pb := sa.Provider("a", 4), sb.Provider("b", 4)
			pb.Priority = 1
			f := New([]config.UsenetProvider{pa, pb}, Options{Logger: quiet, MinIdle: -1})
			defer f.Close()
			f.live.policy.off = mode.off
			var total time.Duration
			n := 0
			for i := 0; b.Loop(); i++ {
				n++
				b.StopTimer()
				id := "bench" + string(rune('a'+i%26)) + "@x"
				sa.Remove(id)
				sb.Remove(id)
				opts := yenc.EncodeOptions{Name: "f.bin", Part: 1, Total: 1, FileSize: int64(len(want))}
				if _, err := sa.AddYenc(id, want, opts); err != nil {
					b.Fatal(err)
				}
				if _, err := sb.AddYenc(id, want, opts); err != nil {
					b.Fatal(err)
				}
				stallBody(sa, time.Minute, id)
				b.StartTimer()
				start := time.Now()
				if _, err := f.Body(context.Background(), id, nil, core.PriorityLive); err != nil {
					b.Fatal(err)
				}
				total += time.Since(start)
			}
			b.ReportMetric(float64(total.Milliseconds())/float64(n), "ttfb-ms")
		})
	}
}
