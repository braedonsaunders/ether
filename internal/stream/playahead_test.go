package stream

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/segcache"
)

// stallFetcher is a slow provider for background work: every
// PriorityPrefetch article waits for release (or cancellation), while demand
// and near read-ahead articles are served at once.
type stallFetcher struct {
	*fakeFetcher
	release  chan struct{}
	stalled  atomic.Int64 // prefetch articles waiting now
	canceled atomic.Int64 // prefetch articles abandoned by cancellation
}

func (f *stallFetcher) BodyPart(ctx context.Context, id string, groups []string, prio core.Priority) ([]byte, core.ArticlePart, error) {
	if prio == core.PriorityPrefetch {
		f.stalled.Add(1)
		select {
		case <-f.release:
			f.stalled.Add(-1)
		case <-ctx.Done():
			f.stalled.Add(-1)
			f.canceled.Add(1)
			return nil, core.ArticlePart{}, ctx.Err()
		}
	}
	return f.fakeFetcher.BodyPart(ctx, id, groups, prio)
}

const (
	playPart = 256 << 10
	playSize = 24 << 20
	// 24 MiB over 24 minutes: one MiB per minute of media.
	playDuration = 24 * time.Minute
)

func playEngine(t *testing.T, f core.ArticleFetcher, cache core.SegmentCache) *Engine {
	t.Helper()
	return newTestEngine(t, f, cache, Options{
		HotBufferBytes: 8 << 20, ReadAheadMin: 1 << 20, ReadAheadMax: 1 << 20,
		PlaybackReadAhead: 5 * time.Minute,
	})
}

