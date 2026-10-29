package playback

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

func waitWarm(t *testing.T, h *harness, userID int64, client string, key core.RatingKey) {
	t.Helper()
	h.m.mu.Lock()
	e := h.m.warm[identity{userID, client, key}]
	h.m.mu.Unlock()
	if e == nil {
		t.Fatal("no warm entry")
	}
	select {
	case <-e.done:
	case <-time.After(5 * time.Second):
		t.Fatal("warm did not complete")
	}
}

func counts(h *harness) (resolves, probes int) {
	h.resolver.mu.Lock()
	resolves = h.resolver.calls
	h.resolver.mu.Unlock()
	h.tr.mu.Lock()
	probes = h.tr.probes
	h.tr.mu.Unlock()
	return
}

// #2: the detail request (no decision extras) resolves under the 4K
// profile; the Play request derives 1080p caps. Different profile names,
// but the warm 1080p release is acceptable, so Prepare reuses it with no
// resolve or probe.
func TestWarmReusedAcrossProfileChange(t *testing.T) {
	h := newHarness(t, []*fakeRelease{mkv("r1", 2<<30, 90*time.Minute)}, []string{"r1"})
	detailCaps := defaultCaps("client")
	detailCaps.MaxHeight = 2160 // SelectProfile -> "4K"
	if !h.m.PreResolve(context.Background(), owner, movieKey, detailCaps) {
		t.Fatal("warm skipped")
	}
	waitWarm(t, h, owner.ID, "client", movieKey)

	playCaps := defaultCaps("client") // MaxHeight 1080 -> "1080p"
	playCaps.MaxBitrateKbps = 20000
	ps, err := h.m.Prepare(context.Background(), owner, movieKey, playCaps)
	if err != nil {
		t.Fatal(err)
	}
	if ps.Resolution.Release.ID != "r1" {
		t.Fatalf("wrong release %+v", ps)
	}
	if resolves, probes := counts(h); resolves != 1 || probes != 1 {
		t.Fatalf("warm miss across profile change: resolves=%d probes=%d", resolves, probes)
	}
}

// #2: a warm release that violates the Play-time cap is discarded.
func TestWarmDiscardedWhenTooTallForPlayCaps(t *testing.T) {
	rel := mkv("r1", 4<<30, 90*time.Minute)
	rel.Height = 2160
	h := newHarness(t, []*fakeRelease{rel}, []string{"r1"})
	detailCaps := defaultCaps("client")
	detailCaps.MaxHeight = 2160
	detailCaps.VideoCodecs = []string{"h264", "hevc"}
	if !h.m.PreResolve(context.Background(), owner, movieKey, detailCaps) {
		t.Fatal("warm skipped")
	}
	waitWarm(t, h, owner.ID, "client", movieKey)

	playCaps := defaultCaps("client") // 1080 cap: warm 2160p release rejected
	if _, err := h.m.Prepare(context.Background(), owner, movieKey, playCaps); err != nil {
		t.Fatal(err)
	}
	if resolves, probes := counts(h); resolves != 2 || probes != 2 {
		t.Fatalf("unacceptable warm was reused: resolves=%d probes=%d", resolves, probes)
	}
}

// #2: after a warm detail page, Prepare makes no resolver/probe calls and
// does not pay their latency.
func TestInstantPrepareAfterWarm(t *testing.T) {
	h := newHarness(t, []*fakeRelease{mkv("r1", 2<<30, 90*time.Minute)}, []string{"r1"})
	caps := defaultCaps("client")
	if !h.m.PreResolve(context.Background(), owner, movieKey, caps) {
		t.Fatal("warm skipped")
	}
	waitWarm(t, h, owner.ID, "client", movieKey)
	resolves0, probes0 := counts(h)

	h.resolver.delay = 500 * time.Millisecond // a cold resolve would pay this
	start := time.Now()
	ps, err := h.m.Prepare(context.Background(), owner, movieKey, caps)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if ps.Resolution.Release.ID != "r1" {
		t.Fatalf("wrong release %+v", ps)
	}
	if elapsed > 300*time.Millisecond {
		t.Fatalf("warm Prepare took %v, want <300ms", elapsed)
	}
	if resolves, probes := counts(h); resolves != resolves0 || probes != probes0 {
		t.Fatalf("warm Prepare called backends: resolves %d->%d probes %d->%d", resolves0, resolves, probes0, probes)
	}
}

