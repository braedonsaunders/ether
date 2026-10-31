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
		BackoffMin: time.Hour, FailThreshold: 2, MinIdle: -1,
	})
	if err := f.Warm(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	dials := statsFor(f, "p").Dials
	p := f.pools[0]
	p.brk.connFailure(errors.New("connect failed"))
	p.brk.connFailure(errors.New("connect failed"))
	if st := statsFor(f, "p"); st.State != StateBackoff {
		t.Fatalf("state %q, want backoff", st.State)
	}
	until := statsFor(f, "p").BackoffUntil

	ok, err := f.Stat(context.Background(), "a@x")
	if err != nil || !ok {
		t.Fatalf("STAT success during backoff: ok=%v err=%v", ok, err)
	}
	ok, err = f.Stat(context.Background(), "missing@x")
	if err != nil || ok {
		t.Fatalf("STAT 430 during backoff: ok=%v err=%v", ok, err)
	}
	st := statsFor(f, "p")
	if st.State != StateBackoff || !st.BackoffUntil.Equal(until) || st.Dials != dials {
		t.Fatalf("STAT reset or redialed the breaker: %+v (dials was %d, until %v)", st, dials, until)
	}

	got, err := f.Body(context.Background(), "a@x", nil, core.PriorityLive)
	if err != nil || string(got) != "alpha" {
		t.Fatalf("BODY during backoff: %q %v", got, err)
	}
	if st := statsFor(f, "p"); st.State != StateOK || st.Dials != dials {
		t.Fatalf("BODY success should clear the cooldown without a new dial: %+v", st)
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
	if after.State != StateBackoff || after.Dials != before.Dials {
		t.Fatalf("connect backoff dialed or cleared: before %+v after %+v", before, after)
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
