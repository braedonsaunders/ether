package stream

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/nzb"
)

func TestHealthSampleSpread(t *testing.T) {
	mk := func(n int) *postedFile {
		f := &nzb.File{Name: "v"}
		for i := 1; i <= n; i++ {
			f.Segments = append(f.Segments, nzb.Segment{Number: i, MessageID: "x"})
		}
		return newPostedFile(0, f, 0, nil)
	}
	a, b, c := mk(40), mk(40), mk(20)
	refs := healthSample([]*postedFile{a, b, c}, 12)
	if len(refs) != 12 {
		t.Fatalf("samples = %d", len(refs))
	}
	if refs[0].pf != a || refs[0].slot != 0 {
		t.Fatalf("first sample = %+v", refs[0])
	}
	if last := refs[len(refs)-1]; last.pf != c || last.slot != 19 {
		t.Fatalf("last sample = %+v", last)
	}
	seen := map[*postedFile]bool{}
	for _, r := range refs {
		seen[r.pf] = true
	}
	if len(seen) != 3 {
		t.Fatal("not every volume sampled")
	}
	// Fewer slots than samples: all of them, once.
	if refs := healthSample([]*postedFile{mk(5)}, 12); len(refs) != 5 {
		t.Fatalf("small sample = %d", len(refs))
	}
	if refs := healthSample([]*postedFile{mk(100)}, 1); len(refs) != 2 {
		t.Fatalf("minimum sample = %d", len(refs))
	}
}

func TestCheckHealthy(t *testing.T) {
	f := newFakeFetcher()
	doc := post(t, f, 10_000, postFile{name: "h.mkv", data: mkvBytes(1, 1_000_000)})
	e := newTestEngine(t, f, nil, Options{HealthSamples: 12})
	ok, err := e.Check(context.Background(), doc)
	if !ok || err != nil {
		t.Fatalf("Check = %v %v", ok, err)
	}
	f.mu.Lock()
	stats := f.statCalls
	f.mu.Unlock()
	// Ten spread STATs and two BODY probes, plus the ten head articles the
	// spread sample did not already cover (slots 0 and 9 are shared).
	if stats != 20 {
		t.Fatalf("STATs = %d, want 20 plus two BODY samples", stats)
	}
	// Open after Check reuses the index: no further header fetch.
	f.resetCounts()
	src := openSource(t, e, "h", doc)
	if f.totalCalls() != 0 {
		t.Fatalf("Open after Check fetched %d", f.totalCalls())
	}
	_ = src
}

func TestCheckMissingSample(t *testing.T) {
	f := newFakeFetcher()
	doc := post(t, f, 10_000, postFile{name: "h.mkv", data: mkvBytes(1, 1_000_000)})
	f.setMissing(postID("h.mkv", 100)) // the last part is always sampled
	e := newTestEngine(t, f, nil, Options{HealthSamples: 5})
	ok, err := e.Check(context.Background(), doc)
	if ok || !errors.Is(err, core.ErrArticleMissing) {
		t.Fatalf("Check = %v %v", ok, err)
	}
}

func TestCheckRejections(t *testing.T) {
	f := newFakeFetcher()
	e := newTestEngine(t, f, nil, Options{})
	noVideo := post(t, f, 1000, postFile{name: "x.nfo", data: []byte("x")}, postFile{name: "x.sample.mkv", data: mkvBytes(1, 2000)})
	if ok, err := e.Check(context.Background(), noVideo); ok || !errors.Is(err, ErrNoPlayableFile) || !IsRejection(err) {
		t.Fatalf("no video: %v %v", ok, err)
	}
	gap := post(t, f, 1000, postFile{name: "gap.mkv", data: mkvBytes(2, 5000), omit: []int{2}})
	if ok, err := e.Check(context.Background(), gap); ok || !errors.Is(err, core.ErrArticleMissing) {
		t.Fatalf("gap: %v %v", ok, err)
	}
	if ok, err := e.Check(context.Background(), []byte("<nzb")); ok || !errors.Is(err, ErrInvalidNZB) {
		t.Fatalf("invalid: %v %v", ok, err)
	}
	missingRAR := post(t, f, 1000,
		postFile{name: "m.part01.rar", data: randBytes(1, 3000)},
		postFile{name: "m.part03.rar", data: randBytes(2, 3000)},
	)
	if ok, err := e.Check(context.Background(), missingRAR); ok || !errors.Is(err, ErrIncomplete) {
		t.Fatalf("missing volume: %v %v", ok, err)
	}
}