// #2 shared contract: trailer/post-play prewarm runs during playback with
// a big clamped prefetch, while a detail warm is still blocked.
func TestPrewarmTrailerDuringPlayback(t *testing.T) {
	h := newHarness(t, []*fakeRelease{mkv("r1", 2<<30, 90*time.Minute)}, []string{"r1"})
	caps := defaultCaps("client")
	h.prepare(owner, movieKey, "client") // something is playing

	if h.m.Prewarm(context.Background(), owner, movie3, caps, core.PrewarmOptions{Reason: core.PrewarmDetail}) {
		t.Fatal("detail warm ran during playback")
	}
	if !h.m.Prewarm(context.Background(), owner, movie2, caps, core.PrewarmOptions{Reason: core.PrewarmTrailer, DuringPlayback: true}) {
		t.Fatal("trailer warm skipped during playback")
	}
	waitWarm(t, h, owner.ID, "client", movie2)

	if !h.m.Prewarm(context.Background(), friend, movie2, defaultCaps("f1"), core.PrewarmOptions{
		Reason: core.PrewarmPostPlay, DuringPlayback: true, PrefetchBytes: 1 << 30,
	}) {
		t.Fatal("postplay warm skipped")
	}
	waitWarm(t, h, friend.ID, "f1", movie2)

	// Prefetches complete after publish; wait for both background calls.
	deadline := time.Now().Add(2 * time.Second)
	for {
		h.engine.mu.Lock()
		n := len(h.engine.prefetch)
		h.engine.mu.Unlock()
		if n >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.engine.mu.Lock()
	prefetch := append([]prefetchCall(nil), h.engine.prefetch...)
	h.engine.mu.Unlock()
	var lengths []int64
	for _, p := range prefetch {
		lengths = append(lengths, p.Length)
	}
	if len(lengths) != 2 || !((lengths[0] == 48<<20 && lengths[1] == 64<<20) || (lengths[0] == 64<<20 && lengths[1] == 48<<20)) {
		t.Fatalf("prefetch lengths = %v, want 48MiB and 64MiB(clamped) in either order", lengths)
	}
	// The trailer-warmed title then prepares without new backend calls.
	resolves0, probes0 := counts(h)
	ps, err := h.m.Prepare(context.Background(), owner, movie2, caps)
	if err != nil {
		t.Fatal(err)
	}
	if ps.Resolution.Release.ID != "r1" {
		t.Fatalf("wrong release %+v", ps)
	}
	if resolves, probes := counts(h); resolves != resolves0 || probes != probes0 {
		t.Fatalf("post-warm Prepare called backends: %d->%d, %d->%d", resolves0, resolves, probes0, probes)
	}
}

func TestSpeculativePrewarmsCannotConsumeDetailRateBudget(t *testing.T) {
	h := newHarness(t, nil, nil)
	caps := defaultCaps("c1")
	for i := int64(100); i < 106; i++ {
		key := core.RatingKey(i)
		if !h.m.Prewarm(context.Background(), owner, key, caps, core.PrewarmOptions{Reason: core.PrewarmTrailer, DuringPlayback: true}) {
			t.Fatalf("speculative request %d was unexpectedly rejected", i)
		}
		deadline := time.Now().Add(time.Second)
		for {
			h.m.mu.Lock()
			pending := h.m.warm[identity{owner.ID, caps.ClientID, key}] != nil
			h.m.mu.Unlock()
			if !pending {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("failed speculative warm did not drain")
			}
			time.Sleep(time.Millisecond)
		}
	}
	if !h.m.Prewarm(context.Background(), owner, 200, caps, core.PrewarmOptions{Reason: core.PrewarmDetail}) {
		t.Fatal("six trailer prewarms exhausted the detail-page rate budget")
	}
}

// Shared contract: PrepareVariant resolves the picked release and
// replaces a session on a different one.
func TestPrepareVariantPicksAndReplaces(t *testing.T) {
	h := newHarness(t, []*fakeRelease{mkv("A", 10000, time.Minute), mkv("B", 20000, time.Minute)}, []string{"A", "B"})
	caps := defaultCaps("c1")

	ps, err := h.m.PrepareVariant(context.Background(), owner, movieKey, caps, "B")
	if err != nil {
		t.Fatal(err)
	}
	if ps.Resolution.Release.ID != "B" {
		t.Fatalf("variant got %s", ps.Resolution.Release.ID)
	}
	h.resolver.mu.Lock()
	prefers := append([]string(nil), h.resolver.prefers...)
	h.resolver.mu.Unlock()
	if len(prefers) == 0 || prefers[0] != "B" {
		t.Fatalf("Prefer not passed to resolver: %v", prefers)
	}
	// Same pick again reuses the session.
	ps2, err := h.m.PrepareVariant(context.Background(), owner, movieKey, caps, "B")
	if err != nil || ps2.ID != ps.ID {
		t.Fatalf("same variant not reused: %v %v", ps2, err)
	}
	// A different pick replaces it.
	ps3, err := h.m.PrepareVariant(context.Background(), owner, movieKey, caps, "A")
	if err != nil {
		t.Fatal(err)
	}
	if ps3.ID == ps.ID || ps3.Resolution.Release.ID != "A" {
		t.Fatalf("variant not replaced: %+v", ps3)
	}
	if _, ok := h.m.Session(ps.ID); ok {
		t.Fatal("replaced session still live")
	}
}

func TestConcurrentPrepareVariantsDoNotLeaveDuplicateSessions(t *testing.T) {
	h := newHarness(t, []*fakeRelease{mkv("A", 10000, time.Minute), mkv("B", 20000, time.Minute)}, []string{"A", "B"})
	h.resolver.delay = 100 * time.Millisecond // let the distinct preference flights overlap
	caps := defaultCaps("c1")
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			id := "A"
			if i%2 == 1 {
				id = "B"
			}
			_, _ = h.m.PrepareVariant(context.Background(), owner, movieKey, caps, id)
		}(i)
	}
	close(start)
	wg.Wait()
	h.m.mu.Lock()
	n := len(h.m.sessions)
	h.m.mu.Unlock()
	if n != 1 {
		t.Fatalf("concurrent version picks left %d sessions, want one", n)
	}
}

