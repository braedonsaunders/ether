package debrid

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// The tests here pin the degraded route's bounded hunt for a route that
// flows: it rides block deliveries (a steady dribble never trips the
// zero-byte stall detector, so nothing else would re-open recovery), it
// runs off the delivery path (a hunt must never hold up playback), it
// visits each candidate at most once per exploration (explicit progress,
// not cooldown luck), it settles after a fixed budget (no background
// probes forever), and every adoption still answers to the live parent
// context, the current payload generation and a stream that may close
// mid-flight.

// degradedAdoption drives wave one over a dribbler so the file stands on
// a degraded route with a hunt budget, as everything here starts there.
func degradedAdoption(t *testing.T, e *Engine, f *remoteFile, raw []byte, failed string, usable func(string) bool, drip *mediaServer, off int64) {
	t.Helper()
	tok := payloadToken(t, raw)
	drip.issue(tok)
	for _, o := range []int64{off, 0} { // both probe offsets: replay probes off, discovery probes 0
		drip.drip[tok+"|"+probeRangeAt(o)] = mediaDrip{chunk: 6 << 10, pause: 100 * time.Millisecond}
	}
	e.recent.record("torbox-0", strings.TrimPrefix(drip.URL, "http://"), time.Now().Add(-10*time.Second))
	moved, replayed := f.resumeOnAnotherEdge(context.Background(), failed, usable, off)
	if !moved || !replayed {
		t.Fatalf("setup: wave one did not adopt the degraded route (%v/%v)", moved, replayed)
	}
	f.mu.Lock()
	deg, left := f.routeDegraded, f.discoverLeft
	f.mu.Unlock()
	if !deg || left != routeDiscoveryWaves {
		t.Fatalf("setup: routeDegraded=%v discoverLeft=%d, want true/%d", deg, left, routeDiscoveryWaves)
	}
}

func readBlock(t *testing.T, src interface {
	ReadAtContext(ctx context.Context, p []byte, off int64) (int, error)
}, buf []byte, off int64) {
	t.Helper()
	if _, err := src.ReadAtContext(context.Background(), buf, off); err != nil {
		t.Fatalf("read at %d: %v", off, err)
	}
}

func TestDiscoveryPromotesDegradedRoute(t *testing.T) {
	e, f, raw, failed, usable, src := rerouteWaveFixture(t, "promote")
	const off = int64(1 << 20)
	d0 := newMediaServer(t, 4<<20)
	degradedAdoption(t, e, f, raw, failed, usable, d0, off)
	rc := src.(readAtContexter)
	tok := payloadToken(t, raw)
	strict := newMediaServer(t, 4<<20) // joins after the adoption, token live at once
	strict.issue(tok)
	e.recent.record("torbox-0", strings.TrimPrefix(strict.URL, "http://"), time.Now().Add(-time.Minute))

	buf := make([]byte, 4096)
	readBlock(t, rc, buf, 0) // block 0 on the degraded route: delivery is the metronome
	deadline := time.Now().Add(10 * time.Second)
	for urlHost(mustPayloadURL(f)) != edgeHost(t, strict) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := urlHost(mustPayloadURL(f)); got != edgeHost(t, strict) {
		t.Fatalf("hunt left the file on %s, want promotion to the flowing edge %s", got, edgeHost(t, strict))
	}
	f.mu.Lock()
	deg := f.routeDegraded
	f.mu.Unlock()
	if deg {
		t.Fatal("route still marked degraded after promoting on flow proof")
	}
	// After promotion deliveries must cost no probes: the hunt is over.
	time.Sleep(300 * time.Millisecond) // let any in-flight delivery of block 0 land first
	nStrict, nD0 := strict.requests.Load(), d0.requests.Load()
	readBlock(t, rc, buf, 1<<20) // a block the first read's fetch window cannot have covered
	time.Sleep(200 * time.Millisecond)
	if got := d0.requests.Load(); got != nD0 {
		t.Fatalf("dribbled edge got %d requests after promotion, want 0 more: no probe, no return", got-nD0)
	}
	if got := strict.requests.Load(); got != nStrict+1 {
		t.Fatalf("flowing edge requests=%d, want exactly one more (the block read, not a probe)", got-nStrict)
	}
}