type bodyUnavailableFetcher struct{ *fakeFetcher }

func (f bodyUnavailableFetcher) Stat(ctx context.Context, _ string) (bool, error) {
	<-ctx.Done()
	return false, ctx.Err()
}
func (f bodyUnavailableFetcher) CheckBody(context.Context, string, []string, core.Priority) error {
	return core.ErrArticleMissing
}
func (f bodyUnavailableFetcher) BodyPart(ctx context.Context, _ string, _ []string, _ core.Priority) ([]byte, core.ArticlePart, error) {
	<-ctx.Done()
	return nil, core.ArticlePart{}, ctx.Err()
}

// streamHealthFetcher adds the production fetcher's optional health
// capabilities: BODY probes validated against the expected part, and a
// pipelined StatStream whose slow chunk only answers when cancelled.
type streamHealthFetcher struct {
	*fakeFetcher
	mu    sync.Mutex
	wants []core.ArticlePart
}

func (h *streamHealthFetcher) isGone(id string) bool {
	h.fakeFetcher.mu.Lock()
	defer h.fakeFetcher.mu.Unlock()
	_, ok := h.articles[id]
	return !ok || h.missing[id]
}

func (h *streamHealthFetcher) CheckBodyPart(_ context.Context, id string, _ []string, _ core.Priority, want core.ArticlePart) error {
	h.mu.Lock()
	h.wants = append(h.wants, want)
	h.mu.Unlock()
	if h.isGone(id) {
		return fmt.Errorf("fake BODY <%s>: %w", id, core.ErrArticleMissing)
	}
	return nil
}

func (h *streamHealthFetcher) StatStream(ctx context.Context, ids []string, fn func(string, bool) bool) error {
	// A confirmed miss is known at once; everything else sits in a slow
	// provider chunk that never answers until the context ends.
	for _, id := range ids {
		if h.isGone(id) && !fn(id, false) {
			return nil
		}
	}
	<-ctx.Done()
	return ctx.Err()
}

// A miss confirmed by the pipelined STAT stream must fail the check at
// once, without waiting for the stream's slow chunk.
func TestCheckStatStreamFailsFastOnConfirmedMiss(t *testing.T) {
	f := newFakeFetcher()
	doc := post(t, f, 10_000, postFile{name: "taken.mkv", data: mkvBytes(1, 1_000_000)})
	f.setMissing(postID("taken.mkv", 100)) // the last part is always sampled
	hf := &streamHealthFetcher{fakeFetcher: f}
	e := newTestEngine(t, hf, nil, Options{HealthSamples: 12, HealthParallel: 12})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	started := time.Now()
	ok, err := e.Check(ctx, doc)
	if ok || !errors.Is(err, core.ErrArticleMissing) {
		t.Fatalf("Check = %v %v", ok, err)
	}
	if time.Since(started) > 1500*time.Millisecond {
		t.Fatal("waited for the slow STAT chunk instead of failing fast")
	}
	tr := e.HealthTimings()
	if !tr.FailFast || !strings.Contains(tr.FailReason, "STAT") {
		t.Fatalf("trace = %+v", tr)
	}
	if tr.BodyProbes != 2 {
		t.Fatalf("body probes = %d, want 2 concurrent with STAT", tr.BodyProbes)
	}
	hf.mu.Lock()
	defer hf.mu.Unlock()
	if len(hf.wants) != 2 {
		t.Fatalf("CheckBodyPart calls = %d", len(hf.wants))
	}
	for _, w := range hf.wants {
		if w.Total != 100 || w.Part < 1 || w.Part > 100 {
			t.Fatalf("probe want = %+v", w)
		}
	}
}

