package debrid

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"
)

// rerouteWaveFixture is a TorBox file pinned to a dead host with the API
// taken out of scope: nothing here can move the link but the wave itself.
// The caller seeds the recent-edge ledger and drives resumeOnAnotherEdge
// directly, asserting adoption through the payload and probe counts
// through each edge's request counter.
func rerouteWaveFixture(t *testing.T, title string) (*Engine, *remoteFile, []byte, string, func(string) bool) {
	t.Helper()
	h := newHarness(t, 4<<20, "torbox")
	tor := h.provs["torbox-0"]
	tor.mu.Lock()
	tor.cached[testHash] = true
	tor.mu.Unlock()
	cfg := h.svc.Config()
	cfg.ReadAheadChunks = -1
	h.svc.UpdateConfig(cfg)
	raw, err := h.svc.Acquire(context.Background(), torrentRelease(title))
	if err != nil {
		t.Fatal(err)
	}
	e := NewEngine(h.svc, EngineOptions{BlockSize: testBlock, HedgeDelay: -1, Logger: h.svc.log})
	t.Cleanup(func() { e.Close() })
	h.svc.entryByID("torbox-0").p = fixtureTorBox("http://api.invalid") // the API is never asked here
	source, err := e.Open(context.Background(), title, raw)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { source.Close() })
	f := source.(*httpSource).f
	failed := payloadURL(t, raw)
	failedAddr := dialAddr(failed)
	usable := func(u string) bool {
		return u != "" && dialAddr(u) != failedAddr && !e.backoff.down(urlHost(u))
	}
	return e, f, raw, failed, usable
}

func payloadURL(t *testing.T, raw []byte) string {
	t.Helper()
	pl, err := DecodePayload(raw)
	if err != nil {
		t.Fatal(err)
	}
	return pl.URL
}

func payloadToken(t *testing.T, raw []byte) string {
	t.Helper()
	u := payloadURL(t, raw)
	return u[strings.LastIndex(u, "/")+1:]
}

