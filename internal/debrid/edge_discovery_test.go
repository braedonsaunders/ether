package debrid

import (
	"context"
	"strings"
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
		time.Sleep(100 * time.Millisecond)
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
