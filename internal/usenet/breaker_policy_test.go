package usenet

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/usenet/nntptest"
)

func TestBreakerInitialCooldownIsOneSecond(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	b := newBreaker(DefaultBackoffMin, DefaultBackoffMax, DefaultFailThreshold, func() time.Time { return now })
	if d := b.connFailure(errors.New("dial")); d != 0 {
		t.Fatalf("first failure opened the breaker for %v", d)
	}
	if d := b.connFailure(errors.New("dial")); d != time.Second {
		t.Fatalf("initial cooldown %v, want 1s", d)
	}
}

func TestSTATDoesNotResetBreaker(t *testing.T) {
	s := nntptest.Start(t, nntptest.Options{})
	addSeg(t, s, "a@x", []byte("alpha"))
	f := newFetcher(t, []config.UsenetProvider{s.Provider("p", 2)}, Options{
		BackoffMin: 20 * time.Millisecond, BackoffMax: 20 * time.Millisecond,
		FailThreshold: 2, MinIdle: -1, Retries: -1,
	})
	if err := f.Warm(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	p := f.pools[0]
	p.brk.commandFailure(errors.New("502"))
	p.brk.commandFailure(errors.New("502"))
	if st := statsFor(f, "p"); st.State != StateBackoff || p.brk.cause != tripCommand {
		t.Fatalf("state %q cause %d", st.State, p.brk.cause)
	}
	// The command window is still open: STAT must not run, and must not clear it.
	if _, err := f.Stat(context.Background(), "a@x"); !errors.Is(err, ErrProviderBackoff) {
		t.Fatalf("STAT during command backoff: %v", err)
	}
	if st := statsFor(f, "p"); st.State != StateBackoff {
		t.Fatalf("STAT cleared command backoff: %+v", st)
	}
	time.Sleep(40 * time.Millisecond)
	ok, err := f.Stat(context.Background(), "a@x")
	if err != nil || !ok {
		t.Fatalf("STAT after the window: ok=%v err=%v", ok, err)
	}
	// One more command failure must reopen immediately. A cleared ladder
	// would still be under the threshold.
	if d := p.brk.commandFailure(errors.New("502")); d == 0 {
		t.Fatal("STAT success cleared the command ladder")
	}
	if st := statsFor(f, "p"); st.State != StateBackoff {
		t.Fatalf("state %q after the ladder stepped", st.State)
	}
}

func TestIdleSocketUsableDuringConnectBackoff(t *testing.T) {
	s := nntptest.Start(t, nntptest.Options{})
	addSeg(t, s, "a@x", []byte("alpha"))
	f := newFetcher(t, []config.UsenetProvider{s.Provider("p", 2)}, Options{
		BackoffMin: time.Hour, FailThreshold: 2, MinIdle: -1,
	})
	if err := f.Warm(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	p := f.pools[0]
	p.brk.connFailure(errors.New("connect failed"))
	p.brk.connFailure(errors.New("connect failed"))
	before := statsFor(f, "p")
	if before.State != StateBackoff || before.IdleConnections == 0 {
		t.Fatalf("setup %+v", before)
	}
	ok, err := f.Stat(context.Background(), "a@x")
	if err != nil || !ok {
		t.Fatalf("idle STAT: ok=%v err=%v", ok, err)
	}
	after := statsFor(f, "p")
	if after.Dials != before.Dials {
		t.Fatalf("connect backoff dialed: before %+v after %+v", before, after)
	}
	if after.State != StateOK {
		t.Fatalf("successful STAT should clear the connect ladder: %+v", after)
	}
}

func TestCommandBackoffSkipsIdleSockets(t *testing.T) {
	s := nntptest.Start(t, nntptest.Options{})
	addSeg(t, s, "a@x", []byte("alpha"))
	s.SetFault(func(cmd, _ string) nntptest.Fault {
		if cmd == "BODY" {
			return nntptest.Fault{Code: 502, Text: "unavailable"}
		}
		return nntptest.Fault{}
	})
	f := newFetcher(t, []config.UsenetProvider{s.Provider("p", 2)}, Options{
		BackoffMin: time.Hour, FailThreshold: 2, MinIdle: -1, Retries: -1,
	})
	if err := f.Warm(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Body(context.Background(), "a@x", nil, core.PriorityLive); err == nil {
		t.Fatal("first 502 succeeded")
	}
	if _, err := f.Body(context.Background(), "a@x", nil, core.PriorityLive); err == nil {
		t.Fatal("second 502 succeeded")
	}
	st := statsFor(f, "p")
	if st.State != StateBackoff || st.IdleConnections == 0 || f.pools[0].brk.cause != tripCommand {
		t.Fatalf("setup %+v cause %d", st, f.pools[0].brk.cause)
	}
	stats := s.Stats().Commands["STAT"]
	if _, err := f.Stat(context.Background(), "a@x"); !errors.Is(err, ErrProviderBackoff) {
		t.Fatalf("idle socket used during command backoff: %v", err)
	}
	if got := s.Stats().Commands["STAT"]; got != stats || statsFor(f, "p").Dials != st.Dials {
		t.Fatalf("provider contacted during command backoff: STAT %d->%d dials %+v", stats, got, statsFor(f, "p"))
	}
}

func TestProbeSTATReopensConcurrentDials(t *testing.T) {
	s := nntptest.Start(t, nntptest.Options{})
	for _, id := range []string{"a@x", "b@x", "c@x", "d@x"} {
		addSeg(t, s, id, []byte(id))
	}
	f := newFetcher(t, []config.UsenetProvider{s.Provider("p", 4)}, Options{
		BackoffMin: 20 * time.Millisecond, BackoffMax: 20 * time.Millisecond,
		FailThreshold: 2, MinIdle: -1, Retries: -1,
	})
	p := f.pools[0]
	p.brk.connFailure(errors.New("dial"))
	p.brk.connFailure(errors.New("dial"))
	time.Sleep(40 * time.Millisecond)
	if ok, err := f.Stat(context.Background(), "a@x"); err != nil || !ok {
		t.Fatalf("probe STAT: ok=%v err=%v", ok, err)
	}
	p.discardIdle()
	var wg sync.WaitGroup
	errCh := make(chan error, 3)
	for _, id := range []string{"b@x", "c@x", "d@x"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := f.Stat(context.Background(), id)
			if err != nil || !ok {
				errCh <- errors.New(id)
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
	if st := statsFor(f, "p"); st.State != StateOK {
		t.Fatalf("dials still serialized: %+v", st)
	}
}

func TestStaleReuseDoesNotTripBreaker(t *testing.T) {
	s := nntptest.Start(t, nntptest.Options{})
	addSeg(t, s, "a@x", []byte("alpha"))
	addSeg(t, s, "b@x", []byte("beta"))
	// Retries -1 is zero extra attempts. The stale socket must still be
	// retried once, and that retry must not be what keeps the breaker closed.
	f := newFetcher(t, []config.UsenetProvider{s.Provider("p", 4)}, Options{
		FailThreshold: 2, Retries: -1, MinIdle: -1, BackoffMin: time.Hour,
	})
	if err := f.Warm(context.Background(), 4); err != nil {
		t.Fatal(err)
	}
	s.DropConnections()
	var wg sync.WaitGroup
	errCh := make(chan error, 2)
	for _, id := range []string{"a@x", "b@x"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := f.Body(context.Background(), id, nil, core.PriorityLive)
			if err != nil {
				errCh <- err
				return
			}
			want := "alpha"
			if id == "b@x" {
				want = "beta"
			}
			if string(got) != want {
				errCh <- errors.New(id + " = " + string(got))
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
	if st := statsFor(f, "p"); st.State != StateOK {
		t.Fatalf("stale reused sockets tripped the breaker: %+v", st)
	}
}

func TestBackgroundStatUsesPrefetch(t *testing.T) {
	s := nntptest.Start(t, nntptest.Options{})
	addSeg(t, s, "a@x", []byte("alpha"))
	addSeg(t, s, "b@x", []byte("beta"))
	f := newFetcher(t, []config.UsenetProvider{s.Provider("p", 1)}, Options{MinIdle: -1})
	p := f.pools[0]
	held, err := p.get(context.Background(), core.PriorityLive)
	if err != nil {
		t.Fatal(err)
	}
	bg := core.WithBackgroundBudget(context.Background(), func(context.Context, string, string, int64) error { return nil })
	statDone := make(chan error, 1)
	go func() {
		_, err := p.stat(bg, "a@x")
		statDone <- err
	}()
	waitQueued(t, p.sem, core.PriorityPrefetch, 1)
	if n := p.sem.stats().Waiting[core.PriorityLive]; n != 0 {
		t.Fatalf("background STAT queued on the live lane: %d", n)
	}
	manyDone := make(chan error, 1)
	go func() {
		_, err := p.statMany(bg, []string{"a@x", "b@x"})
		manyDone <- err
	}()
	waitQueued(t, p.sem, core.PriorityPrefetch, 2)
	liveDone := make(chan error, 1)
	go func() {
		_, err := p.stat(context.Background(), "a@x")
		liveDone <- err
	}()
	waitQueued(t, p.sem, core.PriorityLive, 1)
	p.put(held, core.PriorityLive)
	for _, ch := range []chan error{liveDone, statDone, manyDone} {
		if err := <-ch; err != nil {
			t.Fatal(err)
		}
	}
}
