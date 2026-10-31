package usenet

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/usenet/nntptest"
)

func TestParseConnLimit(t *testing.T) {
	for msg, want := range map[string]int{
		"connection limit (50) reached":                       50,
		"Connection limit 8 exceeded":                         8,
		"too many connections (20)":                           20,
		"Max 30 connections per user":                         30,
		"maximum of 12 simultaneous connections reached":      12,
		"connections limit: 40":                               40,
		"account limited to 25 concurrent connections":        25,
		"10 connections allowed":                              10,
		"too many connections":                                0,
		"too many connections from 10.0.0.1":                  0,
		"service temporarily unavailable":                     0,
		"connection limit (0) reached":                        0,
		"you have exceeded the maximum number of connections": 0,
	} {
		if got := parseConnLimit(msg); got != want {
			t.Errorf("parseConnLimit(%q) = %d, want %d", msg, got, want)
		}
	}
}

func TestLimitCeilingHeadroom(t *testing.T) {
	for stated, want := range map[int]int{1: 1, 2: 1, 8: 7, 10: 9, 20: 18, 50: 45, 100: 90} {
		if got := limitCeiling(stated); got != want {
			t.Errorf("limitCeiling(%d) = %d, want %d", stated, got, want)
		}
	}
}

func refusal(text string) error {
	return &ProtocolError{Cmd: "greeting", Code: 502, Msg: text}
}

// A stated limit caps the pool at N minus headroom; later, lower statements
// lower it further, higher ones never raise it.
func TestStatedLimitCapsAndRatchetsDown(t *testing.T) {
	s := nntptest.Start(t, nntptest.Options{MaxConns: 5, RefuseText: "connection limit (5) reached", Latency: 20 * time.Millisecond})
	for i := 0; i < 16; i++ {
		addSeg(t, s, fmt.Sprintf("s%d@x", i), []byte("x"))
	}
	f := newFetcher(t, []config.UsenetProvider{s.Provider("p", 8)}, Options{limits: limitTiming{widen: time.Hour, refusalPause: 10 * time.Millisecond}})
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := f.Body(context.Background(), fmt.Sprintf("s%d@x", i), nil, core.PriorityLive); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	st := statsFor(f, "p")
	if st.ServerLimit != 5 || st.MaxConnections != 4 || !st.LimitLearned || st.State != StateOK {
		t.Fatalf("stated limit not learned as 5-1: %+v", st)
	}
	if s.Stats().MaxActive > 5 {
		t.Fatal("server limit exceeded")
	}

	p := poolFor(t, f, "p")
	p.mu.Lock()
	p.limit.lastClose = time.Time{} // closing sockets above the cap opened a teardown grace
	p.mu.Unlock()
	p.refused(refusal("connection limit (3) reached"), 2)
	if st := statsFor(f, "p"); st.ServerLimit != 3 || st.MaxConnections != 2 {
		t.Fatalf("lower stated limit not applied: %+v", st)
	}
	p.refused(refusal("connection limit (10) reached"), 2)
	if st := statsFor(f, "p"); st.ServerLimit != 3 || st.MaxConnections != 2 {
		t.Fatalf("ceiling rose: %+v", st)
	}
	// Widening an unknown-limit cap never passes the stated ceiling.
	p.mu.Lock()
	p.limit.cap = 1
	p.limit.widenAt = time.Time{}
	widened := p.widenLocked(time.Now())
	capAfter, eff := p.limit.cap, p.effectiveLocked()
	p.mu.Unlock()
	if !widened || capAfter != 0 || eff != 2 {
		t.Fatalf("widen: widened=%v cap=%d effective=%d", widened, capAfter, eff)
	}
}