func TestDiscoverySettlesWithoutAFlowingRoute(t *testing.T) {
	e, f, raw, failed, usable, src := rerouteWaveFixture(t, "settle")
	const off = int64(1 << 20)
	d0, d1 := newMediaServer(t, 4<<20), newMediaServer(t, 4<<20)
	tok := payloadToken(t, raw)
	degradedAdoption(t, e, f, raw, failed, usable, d0, off)
	d1.issue(tok)
	for _, o := range []int64{off, 0} {
		d1.drip[tok+"|"+probeRangeAt(o)] = mediaDrip{chunk: 6 << 10, pause: 100 * time.Millisecond}
	}
	e.recent.record("torbox-0", strings.TrimPrefix(d1.URL, "http://"), time.Now().Add(-time.Minute))

	// Deliver NEW blocks until the hunt's wave budget is spent: only a
	// delivered block kicks (cached blocks never deliver), and each hunt
	// wave may probe only unvisited candidates, so exploration is finite
	// and explicit, not cooldown luck across overlapping waves.
	rc := src.(readAtContexter)
	buf := make([]byte, 4096)
	deadline := time.Now().Add(30 * time.Second)
	next := int64(0)
	for it := 0; ; it++ {
		readBlock(t, rc, buf, next)
		next += 64 << 10
		f.mu.Lock()
		left := f.discoverLeft
		f.mu.Unlock()
		if left == 0 || time.Now().After(deadline) {
			break
		}
		// One block every ~400 ms: the file's fresh blocks must outlast the
		// paced hunt's windows, so settling waits on spacing, not EOF.
		time.Sleep(300 * time.Millisecond)
	}
	f.mu.Lock()
	left, deg := f.discoverLeft, f.routeDegraded
	f.mu.Unlock()
	if left != 0 {
		t.Fatalf("discoverLeft=%d after 30 s of deliveries, want 0: the hunt must settle", left)
	}
	if !deg {
		t.Fatal("settling must leave the route marked degraded, not silently approved")
	}
	// Settled: deliveries drive no further background probes, and the
	// visited candidate was probed exactly once per exploration.
	if got := d1.requests.Load(); got != 1 {
		t.Fatalf("visited candidate requests=%d, want exactly 1 (hunt once): "+
			"one probe per candidate per exploration, and later waves must re-skip it", got)
	}
	before := d1.requests.Load()
	// Between the last wave's consume and the loop's exit, deliveries kept
	// kicking a spent hunt and the visited edge never saw them; these last
	// reads run from cache (the file is fetched out by now), checking that
	// quiescence also holds with no kicks at all.
	next = 3 << 20
	for i := 0; i < 3; i++ {
		readBlock(t, rc, buf, next)
		next += 64 << 10
	}
	time.Sleep(500 * time.Millisecond)
	if got := d1.requests.Load(); got != before {
		t.Fatalf("after settling the edge saw %d more requests, want 0: no background probes past the budget", got-before)
	}
}