// STAT claims everything is present, but the mid-file BODY probes (via
// CheckBodyPart) report takedowns: unhealthy, with the trace saying so.
type bodyPartMissingFetcher struct {
	*fakeFetcher
	mu    sync.Mutex
	wants []core.ArticlePart
}

func (f *bodyPartMissingFetcher) CheckBodyPart(_ context.Context, id string, _ []string, _ core.Priority, want core.ArticlePart) error {
	f.mu.Lock()
	f.wants = append(f.wants, want)
	f.mu.Unlock()
	return fmt.Errorf("fake BODY <%s> 451: %w", id, core.ErrArticleMissing)
}

func TestCheckBodyPartTakedownBehindStat223(t *testing.T) {
	f := newFakeFetcher()
	doc := post(t, f, 10_000, postFile{name: "taken.mkv", data: mkvBytes(1, 1_000_000)})
	bf := &bodyPartMissingFetcher{fakeFetcher: f}
	e := newTestEngine(t, bf, nil, Options{HealthSamples: 12, HealthParallel: 12})
	ok, err := e.Check(context.Background(), doc)
	if ok || !errors.Is(err, core.ErrArticleMissing) {
		t.Fatalf("Check = %v %v", ok, err)
	}
	tr := e.HealthTimings()
	if tr.BodyProbes != 2 || tr.BodyOK != 0 || !strings.Contains(tr.FailReason, "BODY") {
		t.Fatalf("trace = %+v", tr)
	}
	// The reason must keep the underlying reply code (here the fake's
	// 451): "missing" alone cannot tell a takedown from a plain 430.
	if !strings.Contains(tr.FailReason, "451") {
		t.Fatalf("fail reason %q lost the NNTP code", tr.FailReason)
	}
	bf.mu.Lock()
	defer bf.mu.Unlock()
	seenMid := false
	for _, w := range bf.wants {
		if w.Part > 20 && w.Part < 90 {
			seenMid = true
		}
	}
	if !seenMid {
		t.Fatalf("BODY probes did not target mid-file parts: %+v", bf.wants)
	}
}

// A provider error from the STAT stream is not proof of absence: the
// check surfaces the error instead of declaring the release missing.
type statStreamErrorFetcher struct {
	*fakeFetcher
}

func (f statStreamErrorFetcher) StatStream(context.Context, []string, func(string, bool) bool) error {
	return fmt.Errorf("fake: provider unreachable")
}

func TestCheckStatStreamErrorIsNotMissing(t *testing.T) {
	f := newFakeFetcher()
	doc := post(t, f, 10_000, postFile{name: "x.mkv", data: mkvBytes(1, 200_000)})
	e := newTestEngine(t, statStreamErrorFetcher{f}, nil, Options{HealthSamples: 6})
	ok, err := e.Check(context.Background(), doc)
	if ok || err == nil || errors.Is(err, core.ErrArticleMissing) {
		t.Fatalf("Check = %v %v, want a transient error", ok, err)
	}
}

func TestCheckHealthyTrace(t *testing.T) {
	f := newFakeFetcher()
	doc := post(t, f, 10_000, postFile{name: "h.mkv", data: mkvBytes(1, 1_000_000)})
	e := newTestEngine(t, f, nil, Options{HealthSamples: 12})
	if ok, err := e.Check(context.Background(), doc); !ok || err != nil {
		t.Fatalf("Check = %v %v", ok, err)
	}
	tr := e.HealthTimings()
	if tr.Samples != 22 || tr.BodyProbes != 2 || tr.BodyOK != 2 || tr.FailFast || tr.FailReason != "" {
		t.Fatalf("trace = %+v", tr)
	}
}

