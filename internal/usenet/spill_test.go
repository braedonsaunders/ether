package usenet

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/usenet/nntptest"
)

// hold takes n slots of p at prio, as requests in flight would, and returns
// a release func.
func hold(t *testing.T, p *pool, prio core.Priority, n int) func() {
	t.Helper()
	for i := 0; i < n; i++ {
		if !p.sem.TryAcquire(prio) {
			t.Fatalf("could not hold slot %d of %s", i, p.cfg.ID)
		}
	}
	released := false
	return func() {
		if released {
			return
		}
		released = true
		for i := 0; i < n; i++ {
			p.sem.Release(prio)
		}
	}
}

// twoAccounts is a primary and a lower-priority secondary (plus a backfill
// block account when block is set), all holding article "a@x".
func twoAccounts(t *testing.T, primaryConns, secondaryConns int, block bool) (*Fetcher, map[string]*nntptest.Server) {
	t.Helper()
	servers := map[string]*nntptest.Server{}
	var providers []config.UsenetProvider
	add := func(id string, conns, prio int, backfill bool) {
		s := nntptest.Start(t, nntptest.Options{})
		addSeg(t, s, "a@x", []byte("alpha"))
		addSeg(t, s, "b@x", []byte("bravo"))
		servers[id] = s
		cfg := s.Provider(id, conns)
		cfg.Priority, cfg.Backfill = prio, backfill
		providers = append(providers, cfg)
	}
	add("primary", primaryConns, 0, false)
	add("secondary", secondaryConns, 1, false)
	if block {
		add("block", 4, 2, true)
	}
	return newFetcher(t, providers, Options{}), servers
}

func TestSpillOnlyWhenPrimarySaturated(t *testing.T) {
	f, srv := twoAccounts(t, 2, 2, false)
	ctx := context.Background()
	primary := poolFor(t, f, "primary")

	// Primary has a free slot: strict priority, no spill.
	release := hold(t, primary, core.PriorityLive, 1)
	if _, err := f.Body(ctx, "a@x", nil, core.PriorityLive); err != nil {
		t.Fatal(err)
	}
	if srv["secondary"].BodyRequests("a@x") != 0 || statsFor(f, "secondary").Spillovers != 0 {
		t.Fatal("spilled while the primary had capacity")
	}
	release()

	// Primary full: the live request goes to the idle secondary at once.
	release = hold(t, primary, core.PriorityLive, 2)
	defer release()
	tctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if _, err := f.Body(tctx, "b@x", nil, core.PriorityLive); err != nil {
		t.Fatalf("did not spill: %v", err)
	}
	if srv["secondary"].BodyRequests("b@x") != 1 || srv["primary"].BodyRequests("b@x") != 0 {
		t.Fatal("expected the secondary to serve b@x")
	}
	if st := statsFor(f, "secondary"); st.Spillovers != 1 {
		t.Fatalf("spillovers = %d", st.Spillovers)
	}
	// STAT spills the same way.
	if ok, err := f.Stat(tctx, "a@x"); err != nil || !ok {
		t.Fatalf("stat: %v %v", ok, err)
	}
	if res, err := f.StatMany(tctx, []string{"a@x", "b@x"}); err != nil || !res["a@x"] || !res["b@x"] {
		t.Fatalf("statmany: %v %v", res, err)
	}
	if n := srv["primary"].Stats().Commands["STAT"]; n != 0 {
		t.Fatalf("primary got %d STATs while saturated", n)
	}
	if st := statsFor(f, "secondary"); st.Spillovers != 3 {
		t.Fatalf("spillovers = %d, want 3", st.Spillovers)
	}
	if st := statsFor(f, "primary"); st.Spillovers != 0 {
		t.Fatalf("primary spillovers = %d", st.Spillovers)
	}
}