func TestDiscoveryAbandonsStaleGenerationVerdict(t *testing.T) {
	e, f, raw, failed, usable, src := rerouteWaveFixture(t, "stale-hunt")
	const off = int64(1 << 20)
	d0 := newMediaServer(t, 4<<20)
	degradedAdoption(t, e, f, raw, failed, usable, d0, off)
	rc := src.(readAtContexter)
	tok := payloadToken(t, raw)
	strict := newMediaServer(t, 4<<20)
	strict.issue(tok)
	hold := make(chan struct{})
	strict.gate[tok+"|"+probeRangeAt(0)] = hold // park the hunt's probe mid-flight
	e.recent.record("torbox-0", strings.TrimPrefix(strict.URL, "http://"), time.Now().Add(-time.Minute))

	buf := make([]byte, 4096)
	readBlock(t, rc, buf, 0) // kicks the hunt, which parks on the gate
	if !waitUntil(5*time.Second, func() bool { return strict.requests.Load() > 0 }) {
		t.Fatal("the hunt never reached the gated candidate")
	}
	// The provider hands the file a new link mid-hunt: the hunt's
	// approved 206 is now about a token the file no longer carries.
	p, _ := f.payload()
	q := p
	q.URL = "http://127.0.0.1:1/adopted"
	q.Expires = q.Expires.Add(time.Hour)
	f.adopt(q)
	close(hold) // the stale probe answers NOW
	time.Sleep(300 * time.Millisecond)
	if got := urlHost(mustPayloadURL(f)); got != "127.0.0.1:1" {
		t.Fatalf("payload host after release=%s, want the adopt to stand: a stale verdict must not adopt", got)
	}
	f.mu.Lock()
	deg, left := f.routeDegraded, f.discoverLeft
	f.mu.Unlock()
	if deg {
		t.Fatal("the refreshed payload still marked degraded: adopt answers for its own route")
	}
	if left != routeDiscoveryWaves {
		t.Fatalf("discoverLeft=%d, want the budget untouched: a stale hunt wave costs nothing", left)
	}
}

func TestDiscoveryCancelOnStreamClose(t *testing.T) {
	e, f, raw, failed, usable, src := rerouteWaveFixture(t, "close-hunt")
	const off = int64(1 << 20)
	d0 := newMediaServer(t, 4<<20)
	degradedAdoption(t, e, f, raw, failed, usable, d0, off)
	rc := src.(readAtContexter)
	tok := payloadToken(t, raw)
	strict := newMediaServer(t, 4<<20)
	strict.issue(tok)
	hold := make(chan struct{})
	strict.gate[tok+"|"+probeRangeAt(0)] = hold
	e.recent.record("torbox-0", strings.TrimPrefix(strict.URL, "http://"), time.Now().Add(-time.Minute))
	buf := make([]byte, 4096)
	readBlock(t, rc, buf, 0)
	if !waitUntil(5*time.Second, func() bool { return strict.requests.Load() > 0 }) {
		t.Fatal("the hunt never reached the gated candidate")
	}
	if err := src.Close(); err != nil { // stream close: the parked hunt must die at it
		t.Fatal(err)
	}
	close(hold)
	time.Sleep(300 * time.Millisecond)
	f.mu.Lock()
	alive := f.discovering
	f.mu.Unlock()
	if alive {
		t.Fatal("the hunt outlived the stream: no flight may survive stream close")
	}
	if got := urlHost(mustPayloadURL(f)); got != edgeHost(t, d0) {
		t.Fatalf("payload moved to %s after close: a cancelled hunt adopts nothing", got)
	}
}

func TestDiscoveryNeverBlocksDelivery(t *testing.T) {
	e, f, raw, failed, usable, src := rerouteWaveFixture(t, "no-block")
	const off = int64(1 << 20)
	d0 := newMediaServer(t, 4<<20)
	degradedAdoption(t, e, f, raw, failed, usable, d0, off)
	rc := src.(readAtContexter)
	tok := payloadToken(t, raw)
	strict := newMediaServer(t, 4<<20)
	strict.issue(tok)
	hold := make(chan struct{})
	defer close(hold)
	strict.gate[tok+"|"+probeRangeAt(0)] = hold // park the hunt for longer than the probe budget
	e.recent.record("torbox-0", strings.TrimPrefix(strict.URL, "http://"), time.Now().Add(-time.Minute))

	buf := make([]byte, 4096)
	readBlock(t, rc, buf, 0) // kick
	if !waitUntil(5*time.Second, func() bool { return strict.requests.Load() > 0 }) {
		t.Fatal("the hunt never reached the gated candidate")
	}
	start := time.Now()
	readBlock(t, rc, buf, 1<<16)
	if spent := time.Since(start); spent > time.Second {
		t.Fatalf("a block took %s while a hunt wave parked on a gated probe: discovery must run off the delivery path", spent)
	}
}