// Shared contract: a warm entry for another release is not consumed by a
// variant Prepare.
func TestPrepareVariantIgnoresWarmOfOtherRelease(t *testing.T) {
	h := newHarness(t, []*fakeRelease{mkv("A", 2<<30, 90*time.Minute), mkv("B", 2<<30, 90*time.Minute)}, []string{"A", "B"})
	caps := defaultCaps("c1")
	if !h.m.PreResolve(context.Background(), owner, movieKey, caps) {
		t.Fatal("warm skipped")
	}
	waitWarm(t, h, owner.ID, "c1", movieKey) // warms A (rank 1)

	ps, err := h.m.PrepareVariant(context.Background(), owner, movieKey, caps, "B")
	if err != nil {
		t.Fatal(err)
	}
	if ps.Resolution.Release.ID != "B" {
		t.Fatalf("variant consumed warm A: %+v", ps)
	}
	h.m.mu.Lock()
	_, warmKept := h.m.warm[identity{owner.ID, "c1", movieKey}]
	h.m.mu.Unlock()
	if !warmKept {
		t.Fatal("warm entry for A was consumed by variant B")
	}
}

// #5: with NZB fetching available, an exact-size repost is opened before
// a higher-ranked mismatched candidate.
func TestDirectFailoverPrefersExactSizeRepost(t *testing.T) {
	a := mkv("A", 200000, 100*time.Second)
	a.setFail(120000, 130000, nil)
	h := newHarness(t, []*fakeRelease{a, mkv("B", 400000, 100*time.Second), mkv("C", 200000, 100*time.Second)}, []string{"A", "B", "C"})
	for i, size := range []int64{200000, 400000, 200000} {
		h.resolver.candidates[i].Size = size
	}
	h.m.deps.FetchNZB = func(ctx context.Context, r core.Release) ([]byte, error) {
		return []byte("nzb-" + r.ID), nil
	}
	ps := h.prepare(owner, movieKey, "c1")
	srv := directPlayServer(t, h.m, ps.ID)
	_, body, err := get(t, srv.URL, nil)
	if err == nil || len(body) != int(a.FailFrom) {
		t.Fatalf("first response: bytes=%d err=%v", len(body), err)
	}
	st, _ := h.m.Session(ps.ID)
	if st.Resolution.Release.ID != "C" {
		t.Fatalf("exact-size repost not chosen: %+v", st)
	}
	if st.RequiresRemux {
		t.Fatal("exact-size swap must not require remux")
	}
	h.engine.mu.Lock()
	opens := append([]string(nil), h.engine.opens...)
	h.engine.mu.Unlock()
	for _, id := range opens {
		if id == "B" {
			t.Fatalf("mismatched candidate opened before exact repost: %v", opens)
		}
	}
	// The client re-requests and is served the whole file from C.
	resp, body2, err := get(t, srv.URL, nil)
	if err != nil || resp.StatusCode != 200 || !bytes.Equal(body2, expected("C", 0, 200000)) {
		t.Fatalf("re-request from C: %d %v len=%d", resp.StatusCode, err, len(body2))
	}
}