// Without a stated limit the pool caps at what the server carried and
// widens one connection per interval, only on success.
func TestUnknownLimitWidensOnePerInterval(t *testing.T) {
	s := nntptest.Start(t, nntptest.Options{})
	f := newFetcher(t, []config.UsenetProvider{s.Provider("p", 6)}, Options{limits: limitTiming{widen: time.Hour}})
	p := poolFor(t, f, "p")
	if !p.refused(refusal("too many connections"), 2) {
		t.Fatal("refusal with live connections must be retried, not failed")
	}
	if st := statsFor(f, "p"); st.MaxConnections != 2 || !st.LimitLearned || st.ServerLimit != 0 {
		t.Fatalf("not capped at live: %+v", st)
	}
	now := time.Now()
	p.mu.Lock()
	early := p.widenLocked(now)
	p.mu.Unlock()
	if early {
		t.Fatal("widened before the interval passed")
	}
	for want := 3; want <= 6; want++ {
		now = now.Add(2 * time.Hour)
		p.mu.Lock()
		p.widenLocked(now)
		p.mu.Unlock()
		p.applySize()
		if got := statsFor(f, "p").MaxConnections; got != want {
			t.Fatalf("after step: %d connections, want %d", got, want)
		}
	}
	if st := statsFor(f, "p"); st.LimitLearned {
		t.Fatalf("still limited at the configured size: %+v", st)
	}
}

// A cancelled fetch closes its socket; a server still counting that socket
// refuses the immediate redial. That refusal is retried after a short pause
// rather than learned as a limit or counted against the breaker.
func TestTeardownGraceRetriesRefusal(t *testing.T) {
	run := func(t *testing.T, grace time.Duration) (ProviderStats, error) {
		s := nntptest.Start(t, nntptest.Options{MaxConns: 1, TeardownDelay: 100 * time.Millisecond})
		addSeg(t, s, "a@x", []byte("alpha"))
		addSeg(t, s, "slow@x", []byte("slow body line\r\nmore"))
		s.SetFault(func(cmd, arg string) nntptest.Fault {
			if cmd == "BODY" && arg == "slow@x" {
				return nntptest.Fault{BodyDelay: 5 * time.Second}
			}
			return nntptest.Fault{}
		})
		f := newFetcher(t, []config.UsenetProvider{s.Provider("p", 1)}, Options{
			BackoffMin: time.Minute, BackoffMax: time.Minute,
			limits: limitTiming{teardownGrace: grace, refusalPause: 120 * time.Millisecond},
		})
		if _, err := f.Body(context.Background(), "a@x", nil, core.PriorityLive); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		if _, err := f.Body(ctx, "slow@x", nil, core.PriorityLive); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("slow fetch: %v", err)
		}
		cancel()
		_, err := f.Body(context.Background(), "a@x", nil, core.PriorityLive)
		return statsFor(f, "p"), err
	}
	t.Run("grace", func(t *testing.T) {
		st, err := run(t, 2*time.Second)
		if err != nil {
			t.Fatalf("redial after cancel failed: %v", err)
		}
		if st.TeardownRefusals == 0 || st.LimitRefusals != 0 || st.LimitLearned || st.MaxConnections != 1 || st.State != StateOK || st.Errors != 0 {
			t.Fatalf("teardown refusal treated as a limit: %+v", st)
		}
	})
	t.Run("without grace", func(t *testing.T) {
		st, _ := run(t, time.Nanosecond)
		if st.TeardownRefusals != 0 || st.LimitRefusals == 0 || st.Errors == 0 {
			t.Fatalf("expected the refusal to count without the grace: %+v", st)
		}
	})
}