func waitUntil(d time.Duration, ok func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if ok() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func mustPayloadURL(f *remoteFile) string {
	p, _ := f.payload()
	return p.URL
}

// The fourth review round's lifecycle findings, red first.

// Close ends the hunt ACTIVELY: the parked probe dies with the exploration
// context and the wave never reaches the candidate behind it — proven while
// the gate stays closed, so only cancellation can end the wave early.
func TestDiscoveryCancelIsProactive(t *testing.T) {
	e, f, raw, failed, usable, src := rerouteWaveFixture(t, "cancel-proactive")
	const off = int64(1 << 20)
	d0 := newMediaServer(t, 4<<20)
	degradedAdoption(t, e, f, raw, failed, usable, d0, off)
	rc := src.(readAtContexter)
	tok := payloadToken(t, raw)
	parked, next := newMediaServer(t, 4<<20), newMediaServer(t, 4<<20)
	parked.issue(tok)
	hold := make(chan struct{})
	parked.gate[tok+"|"+probeRangeAt(0)] = hold // outlives the probe budget: the probe sits, then dies on its own
	defer close(hold)
	recordFreshestFirst(e, []*mediaServer{parked, next}) // next answers 403 instantly when asked
	buf := make([]byte, 4096)
	nextOff := int64(512 << 10)
	deadline := time.Now().Add(10 * time.Second)
	for parked.requests.Load() == 0 && time.Now().Before(deadline) {
		readBlock(t, rc, buf, nextOff)
		nextOff += 64 << 10 // one fresh block per beat: deliveries are the metronome
		time.Sleep(50 * time.Millisecond)
	}
	if parked.requests.Load() == 0 {
		t.Fatal("the hunt never probed the freshest candidate")
	}
	src.Close() // gate still closed: only a cancelled exploration ends this wave before its next probe
	if !waitUntil(1*time.Second, func() bool { return parked.clientCancels.Load() >= 1 }) {
		t.Fatal("stream close left the parked request alive: close must cancel it at once, not merely block adoption")
	}
	time.Sleep(4 * time.Second) // a full probe round: the wave must also reach no further candidate
	if got := next.requests.Load(); got != 0 {
		t.Fatalf("the candidate behind the parked probe was probed after stream close: requests=%d, want 0 — close must cancel the exploration, not merely refuse adoption", got)
	}
}

// A retained file, closed and reopened on the same release ID, is reused
// from the cache: the old stream's end must not poison the hunt forever.
func TestDiscoveryResumesAfterReopen(t *testing.T) {
	e, f, raw, failed, usable, src := rerouteWaveFixture(t, "reopen")
	const off = int64(1 << 20)
	d0 := newMediaServer(t, 4<<20)
	degradedAdoption(t, e, f, raw, failed, usable, d0, off)
	tok := payloadToken(t, raw)
	if err := src.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	reopened, err := e.Open(context.Background(), f.id, raw)
	if err != nil {
		t.Fatalf("reopen of the retained file failed: %v", err)
	}
	defer reopened.Close()
	healthy := newMediaServer(t, 4<<20)
	healthy.issue(tok)
	e.recent.record("torbox-0", strings.TrimPrefix(healthy.URL, "http://"), time.Now().Add(-time.Minute))
	buf := make([]byte, 4096)
	nextOff := int64(512 << 10)
	deadline := time.Now().Add(10 * time.Second)
	for urlHost(mustPayloadURL(f)) != edgeHost(t, healthy) && time.Now().Before(deadline) {
		readBlock(t, reopened.(readAtContexter), buf, nextOff)
		if nextOff += 64 << 10; nextOff+4096 >= 4<<20 {
			nextOff = 512 << 10 // cycle: cached beats stop kicking, but the loop must survive to the deadline
		}
		time.Sleep(50 * time.Millisecond)
	}
	if healthy.requests.Load() == 0 {
		t.Fatal("the reopened cached file never hunted again: the closed flag outlived the stream that set it")
	}
	if got := urlHost(mustPayloadURL(f)); got != edgeHost(t, healthy) {
		t.Fatalf("hunt left the file on %s, want promotion to %s", got, edgeHost(t, healthy))
	}
}

// Wave starts pace like the replay wave's: block completions may drive at
// most one wave per spacing, never a drumbeat. The timing of one probe —
// not its count — is the assertion.
func TestDiscoveryWavesArePaced(t *testing.T) {
	e, f, raw, failed, usable, src := rerouteWaveFixture(t, "paced")
	const off = int64(1 << 20)
	d0 := newMediaServer(t, 4<<20)
	degradedAdoption(t, e, f, raw, failed, usable, d0, off)
	rc := src.(readAtContexter)
	tok := payloadToken(t, raw)
	first := newMediaServer(t, 4<<20) // no token: wave 1 probes it and is refused
	recordFreshestFirst(e, []*mediaServer{first})
	buf := make([]byte, 4096)
	nextOff := int64(512 << 10)
	deadline := time.Now().Add(12 * time.Second)
	for first.requests.Load() == 0 && time.Now().Before(deadline) {
		readBlock(t, rc, buf, nextOff)
		nextOff += 64 << 10
		time.Sleep(50 * time.Millisecond)
	}
	if first.requests.Load() == 0 {
		t.Fatal("wave 1 never ran")
	}
	wave1 := time.Now()
	later := newMediaServer(t, 4<<20) // joins after wave 1; wave 2 promotes on it
	later.issue(tok)
	recordFreshestFirst(e, []*mediaServer{first, later})
	var probedAt time.Time
	deadline = time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		readBlock(t, rc, buf, nextOff)
		nextOff += 64 << 10 // keep delivering fresh blocks: the kicks must come from real completions
		if later.requests.Load() > 0 && probedAt.IsZero() {
			probedAt = time.Now()
		}
		if urlHost(mustPayloadURL(f)) == edgeHost(t, later) {
			break
		}
		if nextOff+4096 >= 4<<20 {
			nextOff = 512 << 10             // cycle past the fresh-block set; the pacing window outlasts it
			time.Sleep(3 * time.Second / 2) // slow the cached beats so the deadline stays long
		}
		time.Sleep(150 * time.Millisecond)
	}
	// Promotion may race the next read, which legitimately fetches media
	// from the new edge. Count discovery ranges, not all HTTP requests.
	later.mu.Lock()
	probes := 0
	for _, r := range later.ranges {
		if r == probeRangeAt(0) {
			probes++
		}
	}
	later.mu.Unlock()
	if probes != 1 {
		t.Fatalf("late candidate probed %d times, want exactly 1", probes)
	}
	if got := urlHost(mustPayloadURL(f)); got != edgeHost(t, later) {
		t.Fatalf("hunt left the file on %s, want promotion to %s", got, edgeHost(t, later))
	}
	if gap := probedAt.Sub(wave1); gap < 4*time.Second {
		t.Fatalf("wave 2 probed %v after wave 1, want at least the %v spacing: block completions must not drumbeat the hunt", gap, rerouteSpacing)
	}
}