// #5: SetDecision(remux) satisfies a pending remux requirement.
func TestRequiresRemuxClearedByRemuxDecision(t *testing.T) {
	a := mkv("A", 200000, 100*time.Second)
	a.setFail(120000, 130000, nil)
	h := newHarness(t, []*fakeRelease{a, mkv("B", 400000, 100*time.Second)}, []string{"A", "B"})
	ps := h.prepare(owner, movieKey, "c1")
	srv := directPlayServer(t, h.m, ps.ID)
	if _, _, err := get(t, srv.URL, nil); err == nil {
		t.Fatal("expected response to end")
	}
	if !h.m.RequiresRemux(ps.ID) {
		t.Fatal("remux not required after mismatched swap")
	}
	h.m.SetDecision(ps.ID, core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioCopy: true})
	if h.m.RequiresRemux(ps.ID) {
		t.Fatal("remux decision did not clear the flag")
	}
}

type fakeAligner struct {
	mapped time.Duration
	ok     bool
	delay  time.Duration
	calls  int
}

func (f *fakeAligner) AlignTime(ctx context.Context, req core.AlignRequest, at time.Duration) (time.Duration, bool) {
	f.calls++
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return 0, false
		}
	}
	return f.mapped, f.ok
}

// Shared contract: failover maps through TimelineAligner when wired, and
// falls back to duration alignment on a miss or timeout.
func TestFailoverUsesTimelineAligner(t *testing.T) {
	run := func(t *testing.T, aligner *fakeAligner) time.Duration {
		h := newHarness(t, []*fakeRelease{mkv("A", 100000, 100*time.Second), mkv("B", 100000, 100*time.Second)}, []string{"A", "B"})
		h.m.deps.Aligner = aligner
		ps := h.prepare(owner, movieKey, "c1")
		hls := h.tr.addHLS(ps.ID)
		if _, err := h.m.Failover(context.Background(), ps.ID, 30*time.Second); err != nil {
			t.Fatal(err)
		}
		select {
		case sw := <-hls.swapC:
			return sw.At
		default:
			t.Fatal("no HLS swap")
			return 0
		}
	}
	if got := run(t, &fakeAligner{mapped: 25 * time.Second, ok: true}); got != 25*time.Second {
		t.Fatalf("aligned swap at %v, want 25s", got)
	}
	if got := run(t, &fakeAligner{ok: false}); got != 30*time.Second {
		t.Fatalf("miss fallback at %v, want 30s", got)
	}
	if got := run(t, &fakeAligner{mapped: 5 * time.Second, ok: true, delay: 2 * time.Second}); got != 30*time.Second {
		t.Fatalf("timeout fallback at %v, want 30s", got)
	}
}

// ---------------------------------------------------------------------------
// #28 speed-coupled down-failover

func speedOpts(o *Options) {
	o.SpeedGrace = time.Second
	o.SpeedSlowChecks = 2
	o.SpeedCheckInterval = time.Hour // tests drive checkSpeeds directly
	o.SpeedThreshold = 0.8
}

func feedFetches(s *session, chunks, chunkBytes int, each time.Duration, now time.Time) {
	for i := 0; i < chunks; i++ {
		s.noteFetch(chunkBytes, each, now)
	}
}