// A busy primary never pushes work onto a backfill (block) account: the
// request waits for the primary instead of spending block bytes.
func TestNoSpillOntoBackfill(t *testing.T) {
	s := nntptest.Start(t, nntptest.Options{})
	block := nntptest.Start(t, nntptest.Options{})
	addSeg(t, s, "a@x", []byte("alpha"))
	addSeg(t, block, "a@x", []byte("alpha"))
	bp := block.Provider("block", 4)
	bp.Backfill = true
	f := newFetcher(t, []config.UsenetProvider{s.Provider("primary", 1), bp}, Options{})
	primary := poolFor(t, f, "primary")
	release := hold(t, primary, core.PriorityLive, 1)
	done := make(chan error, 1)
	go func() {
		_, err := f.Body(context.Background(), "a@x", nil, core.PriorityLive)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("request finished while the primary was saturated: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if block.BodyRequests("a@x") != 0 || statsFor(f, "block").Spillovers != 0 {
		t.Fatal("spilled onto the backfill account")
	}
}

// Speculative work borrows a lower tier only while it is quiet: no viewer
// work there and at least half its slots free.
func TestSpeculativeSpillsOnlyOntoQuietSecondary(t *testing.T) {
	f, srv := twoAccounts(t, 2, 4, false)
	primary, secondary := poolFor(t, f, "primary"), poolFor(t, f, "secondary")
	release := hold(t, primary, core.PriorityLive, 2)
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := f.Body(ctx, "a@x", nil, core.PriorityReadahead); err != nil {
		t.Fatal(err)
	}
	if srv["secondary"].BodyRequests("a@x") != 1 {
		t.Fatal("readahead did not borrow the idle secondary")
	}

	// A viewer on the secondary: readahead now waits for the primary.
	releaseSecondary := hold(t, secondary, core.PriorityLive, 1)
	defer releaseSecondary()
	done := make(chan error, 1)
	go func() {
		_, err := f.Body(context.Background(), "b@x", nil, core.PriorityReadahead)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("readahead finished while the primary was saturated: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if srv["secondary"].BodyRequests("b@x") != 0 || srv["primary"].BodyRequests("b@x") != 1 {
		t.Fatal("readahead spilled onto a secondary serving viewers")
	}
	// A live request still spills onto it.
	defer hold(t, primary, core.PriorityLive, 2)()
	if _, err := f.Body(ctx, "b@x", nil, core.PriorityLive); err != nil {
		t.Fatal(err)
	}
	if srv["secondary"].BodyRequests("b@x") != 1 {
		t.Fatal("live request did not spill")
	}
}

// Equally loaded providers in a tier go to the one with the lower expected
// delivery time (measured fetch EWMA); exact ties keep rotating.
func TestTierTieBreakByFetchTime(t *testing.T) {
	a := nntptest.Start(t, nntptest.Options{})
	b := nntptest.Start(t, nntptest.Options{})
	f := newFetcher(t, []config.UsenetProvider{a.Provider("a", 4), b.Provider("b", 4)}, Options{})
	pa, pb := poolFor(t, f, "a"), poolFor(t, f, "b")
	firsts := map[string]int{}
	for i := 0; i < 6; i++ {
		firsts[f.order()[0].cfg.ID]++
	}
	if firsts["a"] == 0 || firsts["b"] == 0 {
		t.Fatalf("unmeasured equal providers did not rotate: %v", firsts)
	}
	pa.latMu.Lock()
	pa.fetching = ewma{v: float64(200 * time.Millisecond), n: 10}
	pa.latMu.Unlock()
	pb.latMu.Lock()
	pb.fetching = ewma{v: float64(20 * time.Millisecond), n: 10}
	pb.latMu.Unlock()
	for i := 0; i < 6; i++ {
		if got := f.order()[0].cfg.ID; got != "b" {
			t.Fatalf("order %d starts with %s, want the faster b", i, got)
		}
	}
	// Load still comes first: a busier fast provider yields.
	release := hold(t, pb, core.PriorityLive, 1)
	defer release()
	if got := f.order()[0].cfg.ID; got != "a" {
		t.Fatalf("order starts with %s, want the idle a", got)
	}
}

// Among several lower tiers the spill goes to the highest-priority one with
// room, and a saturated secondary passes it on.
func TestSpillPicksNextTierWithRoom(t *testing.T) {
	var providers []config.UsenetProvider
	servers := map[string]*nntptest.Server{}
	for i, id := range []string{"p0", "p1", "p2"} {
		s := nntptest.Start(t, nntptest.Options{})
		addSeg(t, s, "a@x", []byte("alpha"))
		servers[id] = s
		cfg := s.Provider(id, 1)
		cfg.Priority = i
		providers = append(providers, cfg)
	}
	f := newFetcher(t, providers, Options{})
	defer hold(t, poolFor(t, f, "p0"), core.PriorityLive, 1)()
	defer hold(t, poolFor(t, f, "p1"), core.PriorityLive, 1)()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := f.Body(ctx, "a@x", nil, core.PriorityLive); err != nil {
		t.Fatal(err)
	}
	if servers["p2"].BodyRequests("a@x") != 1 || statsFor(f, "p2").Spillovers != 1 {
		t.Fatal("expected the third tier to serve")
	}
}

// A live request for an article already being transmitted on the saturated
// primary joins that transfer instead of fetching a second copy elsewhere.
func TestNoSpillAwayFromJoinableFlight(t *testing.T) {
	f, _ := twoAccounts(t, 1, 2, false)
	primary := poolFor(t, f, "primary")
	defer hold(t, primary, core.PriorityLive, 1)()
	req := routeReq{prio: core.PriorityLive, joinable: func(p *pool) bool { return p.joinable("a@x", nil, core.PriorityLive) }}
	if _, spill := f.route(context.Background(), req, nil); spill == nil || spill.cfg.ID != "secondary" {
		t.Fatalf("expected a spill with nothing in flight, got %v", spill)
	}
	fctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	key := primary.bodyKeyFor("a@x", nil, core.PriorityReadahead)
	primary.bodyMu.Lock()
	primary.bodyFlights[key] = &bodyFlight{key: key, ctx: fctx, cancel: cancel, done: make(chan struct{}), started: true}
	primary.bodyMu.Unlock()
	defer func() {
		primary.bodyMu.Lock()
		delete(primary.bodyFlights, key)
		primary.bodyMu.Unlock()
	}()
	pools, spill := f.route(context.Background(), req, nil)
	if spill != nil || pools[0] != primary {
		t.Fatalf("spilled away from a transmitted flight: first=%s spill=%v", pools[0].cfg.ID, spill)
	}
}