// Release and acquire are lifecycle transitions on the same state: hammer
// them concurrently (under -race) and the reused file must remain huntable.
func TestDiscoverySurvivesConcurrentReleaseAcquire(t *testing.T) {
	e, f, raw, failed, usable, src := rerouteWaveFixture(t, "race")
	const off = int64(1 << 20)
	d0 := newMediaServer(t, 4<<20)
	degradedAdoption(t, e, f, raw, failed, usable, d0, off)
	src.Close()
	var wg, rd sync.WaitGroup
	stop := make(chan struct{})
	rd.Add(1)
	go func() { // deliveries drive kicks AT the transitions: without this overlap, a -race pass says nothing about the revive stamp
		defer rd.Done()
		buf := make([]byte, 4096)
		nextOff := int64(512 << 10)
		for {
			select {
			case <-stop:
				return
			default:
			}
			s, err := e.Open(context.Background(), f.id, raw)
			if err != nil {
				return // churn may reject a beat; the hammer below judges the outcome
			}
			readBlock(t, s.(readAtContexter), buf, nextOff)
			if nextOff += 64 << 10; nextOff+4096 >= 4<<20 {
				nextOff = 512 << 10
			}
			s.Close()
		}
	}()
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				s, err := e.Open(context.Background(), f.id, raw)
				if err != nil {
					t.Errorf("concurrent open: %v", err)
					return
				}
				s.Close()
			}
		}()
	}
	wg.Wait()
	close(stop)
	rd.Wait()
	// Everyone released legitimately: the last Close closed the retained
	// file. The transition under test is the reuse side — reopening must
	// finish what release started, under the same lock that serialises them.
	s, err := e.Open(context.Background(), f.id, raw)
	if err != nil {
		t.Fatalf("open after the race: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	f.mu.Lock()
	closed := f.closed
	f.mu.Unlock()
	if closed {
		t.Fatal("the reused file kept the closed flag: acquireFile must finish the lifecycle transition it starts")
	}
	// The races legitimately spent hunt budget and may have refreshed or
	// re-adopted the payload — each of which retires the degraded state by
	// design; hand the file a fresh exploration so the assertion below is
	// about huntable-ness, not about who consumed the budget. The kept-open
	// stream stays open through the drive: a hunt must stay alive while a
	// stream holds the file, and a Close here would end it legitimately.
	f.mu.Lock()
	f.routeDegraded, f.discoverLeft, f.discovered = true, routeDiscoveryWaves, map[string]bool{}
	f.replayAt = time.Time{}
	f.mu.Unlock()
	tok := payloadToken(t, raw)
	healthy := newMediaServer(t, 4<<20)
	healthy.issue(tok)
	e.recent.record("torbox-0", strings.TrimPrefix(healthy.URL, "http://"), time.Now().Add(-time.Minute))
	buf := make([]byte, 4096)
	nextOff := int64(512 << 10)
	deadline := time.Now().Add(12 * time.Second)
	for urlHost(mustPayloadURL(f)) != edgeHost(t, healthy) && time.Now().Before(deadline) {
		readBlock(t, s.(readAtContexter), buf, nextOff)
		if nextOff += 64 << 10; nextOff+4096 >= 4<<20 {
			nextOff = 512 << 10 // cycle, as in the paced test: survive the deadline, kick on fresh blocks first
		}
		time.Sleep(50 * time.Millisecond)
	}
	if healthy.requests.Load() == 0 {
		t.Fatal("no hunt after concurrent release/acquire churn: the cached file refused its kicks")
	}
}

// Generation replacement comes from more than adopt: a link refresh and a
// provider move both retire the degraded generation, and an in-flight hunt
// must die at each — proven server-side, gate still held, so only client
// cancellation can end the parked request.
func TestDiscoveryRefreshEndsParkedProbe(t *testing.T) {
	e, f, raw, failed, usable, src := rerouteWaveFixture(t, "refresh-cancel")
	const off = int64(1 << 20)
	d0 := newMediaServer(t, 4<<20)
	degradedAdoption(t, e, f, raw, failed, usable, d0, off)
	rc := src.(readAtContexter)
	tok := payloadToken(t, raw)
	parked, next := newMediaServer(t, 4<<20), newMediaServer(t, 4<<20) // next never issues: instant 403 when asked
	parked.issue(tok)
	hold := make(chan struct{})
	parked.gate[tok+"|"+probeRangeAt(0)] = hold
	defer close(hold) // released after the assertions; nothing in this test opens it
	recordFreshestFirst(e, []*mediaServer{parked, next})
	buf := make([]byte, 4096)
	nextOff := int64(512 << 10)
	deadline := time.Now().Add(10 * time.Second)
	for parked.requests.Load() == 0 && time.Now().Before(deadline) {
		readBlock(t, rc, buf, nextOff)
		nextOff += 64 << 10
		time.Sleep(50 * time.Millisecond)
	}
	if parked.requests.Load() == 0 {
		t.Fatal("the hunt never parked a probe on the gated candidate")
	}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": "http://refresh.invalid/link"})
	}))
	defer api.Close()
	e.svc.entryByID("torbox-0").p = fixtureTorBox(api.URL) // the API is live for this transition, approvals unchanged
	if _, _, err := f.link(context.Background(), true); err != nil {
		t.Fatalf("forced refresh: %v", err)
	}
	if !waitUntil(1*time.Second, func() bool { return parked.clientCancels.Load() >= 1 }) {
		t.Fatal("a link refresh left the parked hunt alive: refresh must end the old exploration at once")
	}
	time.Sleep(3 * time.Second) // past a full probe budget: no next candidate may be asked on the replacement payload
	if got := next.requests.Load(); got != 0 {
		t.Fatalf("next candidate probed %d times after the generation was replaced, want 0", got)
	}
	f.mu.Lock()
	left := f.discoverLeft
	f.mu.Unlock()
	if left != routeDiscoveryWaves {
		t.Fatalf("a cancelled exploration spent %d of the discovery budget, want it unspent", routeDiscoveryWaves-left)
	}
}