func TestDownFailoverTriggersOnSlowSource(t *testing.T) {
	h := newHarness(t, []*fakeRelease{
		mkv("A", 80_000_000, 100*time.Second), // 6.4 Mbps
		mkv("B", 10_000_000, 100*time.Second), // 0.8 Mbps
	}, []string{"A", "B"}, func(h *harness, o *Options) { speedOpts(o) })
	ps := h.prepare(owner, movieKey, "c1")
	h.m.SetDecision(ps.ID, core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioCopy: true})
	hls := h.tr.addHLS(ps.ID)
	s := h.m.lookup(ps.ID)

	h.clock.Advance(2 * time.Second) // past the grace period
	// ~327 KB/s of fetch against an 800 KB/s media rate.
	feedFetches(s, 10, 64<<10, 200*time.Millisecond, h.clock.Now())

	h.m.checkSpeeds() // hysteresis: first slow check only counts
	select {
	case <-hls.swapC:
		t.Fatal("swapped on first slow check")
	default:
	}
	h.m.checkSpeeds()
	select {
	case <-hls.swapC:
	case <-time.After(3 * time.Second):
		t.Fatal("down-failover did not swap the HLS input")
	}
	st, _ := h.m.Session(ps.ID)
	if st.Resolution.Release.ID != "B" || st.DownFailovers != 1 || st.Failovers != 1 {
		t.Fatalf("down-failover state: %+v", st)
	}
	if h.resolver.isBad("A") {
		t.Fatal("slow release marked bad")
	}
}