func TestBodyProbeSlots(t *testing.T) {
	if got := bodyProbeSlots(12); len(got) != 2 || !got[4] || !got[8] {
		t.Fatalf("12 samples: %v", got)
	}
	if got := bodyProbeSlots(4); len(got) != 2 || !got[1] || !got[2] {
		t.Fatalf("4 samples: %v", got)
	}
	if got := bodyProbeSlots(3); len(got) != 1 || !got[1] {
		t.Fatalf("3 samples: %v", got)
	}
	if got := bodyProbeSlots(2); got != nil {
		t.Fatalf("2 samples: %v", got)
	}
}

func TestExpectedPart(t *testing.T) {
	f := &nzb.File{Name: "v"}
	for i := 1; i <= 10; i++ {
		f.Segments = append(f.Segments, nzb.Segment{Number: i, MessageID: "x"})
	}
	pf := newPostedFile(0, f, 0, nil)
	if got := pf.expectedPart(0); got != (core.ArticlePart{Part: 1, Total: 10}) {
		t.Fatalf("unlearned: %+v", got)
	}
	pf.learn(2, 200, 300, 1000, 10)
	want := core.ArticlePart{Part: 3, Total: 10, FileSize: 1000, Begin: 201, End: 300}
	if got := pf.expectedPart(2); got != want {
		t.Fatalf("learned: %+v, want %+v", got, want)
	}
	if got := pf.expectedPart(4); got.FileSize != 1000 || got.Begin != 0 || got.End != 0 {
		t.Fatalf("size learned, range not: %+v", got)
	}
}

func TestBodyTakedownCancelsSlowHeaderAndStatProbes(t *testing.T) {
	f := newFakeFetcher()
	doc := post(t, f, 10000, postFile{name: "taken.mkv", data: mkvBytes(1, 1000000)})
	e := newTestEngine(t, bodyUnavailableFetcher{f}, nil, Options{HealthSamples: 12, HealthParallel: 12})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	started := time.Now()
	ok, err := e.Check(ctx, doc)
	if ok || !errors.Is(err, core.ErrArticleMissing) {
		t.Fatalf("BODY takedown: %v %v", ok, err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("waited for slow header/STAT instead of cancelling")
	}
}

func TestCheckRejectsAGapInTheFirstArticles(t *testing.T) {
	f := newFakeFetcher()
	doc := post(t, f, 10_000, postFile{name: "gap.mkv", data: mkvBytes(1, 1_000_000)})
	// Article 6 (about 60 KB in) is missing: the spread sample skips it,
	// but playback would hit it in its first second.
	f.mu.Lock()
	f.missing[postID("gap.mkv", 7)] = true
	f.mu.Unlock()
	e := newTestEngine(t, f, nil, Options{HealthSamples: 12})
	if ok, err := e.Check(context.Background(), doc); ok || !errors.Is(err, core.ErrArticleMissing) {
		t.Fatalf("Check = %v %v, want a missing-article rejection", ok, err)
	}
}

func TestHealthPriorityFollowsContext(t *testing.T) {
	f := newFakeFetcher()
	doc := post(t, f, 10_000, postFile{name: "h.mkv", data: mkvBytes(1, 100_000)})
	e := newTestEngine(t, f, nil, Options{HealthSamples: 4})
	if ok, err := e.Check(context.Background(), doc); !ok || err != nil {
		t.Fatalf("foreground Check = %v %v", ok, err)
	}
	if live, bg := f.prio(core.PriorityLive), f.prio(core.PriorityPrefetch); live == 0 || bg != 0 {
		t.Fatalf("foreground priorities: live %d prefetch %d", live, bg)
	}
	f.resetCounts()
	ctx := core.WithBackgroundBudget(context.Background(), func(context.Context, string, string, int64) error { return nil })
	if ok, err := e.Check(ctx, doc); !ok || err != nil {
		t.Fatalf("background Check = %v %v", ok, err)
	}
	if live, bg := f.prio(core.PriorityLive), f.prio(core.PriorityPrefetch); live != 0 || bg == 0 {
		t.Fatalf("background priorities: live %d prefetch %d", live, bg)
	}
}