// The provider move replaces the generation on another provider's evidence:
// the retired hunt follows it, same proof shape.
func TestDiscoveryProviderMoveEndsParkedProbe(t *testing.T) {
	h := newHarness(t, 4<<20, "torbox", "realdebrid")
	e, f, raw, failed, usable, src := rerouteWaveFixtureOn(t, h, "move-cancel")
	const off = int64(1 << 20)
	d0 := newMediaServer(t, 4<<20)
	degradedAdoption(t, e, f, raw, failed, usable, d0, off)
	rc := src.(readAtContexter)
	tok := payloadToken(t, raw)
	parked, next := newMediaServer(t, 4<<20), newMediaServer(t, 4<<20)
	parked.issue(tok)
	hold := make(chan struct{})
	parked.gate[tok+"|"+probeRangeAt(0)] = hold
	defer close(hold)
	recordFreshestFirst(e, []*mediaServer{parked, next})
	buf := make([]byte, 4096)
	nextOff := int64(512 << 10)
	deadline := time.Now().Add(10 * time.Second)
	for parked.requests.Load() == 0 && time.Now().Before(deadline) {
		readBlock(t, rc, buf, nextOff)
		nextOff += 64 << 10
		time.Sleep(50 * time.Millisecond)
	}
	if parked.requests.Load() == 0 {
		t.Fatal("the hunt never parked a probe on the gated candidate")
	}
	rd := h.provs["realdebrid-1"]
	rd.mu.Lock()
	rd.cached[testHash] = true
	rd.mu.Unlock()
	_, g := f.payload()
	if !f.moveFromHost(context.Background(), g) {
		t.Fatal("setup: the alternate provider refused the move")
	}
	if !waitUntil(1*time.Second, func() bool { return parked.clientCancels.Load() >= 1 }) {
		t.Fatal("a provider move left the parked hunt alive: moving providers must end the old exploration at once")
	}
	time.Sleep(3 * time.Second)
	if got := next.requests.Load(); got != 0 {
		t.Fatalf("next candidate probed %d times after the provider moved, want 0", got)
	}
}