func TestDownFailoverSkipsFastSourceAndGraceAndUnsafe(t *testing.T) {
	t.Run("fast", func(t *testing.T) {
		h := newHarness(t, []*fakeRelease{mkv("A", 80_000_000, 100*time.Second), mkv("B", 10_000_000, 100*time.Second)}, []string{"A", "B"},
			func(h *harness, o *Options) { speedOpts(o) })
		ps := h.prepare(owner, movieKey, "c1")
		h.m.SetDecision(ps.ID, core.Decision{Kind: core.DecisionRemux})
		h.tr.addHLS(ps.ID)
		h.clock.Advance(2 * time.Second)
		feedFetches(h.m.lookup(ps.ID), 10, 1<<20, 100*time.Millisecond, h.clock.Now()) // 10 MiB/s
		h.m.checkSpeeds()
		h.m.checkSpeeds()
		if st, _ := h.m.Session(ps.ID); st.Resolution.Release.ID != "A" || st.DownFailovers != 0 {
			t.Fatalf("fast source down-failed: %+v", st)
		}
	})
	t.Run("grace", func(t *testing.T) {
		h := newHarness(t, []*fakeRelease{mkv("A", 80_000_000, 100*time.Second), mkv("B", 10_000_000, 100*time.Second)}, []string{"A", "B"},
			func(h *harness, o *Options) { speedOpts(o) })
		ps := h.prepare(owner, movieKey, "c1")
		h.m.SetDecision(ps.ID, core.Decision{Kind: core.DecisionRemux})
		h.tr.addHLS(ps.ID)
		// No clock advance: still in the start grace period.
		feedFetches(h.m.lookup(ps.ID), 10, 64<<10, 200*time.Millisecond, h.clock.Now())
		h.m.checkSpeeds()
		h.m.checkSpeeds()
		if st, _ := h.m.Session(ps.ID); st.Resolution.Release.ID != "A" {
			t.Fatalf("down-failed during grace: %+v", st)
		}
	})
	t.Run("no safe swap path", func(t *testing.T) {
		h := newHarness(t, []*fakeRelease{mkv("A", 80_000_000, 100*time.Second), mkv("B", 10_000_000, 100*time.Second)}, []string{"A", "B"},
			func(h *harness, o *Options) { speedOpts(o) })
		ps := h.prepare(owner, movieKey, "c1")
		// No decision recorded and no HLS session: nothing safe to swap.
		h.clock.Advance(2 * time.Second)
		feedFetches(h.m.lookup(ps.ID), 10, 64<<10, 200*time.Millisecond, h.clock.Now())
		h.m.checkSpeeds()
		h.m.checkSpeeds()
		if st, _ := h.m.Session(ps.ID); st.Resolution.Release.ID != "A" || st.DownFailovers != 0 {
			t.Fatalf("unsafe down-failover happened: %+v", st)
		}
	})
	t.Run("direct play moves to remux", func(t *testing.T) {
		h := newHarness(t, []*fakeRelease{mkv("A", 80_000_000, 100*time.Second), mkv("B", 10_000_000, 100*time.Second)}, []string{"A", "B"},
			func(h *harness, o *Options) { speedOpts(o) })
		ps := h.prepare(owner, movieKey, "c1")
		h.m.SetDecision(ps.ID, core.Decision{Kind: core.DecisionDirectPlay})
		h.clock.Advance(2 * time.Second)
		feedFetches(h.m.lookup(ps.ID), 10, 64<<10, 200*time.Millisecond, h.clock.Now())
		h.m.checkSpeeds()
		h.m.checkSpeeds()
		deadline := time.Now().Add(3 * time.Second)
		for {
			st, _ := h.m.Session(ps.ID)
			if st != nil && st.Resolution.Release.ID == "B" {
				if !st.RequiresRemux || st.DownFailovers != 1 {
					t.Fatalf("direct down-failover state: %+v", st)
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("direct down-failover did not swap: %+v", st)
			}
			time.Sleep(5 * time.Millisecond)
		}
	})
}

// #28 end to end: a throttled fake source produces the fetch evidence
// through real reads, then the monitor swaps releases.
func TestDownFailoverFromThrottledReads(t *testing.T) {
	slow := mkv("A", 200_000_000, 100*time.Second) // 2 MB/s media rate
	slow.ReadDelay = 200 * time.Millisecond        // per 64 KiB chunk: ~327 KB/s
	h := newHarness(t, []*fakeRelease{slow, mkv("B", 10_000_000, 100*time.Second)}, []string{"A", "B"},
		func(h *harness, o *Options) { speedOpts(o); o.ChunkSize = 64 << 10 })
	ps := h.prepare(owner, movieKey, "c1")
	h.m.SetDecision(ps.ID, core.Decision{Kind: core.DecisionRemux})
	hls := h.tr.addHLS(ps.ID)
	srv := directPlayServer(t, h.m, ps.ID)
	if _, _, err := get(t, srv.URL, map[string]string{"Range": "bytes=0-524287"}); err != nil {
		t.Fatal(err)
	}
	h.clock.Advance(2 * time.Second)
	h.m.checkSpeeds()
	h.m.checkSpeeds()
	select {
	case <-hls.swapC:
	case <-time.After(3 * time.Second):
		st, _ := h.m.Session(ps.ID)
		t.Fatalf("throttled source did not down-fail over: %+v", st)
	}
	if st, _ := h.m.Session(ps.ID); st.Resolution.Release.ID != "B" {
		t.Fatalf("wrong release after throttled down-failover: %+v", st)
	}
}

// ---------------------------------------------------------------------------
// #6 keyframe publication

func TestKeyframesPublishedInBackground(t *testing.T) {
	b, want := indexedMKV()
	rel := &fakeRelease{ID: "K", Name: "K.mkv", Size: int64(len(b)), Duration: 10 * time.Second, Content: b}
	h := newHarness(t, []*fakeRelease{rel}, []string{"K"})
	ps := h.prepare(owner, movieKey, "c1")
	deadline := time.Now().Add(3 * time.Second)
	for {
		st, _ := h.m.Session(ps.ID)
		if st != nil && len(st.Info.Keyframes) == len(want) {
			for i, p := range want {
				if st.Info.Keyframes[i].Offset != p.Offset || st.Info.Keyframes[i].Time != p.Time {
					t.Fatalf("keyframe %d = %+v, want %+v", i, st.Info.Keyframes[i], p)
				}
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("keyframes never published: %+v", st)
		}
		time.Sleep(5 * time.Millisecond)
	}
	// The virtual TimeMap uses them too.
	s := h.m.lookup(ps.ID)
	s.mu.Lock()
	pts := s.virtual.Points
	s.mu.Unlock()
	if len(pts) != len(want) {
		t.Fatalf("virtual points = %v", pts)
	}
	rp, err := h.m.ResumePoint(context.Background(), owner.ID, movieKey)
	if err != nil || rp == nil || len(rp.Keyframes) != len(want) {
		t.Fatalf("resume keyframe index = %+v, %v", rp, err)
	}
	for i, p := range want {
		if rp.Keyframes[i].Offset != p.Offset || rp.Keyframes[i].Time != p.Time {
			t.Fatalf("resume keyframe %d = %+v, want %+v", i, rp.Keyframes[i], p)
		}
	}
	// Callers receive snapshots; mutating one must not corrupt the published
	// index used by later timeline mapping or snapshots.
	st, _ := h.m.Session(ps.ID)
	st.Info.Keyframes[0].Offset = -1
	st2, _ := h.m.Session(ps.ID)
	if st2.Info.Keyframes[0].Offset != want[0].Offset {
		t.Fatalf("snapshot mutation changed published index: %+v", st2.Info.Keyframes)
	}
}

func TestKeyframeOffsetAtSnapsToKeyframe(t *testing.T) {
	m := TimeMap{Size: 1000, Duration: 100 * time.Second, Points: []IndexPoint{{0, 0}, {500, 20 * time.Second}, {900, 80 * time.Second}}}
	if got := m.KeyframeOffsetAt(50 * time.Second); got != 500 {
		t.Fatalf("snap = %d, want 500", got)
	}
	if got := m.KeyframeOffsetAt(10 * time.Second); got != 0 {
		t.Fatalf("snap before first point = %d, want 0", got)
	}
	plain := TimeMap{Size: 1000, Duration: 100 * time.Second}
	if got := plain.KeyframeOffsetAt(50 * time.Second); got != 500 {
		t.Fatalf("no-index snap = %d, want CBR 500", got)
	}
	info := core.MediaInfo{Size: 1000, Duration: 100 * time.Second, Keyframes: []core.KeyframePoint{{Offset: 500, Time: 20 * time.Second}}}
	if tm := TimeMapFor(info); tm.KeyframeOffsetAt(30*time.Second) != 500 {
		t.Fatalf("TimeMapFor lost keyframes: %+v", tm)
	}
}

func TestWarmCachesKeyframes(t *testing.T) {
	b, want := indexedMKV()
	rel := &fakeRelease{ID: "K", Name: "K.mkv", Size: int64(len(b)), Duration: 10 * time.Second, Content: b}
	h := newHarness(t, []*fakeRelease{rel}, []string{"K"})
	// The synthetic file is far smaller than any real release; drop the
	// profile's size floor so warm-acceptance checks the other rules.
	profile := config.DefaultQuality().Profiles[1]
	profile.MinSizeMBPerMin = 0
	h.m.deps.SelectProfile = func(core.ClientCaps, *core.User) core.QualityProfile { return profile }
	caps := defaultCaps("c1")
	if !h.m.PreResolve(context.Background(), owner, movieKey, caps) {
		t.Fatal("warm skipped")
	}
	waitWarm(t, h, owner.ID, "c1", movieKey)
	// Readiness no longer waits for an optional seek index. Once it has
	// finished in the background, later Play should reuse it too.
	deadline := time.Now().Add(time.Second)
	for {
		h.m.mu.Lock()
		entry := h.m.warm[identity{owner.ID, caps.ClientID, movieKey}]
		ready := entry != nil && entry.probe != nil && len(entry.probe.points) == len(want)
		h.m.mu.Unlock()
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("warm keyframe cache never filled")
		}
		time.Sleep(time.Millisecond)
	}
	ps, err := h.m.Prepare(context.Background(), owner, movieKey, caps)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps.Info.Keyframes) != len(want) {
		t.Fatalf("warm keyframes missing on Prepare: %+v", ps.Info.Keyframes)
	}
}

func TestBlockedWarmAndPersistedResumeResolveAgain(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, []*fakeRelease{mkv("A", 10000, time.Minute), mkv("B", 10000, time.Minute)}, []string{"A", "B"})
	ps := h.prepare(owner, movieKey, "first")
	h.m.Stop(ps.ID)
	if !h.m.PreResolve(ctx, owner, movieKey, defaultCaps("warm")) {
		t.Fatal("warm skipped")
	}
	waitWarm(t, h, owner.ID, "warm", movieKey)
	if err := h.resolver.MarkBad(ctx, "A", "owner"); err != nil {
		t.Fatal(err)
	}
	if p, err := h.m.ResumePoint(ctx, owner.ID, movieKey); err != nil || p != nil {
		t.Fatalf("blocked resume point: %v %v", p, err)
	}
	ps = h.prepare(owner, movieKey, "warm")
	if ps.Resolution.Release.ID != "B" {
		t.Fatalf("blocked warm reused: %s", ps.Resolution.Release.ID)
	}
	h.m.Stop(ps.ID)
}