func edgeHost(t *testing.T, m *mediaServer) string {
	t.Helper()
	u, err := url.Parse(m.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}

// recordFreshestFirst seeds the ledger so edges[0] is the freshest
// candidate and the wave therefore probes it first.
func recordFreshestFirst(e *Engine, edges []*mediaServer) {
	now := time.Now()
	for i, m := range edges {
		e.recent.record("torbox-0", m.URL[len("http://"):], now.Add(-time.Duration(10*(i+1))*time.Second))
	}
}

// probeRangeAt is the Range header the probe asks for at one offset: the
// drip fixture keys bodies on it verbatim, like stall and gate.
func probeRangeAt(off int64) string {
	return fmt.Sprintf("bytes=%d-%d", off, off+probeSample-1)
}

// The counterexample: with the probe spending its attempts by count and
// every dribble counting as a plain miss, a healthy candidate fourth
// behind three dribbled ones is never examined, and the wave gives up to
// minting onto the pinned dead node. Under the degraded tier the wave
// must keep walking on degraded verdicts and adopt the healthy edge
// inside one bounded wave.
func TestRerouteAdoptsHealthyPastDribblingBatch(t *testing.T) {
	e, f, raw, failed, usable := rerouteWaveFixture(t, "healthy-fourth")
	const off = int64(1 << 20)
	tok := payloadToken(t, raw)
	probeRange := probeRangeAt(off)
	var edges []*mediaServer
	for i := 0; i < 4; i++ {
		m := newMediaServer(t, 4<<20)
		m.issue(tok)
		edges = append(edges, m)
	}
	for _, i := range []int{0, 1, 2} { // the observed drained band: ~0.6 MB/s
		edges[i].drip[tok+"|"+probeRange] = mediaDrip{chunk: 6 << 10, pause: 100 * time.Millisecond}
	}
	recordFreshestFirst(e, edges) // dribblers freshest, healthy candidate fourth

	moved, replayed := f.resumeOnAnotherEdge(context.Background(), failed, usable, off)
	if !moved || !replayed {
		t.Fatalf("moved/replayed=%v/%v, want true/true: the wave must reach candidate four "+
			"instead of giving up after three dribbled probes", moved, replayed)
	}
	if p, _ := f.payload(); urlHost(p.URL) != edgeHost(t, edges[3]) {
		t.Fatalf("adopted host=%s, want the healthy edge %s: a dribbler must not outrank a flowing candidate",
			urlHost(f.pl.URL), edgeHost(t, edges[3]))
	}
	for i, m := range edges {
		if got := m.requests.Load(); got != 1 {
			t.Fatalf("edge %d requests=%d, want 1: each candidate is probed exactly once in one wave", i, got)
		}
	}
}

// An all-dribbled set must still recover: the wave ends its bounded
// attempts on a degraded-eligible edge instead of returning false, and
// every probed candidate lands in the probe cooldown so the next wave
// cannot re-kiss the same dribblers while better hosts sit unexamined.
func TestRerouteDegradesWhenAllCandidatesDribble(t *testing.T) {
	e, f, raw, failed, usable := rerouteWaveFixture(t, "all-slow")
	const off = int64(1 << 20)
	tok := payloadToken(t, raw)
	probeRange := probeRangeAt(off)
	d0, d1 := newMediaServer(t, 4<<20), newMediaServer(t, 4<<20)
	d0.issue(tok)
	d1.issue(tok)
	d0.drip[tok+"|"+probeRange] = mediaDrip{chunk: 6 << 10, pause: 100 * time.Millisecond}
	d1.drip[tok+"|"+probeRange] = mediaDrip{chunk: 6 << 10, pause: 100 * time.Millisecond}
	recordFreshestFirst(e, []*mediaServer{d1, d0}) // d1 fresher: probed first, adopted first

	moved, replayed := f.resumeOnAnotherEdge(context.Background(), failed, usable, off)
	if !moved || !replayed {
		t.Fatalf("moved/replayed=%v/%v, want true/true: an all-dribbled set must recover on a degraded route", moved, replayed)
	}
	if p, _ := f.payload(); urlHost(p.URL) != edgeHost(t, d1) {
		t.Fatalf("adopted host=%s, want the first probed dribbler %s", urlHost(f.pl.URL), edgeHost(t, d1))
	}
	if got := d0.requests.Load(); got != 1 {
		t.Fatalf("second candidate requests=%d, want 1: a degraded probe must not end the wave", got)
	}
	f.mu.Lock()
	cool := len(f.edgeProbeAfter)
	f.mu.Unlock()
	if cool != 2 {
		t.Fatalf("probe cooldown entries=%d, want 2: degraded verdicts must cooldown like timeouts", cool)
	}
}

// The wave's parent deadline must not be spent and then adopted under:
// a fallback remembered before the parent expired is no longer adoptable
// after it (fail closed), exactly as a strict verdict would be dropped.
func TestRerouteFallbackFailsClosedOnExpiredParent(t *testing.T) {
	e, f, raw, failed, usable := rerouteWaveFixture(t, "expired-parent")
	const off = int64(1 << 20)
	tok := payloadToken(t, raw)
	probeRange := probeRangeAt(off)
	d0, d1 := newMediaServer(t, 4<<20), newMediaServer(t, 4<<20)
	d0.issue(tok)
	d1.issue(tok)
	// Both answer instantly (eligibility arrives at once) and fall silent.
	d0.drip[tok+"|"+probeRange] = mediaDrip{chunk: 64, pause: time.Minute}
	d1.drip[tok+"|"+probeRange] = mediaDrip{chunk: 64, pause: time.Minute}
	recordFreshestFirst(e, []*mediaServer{d1, d0})

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	moved, _ := f.resumeOnAnotherEdge(ctx, failed, usable, off)
	if moved {
		t.Fatal("moved=true on an expired parent context: a fallback must not be adopted after the deadline")
	}
	if p, _ := f.payload(); p.OnReplayEdge {
		t.Fatal("the expired wave left the payload marked as replayed without a route")
	}
}

// Every probe counts toward the attempt cap no matter its verdict: fast
// truncated answers (the cheap-degraded trap) may not stretch one wave
// across the whole candidate ledger. And a fleet of truncated-but-
// eligible answers must still end on a degraded adoption, not a miss.
func TestRerouteProbeCapCountsEveryVerdict(t *testing.T) {
	t.Run("fast-truncated fleet adopts within the cap", func(t *testing.T) {
		e, f, raw, failed, usable := rerouteWaveFixture(t, "truncated-fleet")
		const off = int64(1 << 20)
		tok := payloadToken(t, raw)
		probeRange := probeRangeAt(off)
		var edges []*mediaServer
		for i := 0; i < 6; i++ {
			m := newMediaServer(t, 4<<20)
			m.issue(tok)
			m.drip[tok+"|"+probeRange] = mediaDrip{chunk: 6 << 10, eof: true} // 64 KiB then EOF, instantly
			edges = append(edges, m)
		}
		recordFreshestFirst(e, edges)
		moved, replayed := f.resumeOnAnotherEdge(context.Background(), failed, usable, off)
		if !moved || !replayed {
			t.Fatalf("moved/replayed=%v/%v, want true/true: a truncated-but-eligible fleet is degraded evidence, not a miss", moved, replayed)
		}
		for i, m := range edges[:recentEdgeTries] {
			if got := m.requests.Load(); got != 1 {
				t.Fatalf("edge %d requests=%d, want 1", i, got)
			}
		}
		for i := recentEdgeTries; i < len(edges); i++ {
			if got := edges[i].requests.Load(); got != 0 {
				t.Fatalf("edge %d requests=%d, want 0: the cap must stop the wave regardless of verdict", i, got)
			}
		}
		if p, _ := f.payload(); urlHost(p.URL) != edgeHost(t, edges[0]) {
			t.Fatal("adopted the wrong degraded candidate")
		}
	})
	t.Run("fast-rejected fleet exhausts exactly the cap", func(t *testing.T) {
		e, f, _, failed, usable := rerouteWaveFixture(t, "rejecting-fleet")
		const off = int64(1 << 20)
		var edges []*mediaServer
		for i := 0; i < 6; i++ {
			m := newMediaServer(t, 4<<20) // token never issued: a fast 403
			edges = append(edges, m)
		}
		recordFreshestFirst(e, edges)
		moved, _ := f.resumeOnAnotherEdge(context.Background(), failed, usable, off)
		if moved {
			t.Fatal("a fleet that proved nothing must not report a route")
		}
		var total int64
		for _, m := range edges {
			total += m.requests.Load()
		}
		if total != int64(recentEdgeTries) {
			t.Fatalf("total probes=%d, want exactly %d: the cap counts every verdict", total, recentEdgeTries)
		}
	})
}