// Failed handshakes back off exponentially; a successful one resets it.
func TestHandshakeBackoff(t *testing.T) {
	s := nntptest.Start(t, nntptest.Options{})
	addSeg(t, s, "a@x", []byte("a"))
	var mu sync.Mutex
	var dials []time.Time
	failures := 3
	dialer := func(ctx context.Context, network, addr string) (net.Conn, error) {
		mu.Lock()
		dials = append(dials, time.Now())
		fail := len(dials) <= failures
		mu.Unlock()
		if fail {
			return nil, &net.OpError{Op: "dial", Net: network, Err: errors.New("connection reset")}
		}
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}
	f := newFetcher(t, []config.UsenetProvider{s.Provider("p", 2)}, Options{
		DialContext: dialer, FailThreshold: 100, Retries: 5,
		limits: limitTiming{backoffMin: 40 * time.Millisecond, backoffMax: 120 * time.Millisecond},
	})
	if _, err := f.Body(context.Background(), "a@x", nil, core.PriorityLive); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got := append([]time.Time(nil), dials...)
	mu.Unlock()
	if len(got) != 4 {
		t.Fatalf("%d dials, want 4", len(got))
	}
	for i, want := range []time.Duration{40 * time.Millisecond, 80 * time.Millisecond, 120 * time.Millisecond} {
		gap := got[i+1].Sub(got[i])
		if gap < want-5*time.Millisecond {
			t.Errorf("gap %d = %v, want at least %v", i, gap, want)
		}
		if gap > want+500*time.Millisecond {
			t.Errorf("gap %d = %v, far above %v (max not applied?)", i, gap, want)
		}
	}
	p := poolFor(t, f, "p")
	p.mu.Lock()
	fails, after := p.limit.dialFails, p.limit.dialAfter
	p.mu.Unlock()
	if fails != 0 || !after.IsZero() {
		t.Fatalf("success did not reset backoff: fails=%d after=%v", fails, after)
	}
}

// Cold dials are never paced: a burst on a fresh pool opens its connections
// in parallel.
func TestColdDialsAreParallel(t *testing.T) {
	s := nntptest.Start(t, nntptest.Options{Latency: 50 * time.Millisecond})
	for i := 0; i < 8; i++ {
		addSeg(t, s, fmt.Sprintf("s%d@x", i), []byte("x"))
	}
	var mu sync.Mutex
	var dials []time.Time
	dialer := func(ctx context.Context, network, addr string) (net.Conn, error) {
		mu.Lock()
		dials = append(dials, time.Now())
		mu.Unlock()
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}
	f := newFetcher(t, []config.UsenetProvider{s.Provider("p", 8)}, Options{DialContext: dialer, BodyPipelineDepth: 1})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := f.Body(context.Background(), fmt.Sprintf("s%d@x", i), nil, core.PriorityLive); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if len(dials) != 8 {
		t.Fatalf("%d dials, want 8", len(dials))
	}
	first, last := dials[0], dials[0]
	for _, d := range dials {
		first, last = minTime(first, d), maxTime(last, d)
	}
	if spread := last.Sub(first); spread >= defaultDialBackoff {
		t.Fatalf("cold dials spread over %v: paced", spread)
	}
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// Each socket Ether closed excuses at most two refusals; after that a
// refusal inside the grace window is a real limit.
func TestTeardownGraceIsBoundedByCloses(t *testing.T) {
	s := nntptest.Start(t, nntptest.Options{})
	f := newFetcher(t, []config.UsenetProvider{s.Provider("p", 6)}, Options{limits: limitTiming{widen: time.Hour, teardownGrace: time.Hour}})
	p := poolFor(t, f, "p")
	p.noteClose(1)
	for i := 0; i < 2; i++ {
		if !p.refused(refusal("too many connections"), 3) {
			t.Fatal("excused refusal not retried")
		}
	}
	if st := statsFor(f, "p"); st.TeardownRefusals != 2 || st.LimitRefusals != 0 || st.LimitLearned {
		t.Fatalf("first two refusals not excused: %+v", st)
	}
	p.refused(refusal("too many connections"), 3)
	if st := statsFor(f, "p"); st.TeardownRefusals != 2 || st.LimitRefusals != 1 || st.MaxConnections != 3 {
		t.Fatalf("third refusal should be learned: %+v", st)
	}
}