func openCache(t *testing.T, budget int64) *segcache.Cache {
	t.Helper()
	c, err := segcache.Open(t.TempDir(), budget, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// playRead reads [0, n) sequentially like a player.
func playRead(t *testing.T, src *source, n int64) {
	t.Helper()
	buf := make([]byte, playPart)
	for off := int64(0); off < n; off += int64(len(buf)) {
		if _, err := src.ReadAt(buf, off); err != nil {
			t.Fatalf("read at %d: %v", off, err)
		}
	}
}

// onDisk reports whether every segment of [off, off+n) is in the cache.
func onDisk(src *source, c *segcache.Cache, off, n int64) bool {
	for _, pc := range src.plan.fetchPieces(off, n) {
		pf := src.plan.vols[pc.vol]
		for _, i := range pf.slotsFor(pc.off, pc.n) {
			if !c.Has(src.releaseID, pf.segBase+i) {
				return false
			}
		}
	}
	return true
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	// Generous: CI runners (macOS, -race) are many times slower than a
	// workstation; the condition returns as soon as it holds.
	deadline := time.Now().Add(60 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// playCovered is what the loop guarantees on disk ahead of a reader that
// stopped at next. It replans only once the target has moved a low-water mark
// past what it covered, so a reader that stops leaves coverage anywhere in
// (target-lowWater, target], depending on where the reader was when the last
// batch was planned: one planned at 2.75 MiB, with the reader then stopping at
// 3 MiB, leaves the target 256 KiB short, under the mark, for good.
func playCovered(src *source, next int64) int64 {
	depth := src.playAheadBytes()
	return next + depth - min(int64(playLowWater), depth/8)
}

func TestPlaybackReadAheadKeepsMinutesOnDisk(t *testing.T) {
	f := newFakeFetcher()
	f.latency = 3 * time.Millisecond // a slow provider
	doc := post(t, f, playPart, postFile{name: "play.mkv", data: mkvBytes(31, playSize)})
	cache := openCache(t, 1<<30)
	e := playEngine(t, f, cache)
	src := openSource(t, e, "play", doc).(*source)

	// Without a running time (prewarm, detail pages) nothing is read ahead.
	playRead(t, src, 2<<20)
	time.Sleep(50 * time.Millisecond)
	if n := f.prio(core.PriorityPrefetch); n != 0 {
		t.Fatalf("read ahead %d articles before playback reported a duration", n)
	}

	src.SetPlaybackDuration(playDuration)
	playRead(t, src, 3<<20) // three minutes in
	// Five minutes ahead of the reader (less the low-water mark), past the
	// 1 MiB in-memory window.
	covered := playCovered(src, 3<<20)
	if covered <= 7<<20 {
		t.Fatalf("covered target %d is not minutes past the in-memory window", covered)
	}
	waitFor(t, "five minutes ahead on disk", func() bool { return onDisk(src, cache, 4<<20, covered-4<<20) })
	if f.prio(core.PriorityPrefetch) == 0 {
		t.Fatal("playback read-ahead did not use the background lane")
	}
	time.Sleep(100 * time.Millisecond)
	// Bounded: nothing past five minutes (plus the part straddling it).
	for n := (8<<20)/playPart + 2; n <= playSize/playPart; n++ {
		if c := f.calls(postID("play.mkv", n)); c != 0 {
			t.Fatalf("part %d (past the target) fetched %d times", n, c)
		}
	}

	// The target follows the reader.
	playRead(t, src, 6<<20)
	covered = playCovered(src, 6<<20)
	if covered <= 8<<20 {
		t.Fatalf("covered target %d does not pass the previous target", covered)
	}
	waitFor(t, "target to follow the reader", func() bool { return onDisk(src, cache, 8<<20, covered-8<<20) })
}

func TestPlaybackReadAheadNeverDelaysDemandReads(t *testing.T) {
	f := &stallFetcher{fakeFetcher: newFakeFetcher(), release: make(chan struct{})}
	defer close(f.release)
	doc := post(t, f.fakeFetcher, playPart, postFile{name: "demand.mkv", data: mkvBytes(32, playSize)})
	e := playEngine(t, f, openCache(t, 1<<30))
	src := openSource(t, e, "demand", doc).(*source)
	src.SetPlaybackDuration(playDuration)
	playRead(t, src, 1<<20)
	waitFor(t, "read-ahead stalled on the provider", func() bool { return f.stalled.Load() > 0 })

	buf := make([]byte, playPart)
	// Continuing playback into bytes the stalled read-ahead is fetching,
	// and seeking far away, are both served at once.
	for _, off := range []int64{1 << 20, 3 << 20, 20 << 20} {
		start := time.Now()
		if _, err := src.ReadAt(buf, off); err != nil {
			t.Fatalf("read at %d: %v", off, err)
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Fatalf("demand read at %d waited %v behind read-ahead", off, d)
		}
	}
	if f.stalled.Load() == 0 {
		t.Fatal("the stall ended early; the test proved nothing")
	}
}

func TestPlaybackReadAheadStopsOnClose(t *testing.T) {
	f := &stallFetcher{fakeFetcher: newFakeFetcher(), release: make(chan struct{})}
	defer close(f.release)
	doc := post(t, f.fakeFetcher, playPart, postFile{name: "close.mkv", data: mkvBytes(33, playSize)})
	e := playEngine(t, f, openCache(t, 1<<30))
	src := openSource(t, e, "close", doc).(*source)
	src.SetPlaybackDuration(playDuration)
	playRead(t, src, 1<<20)
	waitFor(t, "read-ahead running", func() bool { return f.stalled.Load() > 0 })

	done := make(chan struct{})
	go func() { _ = src.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close waited for stalled read-ahead")
	}
	waitFor(t, "stalled fetches cancelled", func() bool { return f.stalled.Load() == 0 })
	if f.canceled.Load() == 0 {
		t.Fatal("stalled read-ahead was not cancelled")
	}
	before := f.prio(core.PriorityPrefetch)
	src.kickPlayAhead()
	time.Sleep(50 * time.Millisecond)
	if after := f.prio(core.PriorityPrefetch); after != before {
		t.Fatalf("read-ahead fetched %d articles after Close", after-before)
	}
	running := 0
	e.playSources.Range(func(any, any) bool { running++; return true })
	if running != 0 {
		t.Fatalf("%d read-ahead loops still running", running)
	}
}

func TestPlaybackReadAheadRespectsCacheLimit(t *testing.T) {
	f := newFakeFetcher()
	doc := post(t, f, playPart, postFile{name: "budget.mkv", data: mkvBytes(34, playSize)})
	// A 8 MiB budget leaves 2 MiB for read-ahead, not the 5 MiB asked for.
	cache := openCache(t, 8<<20)
	e := playEngine(t, f, cache)
	src := openSource(t, e, "budget", doc).(*source)
	src.SetPlaybackDuration(playDuration)
	if got := src.playAheadBytes(); got != 2<<20 {
		t.Fatalf("read-ahead depth = %d, want the 2 MiB cache share", got)
	}
	playRead(t, src, 2<<20)
	waitFor(t, "budgeted read-ahead", func() bool {
		src.mu.Lock()
		defer src.mu.Unlock()
		return len(src.cursors) > 0 && src.cursors[0].playNext >= 4<<20
	})
	if got := src.BufferedAhead(); got < 2<<20 {
		t.Fatalf("buffered ahead = %d, want at least the 2 MiB read-ahead", got)
	}
	time.Sleep(100 * time.Millisecond)
	for n := (4<<20)/playPart + 2; n <= playSize/playPart; n++ {
		if c := f.calls(postID("budget.mkv", n)); c != 0 {
			t.Fatalf("part %d (past the cache share) fetched %d times", n, c)
		}
	}

	// Free space below the floor's margin shrinks it further.
	cache.SetDiskUsage(func() (int64, int64, error) {
		const total = 100 << 30
		return total, segcache.MinFreeBytes(total) + 4<<20, nil
	})
	if got, limit := src.playAheadBytes(), cache.UsageLimit(); limit > 8<<20 || got != limit/4 {
		t.Fatalf("read-ahead depth = %d with cache limit %d", got, limit)
	}
	// A cache that retains nothing turns it off; so does the owner setting.
	cache.SetPolicy(segcache.Policy{Mode: "off"})
	if got := src.playAheadBytes(); got != 0 {
		t.Fatalf("read-ahead depth = %d with media caching off", got)
	}
	cache.SetPolicy(segcache.Policy{})
	e.UpdatePlaybackReadAhead(0)
	if got := src.playAheadBytes(); got != 0 {
		t.Fatalf("read-ahead depth = %d when turned off", got)
	}
}

// A paused viewer's source keeps its loop until the session closes, but it
// must not shrink the read-ahead of viewers still playing (Codex review on
// #274): only sources read within playIdle share the cache allowance.
func TestPausedSourceDoesNotShrinkOthersReadAhead(t *testing.T) {
	f := newFakeFetcher()
	docA := post(t, f, playPart, postFile{name: "a.mkv", data: mkvBytes(35, playSize)})
	docB := post(t, f, playPart, postFile{name: "b.mkv", data: mkvBytes(36, playSize)})
	cache := openCache(t, 8<<20) // 2 MiB of read-ahead for one viewer
	e := playEngine(t, f, cache)
	a := openSource(t, e, "a", docA).(*source)
	b := openSource(t, e, "b", docB).(*source)
	for _, src := range []*source{a, b} {
		src.SetPlaybackDuration(playDuration)
		playRead(t, src, 1<<20)
	}
	waitFor(t, "both read-ahead loops", func() bool {
		n := 0
		e.playSources.Range(func(any, any) bool { n++; return true })
		return n == 2
	})
	if got := a.playAheadBytes(); got != 1<<20 {
		t.Fatalf("two playing viewers: depth %d, want half the 2 MiB share", got)
	}
	b.lastRead.Store(time.Now().Add(-2 * playIdle).UnixNano()) // paused
	if got := a.playAheadBytes(); got != 2<<20 {
		t.Fatalf("other viewer paused: depth %d, want the whole 2 MiB share", got)
	}
	playRead(t, b, 1<<20) // resumed
	if got := a.playAheadBytes(); got != 1<<20 {
		t.Fatalf("other viewer resumed: depth %d, want half again", got)
	}
}
