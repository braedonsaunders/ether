package stream

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/nzb"
	"github.com/braedonsaunders/ether/internal/segcache"
)

func TestFileIndexEstimatesAndCorrections(t *testing.T) {
	// 10 parts listed; real sizes non-uniform: 100 x5 then 90 x5 (last 90).
	f := &nzb.File{Name: "x.mkv"}
	for n := 1; n <= 10; n++ {
		f.Segments = append(f.Segments, nzb.Segment{Number: n, MessageID: "m"})
	}
	pf := newPostedFile(0, f, 0, nil)
	// Learn part 1: [0,100), file size 950.
	pf.learn(0, 0, 100, 950, 10)
	if pf.Size() != 950 || !pf.SizeKnown() {
		t.Fatalf("size = %d", pf.Size())
	}
	if b, e, exact := pf.rangeOf(3); b != 300 || e != 400 || exact {
		t.Fatalf("estimate part 4 = [%d,%d) %v", b, e, exact)
	}
	if b, e, _ := pf.rangeOf(9); b != 900 || e != 950 {
		t.Fatalf("estimate last = [%d,%d)", b, e)
	}
	if got := pf.locate(450); got != 4 {
		t.Fatalf("locate(450) = %d", got)
	}
	// Learn the real range of part 7: [590, 680) — the post shrank after part 5.
	if changed := pf.learn(6, 590, 680, 950, 10); !changed {
		t.Fatal("learn did not report a change")
	}
	// Parts between anchors 1 and 7 are estimated from part 1 and clamped.
	if b, _, _ := pf.rangeOf(5); b != 500 {
		t.Fatalf("part 6 begin = %d", b)
	}
	if _, e, _ := pf.rangeOf(5); e != 590 {
		t.Fatalf("part 6 end = %d (clamped to the next anchor)", e)
	}
	// Parts after anchor 7 follow it.
	if b, e, _ := pf.rangeOf(7); b != 680 || e != 780 {
		t.Fatalf("part 8 = [%d,%d)", b, e)
	}
	if got := pf.locate(685); got != 7 {
		t.Fatalf("locate(685) = %d", got)
	}
	if got := pf.slotsFor(580, 120); len(got) != 3 || got[0] != 5 || got[2] != 7 {
		t.Fatalf("slotsFor = %v", got)
	}
	// Total larger than listed: the slot table grows (tail missing).
	pf.learn(0, 0, 100, 950, 12)
	if pf.numSlots() != 12 || pf.msgID(11) != "" {
		t.Fatalf("slots = %d", pf.numSlots())
	}
}

func TestPlainRandomAccess(t *testing.T) {
	f := newFakeFetcher()
	data := mkvBytes(1, 3_000_017)
	doc := post(t, f, 100_003,
		postFile{name: "Movie.2024.1080p.mkv", data: data},
		postFile{name: "Movie.2024.nfo", data: []byte("info")},
		postFile{name: "Movie.2024.sample.mkv", data: mkvBytes(2, 400_000)},
		postFile{name: "Movie.2024.par2", data: randBytes(3, 5000)},
	)
	e := newTestEngine(t, f, nil, Options{})
	src := openSource(t, e, "rel-1", doc)
	if src.Size() != int64(len(data)) {
		t.Fatalf("size = %d", src.Size())
	}
	if src.Name() != "Movie.2024.1080p.mkv" || src.ReleaseID() != "rel-1" {
		t.Fatalf("name = %q id = %q", src.Name(), src.ReleaseID())
	}
	// Open fetched exactly one article.
	if c := f.totalCalls(); c != 1 {
		t.Fatalf("Open fetched %d articles, want 1", c)
	}
	checkRandomReads(t, src, data, 11, 200)
	// Boundaries: last byte, read past the end, at the end.
	buf := make([]byte, 10)
	if n, err := src.ReadAt(buf, int64(len(data))-3); n != 3 || err != io.EOF || !bytes.Equal(buf[:3], data[len(data)-3:]) {
		t.Fatalf("tail read = %d %v", n, err)
	}
	if n, err := src.ReadAt(buf, int64(len(data))); n != 0 || err != io.EOF {
		t.Fatalf("read at end = %d %v", n, err)
	}
	if _, err := src.ReadAt(buf, -1); err == nil {
		t.Fatal("negative offset accepted")
	}
	// Each article fetched at most once thanks to the hot buffer.
	for n := 1; n <= 31; n++ {
		if c := f.maxLaneCalls(postID("Movie.2024.1080p.mkv", n)); c > 1 {
			t.Fatalf("part %d fetched %d times in one priority lane", n, c)
		}
	}
}

func TestNonUniformPartsAreCorrected(t *testing.T) {
	f := newFakeFetcher()
	data := mkvBytes(4, 1_000_000)
	// Irregular posting: sizes vary wildly; only part 1 is "typical".
	sizes := []int{100_000, 37_000, 150_000, 99_999, 1, 200_000, 50_000, 150_000, 100_000, 113_000}
	sum := 0
	for _, s := range sizes {
		sum += s
	}
	if sum != len(data) {
		t.Fatalf("sizes sum %d", sum)
	}
	doc := post(t, f, 0, postFile{name: "odd.mkv", data: data, parts: sizes})
	e := newTestEngine(t, f, nil, Options{ReadAheadParallel: 1})
	src := openSource(t, e, "odd", doc)
	checkRandomReads(t, src, data, 5, 300)
	got := readAll(t, src, 7_777)
	if !bytes.Equal(got, data) {
		t.Fatal("sequential read mismatch")
	}
}

func TestNoYEncHeaderFallsBackToEstimates(t *testing.T) {
	f := newFakeFetcher()
	f.noHeader = true
	data := mkvBytes(5, 450_000)
	doc := post(t, f, 100_000, postFile{name: "nohdr.mkv", data: data})
	e := newTestEngine(t, f, nil, Options{})
	src := openSource(t, e, "nohdr", doc)
	if src.Size() != int64(len(data)) {
		t.Fatalf("size = %d, want %d", src.Size(), len(data))
	}
	if got := readAll(t, src, 64<<10); !bytes.Equal(got, data) {
		t.Fatal("mismatch")
	}
}

func TestMissingArticleSurfacesFromReadAt(t *testing.T) {
	f := newFakeFetcher()
	data := mkvBytes(6, 1_000_000)
	doc := post(t, f, 100_000, postFile{name: "m.mkv", data: data})
	f.setMissing(postID("m.mkv", 5)) // bytes [400000, 500000)
	e := newTestEngine(t, f, nil, Options{ReadAheadParallel: 1})
	src := openSource(t, e, "m", doc)

	buf := make([]byte, 1000)
	if _, err := src.ReadAt(buf, 0); err != nil {
		t.Fatalf("read before hole: %v", err)
	}
	n, err := src.ReadAt(make([]byte, 200_000), 350_000)
	if !errors.Is(err, core.ErrArticleMissing) {
		t.Fatalf("err = %v, want ErrArticleMissing", err)
	}
	if n != 50_000 {
		t.Fatalf("partial read = %d, want 50000 bytes before the hole", n)
	}
	if _, err := src.ReadAt(buf, 600_000); err != nil {
		t.Fatalf("read after hole: %v", err)
	}
}

func TestOpenRejectsIncompleteNZB(t *testing.T) {
	f := newFakeFetcher()
	doc := post(t, f, 100_000, postFile{name: "gap.mkv", data: mkvBytes(7, 500_000), omit: []int{3}})
	e := newTestEngine(t, f, nil, Options{})
	_, err := e.Open(context.Background(), "gap", doc)
	if !errors.Is(err, ErrIncomplete) || !errors.Is(err, core.ErrArticleMissing) {
		t.Fatalf("err = %v", err)
	}
	if !IsRejection(err) {
		t.Fatal("incomplete NZB is not a rejection")
	}
}

func TestOpenErrors(t *testing.T) {
	f := newFakeFetcher()
	e := newTestEngine(t, f, nil, Options{})
	if _, err := e.Open(context.Background(), "x", []byte("garbage")); !errors.Is(err, ErrInvalidNZB) {
		t.Fatalf("invalid nzb err = %v", err)
	}
	doc := post(t, f, 1000,
		postFile{name: "readme.nfo", data: []byte("hello")},
		postFile{name: "x.sample.mkv", data: mkvBytes(1, 3000)},
	)
	if _, err := e.Open(context.Background(), "x", doc); !errors.Is(err, ErrNoPlayableFile) {
		t.Fatalf("no video err = %v", err)
	}
	// The first article being gone is a missing-article error.
	doc = post(t, f, 1000, postFile{name: "first.mkv", data: mkvBytes(2, 3000)})
	f.setMissing(postID("first.mkv", 1))
	if _, err := e.Open(context.Background(), "y", doc); !errors.Is(err, core.ErrArticleMissing) {
		t.Fatalf("missing first err = %v", err)
	}
}

func TestTransientFailuresAreRetried(t *testing.T) {
	f := newFakeFetcher()
	data := mkvBytes(8, 300_000)
	doc := post(t, f, 100_000, postFile{name: "flaky.mkv", data: data})
	f.failures[postID("flaky.mkv", 2)] = 2
	e := newTestEngine(t, f, nil, Options{Retries: 2})
	src := openSource(t, e, "flaky", doc)
	if got := readAll(t, src, 50_000); !bytes.Equal(got, data) {
		t.Fatal("mismatch")
	}
	// The first source can still prefetch after readAll returns. Give the
	// exhausted-retry scenario its own fetcher so neither its map mutation
	// nor its failure budget is shared with those background reads.
	f2 := newFakeFetcher()
	doc = post(t, f2, 100_000, postFile{name: "flaky.mkv", data: data})
	f2.failures[postID("flaky.mkv", 3)] = 5
	e2 := newTestEngine(t, f2, nil, Options{Retries: 1})
	src2 := openSource(t, e2, "flaky", doc)
	if _, err := src2.ReadAt(make([]byte, 10), 250_000); err == nil || errors.Is(err, core.ErrArticleMissing) {
		t.Fatalf("persistent failure err = %v", err)
	}
}

func TestSplitAcrossVolumes(t *testing.T) {
	f := newFakeFetcher()
	data := mkvBytes(9, 2_500_000)
	vol := 1_000_000
	doc := post(t, f, 99_991,
		postFile{name: "Movie.mkv.001", data: data[:vol]},
		postFile{name: "Movie.mkv.003", data: data[2*vol:]},
		postFile{name: "Movie.mkv.002", data: data[vol : 2*vol]},
	)
	e := newTestEngine(t, f, nil, Options{})
	src := openSource(t, e, "split", doc)
	if src.Name() != "Movie.mkv" || src.Size() != int64(len(data)) {
		t.Fatalf("name %q size %d", src.Name(), src.Size())
	}
	checkRandomReads(t, src, data, 3, 150)
	// A read spanning both boundaries.
	buf := make([]byte, 1_200_000)
	if n, err := src.ReadAt(buf, 900_000); err != nil || n != len(buf) || !bytes.Equal(buf, data[900_000:2_100_000]) {
		t.Fatalf("spanning read = %d %v", n, err)
	}
}

func TestSplitWithoutInnerExtensionIsSniffed(t *testing.T) {
	f := newFakeFetcher()
	data := mkvBytes(40, 900_000)
	doc := post(t, f, 100_000,
		postFile{name: "Movie.2024.001", data: data[:500_000]},
		postFile{name: "Movie.2024.002", data: data[500_000:]},
		postFile{name: "Other.iso.001", data: randBytes(1, 2_000_000)},
	)
	e := newTestEngine(t, f, nil, Options{})
	src := openSource(t, e, "split2", doc)
	if src.Name() != "Movie.2024" || src.Size() != int64(len(data)) {
		t.Fatalf("name %q size %d", src.Name(), src.Size())
	}
	if got := readAll(t, src, 64<<10); !bytes.Equal(got, data) {
		t.Fatal("mismatch")
	}
}

func TestCoalescesConcurrentReads(t *testing.T) {
	f := newFakeFetcher()
	f.latency = 20 * time.Millisecond
	data := mkvBytes(10, 800_000)
	doc := post(t, f, 100_000, postFile{name: "c.mkv", data: data})
	e := newTestEngine(t, f, nil, Options{ReadAheadParallel: 1})
	src := openSource(t, e, "c", doc)
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for g := 0; g < 32; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, 300_000)
			n, err := src.ReadAt(buf, 250_000)
			if err != nil || !bytes.Equal(buf[:n], data[250_000:550_000]) {
				errs <- errors.New("bad concurrent read")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	for n := 1; n <= 8; n++ {
		if c := f.maxLaneCalls(postID("c.mkv", n)); c > 1 {
			t.Fatalf("part %d fetched %d times in one priority lane", n, c)
		}
	}
	if e.Stats().Coalesced == 0 {
		t.Fatal("no coalesced requests recorded")
	}
}

func TestReadAheadSequential(t *testing.T) {
	f := newFakeFetcher()
	f.latency = 5 * time.Millisecond
	data := mkvBytes(11, 6_000_000)
	doc := post(t, f, 100_000, postFile{name: "ra.mkv", data: data})
	e := newTestEngine(t, f, nil, Options{ReadAheadMin: 500_000, ReadAheadMax: 2_000_000, ReadAheadParallel: 8})
	src := openSource(t, e, "ra", doc)
	f.resetCounts()

	// Read the first 32 KiB and give read-ahead a moment.
	buf := make([]byte, 32<<10)
	if _, err := src.ReadAt(buf, 0); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for f.calls(postID("ra.mkv", 5)) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if f.calls(postID("ra.mkv", 5)) == 0 {
		t.Fatal("read-ahead did not fetch ahead of the reader")
	}
	if f.prio(core.PriorityReadahead) == 0 {
		t.Fatal("no fetches at read-ahead priority")
	}
	// It must not run away past the window.
	if c := f.calls(postID("ra.mkv", 30)); c != 0 {
		t.Fatal("read-ahead fetched far beyond its window")
	}
	// Sequential read of the whole file: every part fetched exactly once,
	// and most of them ahead of the reader.
	got := readAll(t, src, 32<<10)
	if !bytes.Equal(got, data) {
		t.Fatal("mismatch")
	}
	for n := 1; n <= 60; n++ {
		if c := f.maxLaneCalls(postID("ra.mkv", n)); c != 1 && n != 1 {
			t.Fatalf("part %d fetched %d times in one priority lane", n, c)
		}
	}
	if live, ra := f.prio(core.PriorityLive), f.prio(core.PriorityReadahead); ra < live {
		t.Fatalf("read-ahead fetched %d parts, live %d: reader mostly waited", ra, live)
	}
}

func TestSeekStartsNewCursor(t *testing.T) {
	f := newFakeFetcher()
	data := mkvBytes(12, 5_000_000)
	doc := post(t, f, 100_000, postFile{name: "seek.mkv", data: data})
	e := newTestEngine(t, f, nil, Options{ReadAheadMin: 300_000, ReadAheadMax: 300_000})
	src := openSource(t, e, "seek", doc)
	buf := make([]byte, 1000)
	if _, err := src.ReadAt(buf, 4_000_000); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for f.calls(postID("seek.mkv", 43)) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if f.calls(postID("seek.mkv", 43)) == 0 {
		t.Fatal("no read-ahead after a seek")
	}
}

func TestCloseStopsReads(t *testing.T) {
	f := newFakeFetcher()
	doc := post(t, f, 1000, postFile{name: "close.mkv", data: mkvBytes(13, 10_000)})
	e := newTestEngine(t, f, nil, Options{})
	src, err := e.Open(context.Background(), "close", doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := src.Close(); err != nil {
		t.Fatal(err)
	}
	if err := src.Close(); err != nil {
		t.Fatal("second close:", err)
	}
	if _, err := src.ReadAt(make([]byte, 10), 0); !errors.Is(err, ErrClosed) {
		t.Fatalf("read after close = %v", err)
	}
	if e.Stats().ActiveSources != 0 {
		t.Fatal("source still counted active")
	}
}

func TestReopenReusesIndex(t *testing.T) {
	f := newFakeFetcher()
	data := mkvBytes(14, 500_000)
	doc := post(t, f, 100_000, postFile{name: "re.mkv", data: data})
	e := newTestEngine(t, f, nil, Options{})
	src := openSource(t, e, "re", doc)
	_ = src.Close()
	f.resetCounts()
	src2 := openSource(t, e, "re", doc)
	if f.totalCalls() != 0 {
		t.Fatalf("re-open fetched %d articles", f.totalCalls())
	}
	if src2.Size() != int64(len(data)) {
		t.Fatal("size")
	}
}

func TestPrefetchWarmsDiskCache(t *testing.T) {
	dir := t.TempDir()
	cache, err := segcache.Open(dir, 1<<30, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	f := newFakeFetcher()
	data := mkvBytes(15, 2_000_000)
	doc := post(t, f, 100_000, postFile{name: "pre.mkv", data: data})
	e := newTestEngine(t, f, cache, Options{})
	if err := e.Prefetch(context.Background(), "rel-pre", doc, 0, 1_000_000); err != nil {
		t.Fatal(err)
	}
	if f.prio(core.PriorityPrefetch) == 0 || f.prio(core.PriorityLive) != 0 {
		t.Fatalf("prefetch priorities: prefetch %d live %d", f.prio(core.PriorityPrefetch), f.prio(core.PriorityLive))
	}
	if used, _ := cache.Usage(); used < 1_000_000 {
		t.Fatalf("cache holds %d bytes", used)
	}
	// Prefetch again: nothing new is fetched.
	f.resetCounts()
	if err := e.Prefetch(context.Background(), "rel-pre", doc, 0, 1_000_000); err != nil {
		t.Fatal(err)
	}
	if f.totalCalls() != 0 {
		t.Fatalf("second prefetch fetched %d", f.totalCalls())
	}

	// A fresh engine (empty memory) with the network down serves the
	// prefetched range from disk.
	f.failAll = true
	e2 := newTestEngine(t, f, cache, Options{ReadAheadParallel: 1})
	src := openSource(t, e2, "rel-pre", doc)
	buf := make([]byte, 900_000)
	if n, err := src.ReadAt(buf, 50_000); err != nil || !bytes.Equal(buf[:n], data[50_000:950_000]) {
		t.Fatalf("cached read = %d %v", n, err)
	}
	if e2.Stats().CacheHits == 0 {
		t.Fatal("no cache hits")
	}
	if _, err := src.ReadAt(buf, 1_500_000); err == nil {
		t.Fatal("uncached read succeeded with the network down")
	}
}

func TestBackgroundReadAtUsesPrefetchPriority(t *testing.T) {
	f := newFakeFetcher()
	data := mkvBytes(16, 2_000_000)
	doc := post(t, f, 100_000, postFile{name: "background.mkv", data: data})
	e := newTestEngine(t, f, nil, Options{})
	src := openSource(t, e, "rel-background", doc)
	f.resetCounts()
	cr, ok := src.(interface {
		ReadAtPrefetchContext(context.Context, []byte, int64) (int, error)
	})
	if !ok {
		t.Fatal("source does not expose background read path")
	}
	buf := make([]byte, 64)
	if n, err := cr.ReadAtPrefetchContext(context.Background(), buf, 1_000_000); n != len(buf) || err != nil {
		t.Fatalf("background read = %d, %v", n, err)
	}
	if live, bg := f.prio(core.PriorityLive), f.prio(core.PriorityPrefetch); live != 0 || bg == 0 {
		t.Fatalf("background read priorities: live %d, prefetch %d", live, bg)
	}
}

func TestLiveReadsSpillToCache(t *testing.T) {
	cache, err := segcache.Open(t.TempDir(), 1<<30, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	f := newFakeFetcher()
	data := mkvBytes(16, 400_000)
	doc := post(t, f, 100_000, postFile{name: "spill.mkv", data: data})
	e := newTestEngine(t, f, cache, Options{})
	src := openSource(t, e, "spill", doc)
	_ = readAll(t, src, 100_000)
	// Live reads publish bytes before persisting them. Join the engine's
	// pending writes instead of imposing a disk-speed deadline on CI.
	if err := src.Close(); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if used, _ := cache.Usage(); used < int64(len(data)) {
		t.Fatalf("cache holds %d bytes", used)
	}
}

func TestSegmentEncoding(t *testing.T) {
	s := &segData{data: []byte("hello"), begin: 1234567890123, fileSize: 9876543210, total: 4242, name: "x.mkv"}
	got, ok := decodeSegment(encodeSegment(s))
	if !ok || !bytes.Equal(got.data, s.data) || got.begin != s.begin || got.fileSize != s.fileSize || got.total != s.total || got.name != s.name {
		t.Fatalf("round trip = %+v %v", got, ok)
	}
	if _, ok := decodeSegment([]byte("nope")); ok {
		t.Fatal("decoded garbage")
	}
}

func TestHotBufferBounded(t *testing.T) {
	h := newHotBuffer(1000)
	for i := 0; i < 50; i++ {
		h.put(segKey{"r", i}, &segData{data: make([]byte, 100)})
	}
	if h.usage() > 1000 {
		t.Fatalf("usage %d", h.usage())
	}
	if _, ok := h.get(segKey{"r", 49}); !ok {
		t.Fatal("newest evicted")
	}
	if _, ok := h.get(segKey{"r", 0}); ok {
		t.Fatal("oldest kept")
	}
	h.dropRelease("r")
	if h.usage() != 0 {
		t.Fatal("drop")
	}
}

func TestObfuscatedPlainPostUsesYEncName(t *testing.T) {
	f := newFakeFetcher()
	data := mkvBytes(17, 600_000)
	doc := post(t, f, 100_000,
		postFile{name: "a8f7e6d5c4b3a2f1e0d9c8b7a6f5e4d3", yName: "Real.Movie.2024.mkv", data: data},
		postFile{name: "b8f7e6d5c4b3a2f1e0d9c8b7a6f5e4d3", yName: "Real.Movie.2024.nfo", data: []byte("nfo")},
	)
	e := newTestEngine(t, f, nil, Options{})
	src := openSource(t, e, "obf", doc)
	if src.Name() != "Real.Movie.2024.mkv" || src.Size() != int64(len(data)) {
		t.Fatalf("name %q size %d", src.Name(), src.Size())
	}
	checkRandomReads(t, src, data, 1, 50)
}

func TestObfuscatedContentSniffed(t *testing.T) {
	f := newFakeFetcher()
	data := mkvBytes(18, 600_000)
	doc := post(t, f, 100_000,
		postFile{name: "c8f7e6d5c4b3a2f1e0d9c8b7a6f5e4d3", data: data},
		postFile{name: "d8f7e6d5c4b3a2f1e0d9c8b7a6f5e4d3", data: randBytes(1, 2000)},
	)
	e := newTestEngine(t, f, nil, Options{})
	src := openSource(t, e, "obf2", doc)
	if src.Size() != int64(len(data)) {
		t.Fatalf("size %d", src.Size())
	}
	if got := readAll(t, src, 64<<10); !bytes.Equal(got, data) {
		t.Fatal("mismatch")
	}
}

func TestReleaseCacheBounded(t *testing.T) {
	f := newFakeFetcher()
	e := newTestEngine(t, f, nil, Options{MaxReleases: 2})
	var keep []core.MediaSource
	for i := 0; i < 5; i++ {
		doc := post(t, f, 1000, postFile{name: "r" + string(rune('a'+i)) + ".mkv", data: mkvBytes(uint64(i), 3000)})
		src := openSource(t, e, "r", doc)
		if i == 0 {
			keep = append(keep, src) // stays open: never dropped
		} else {
			_ = src.Close()
		}
	}
	if n := e.Stats().OpenReleases; n > 2 {
		t.Fatalf("holding %d releases", n)
	}
	if _, err := keep[0].ReadAt(make([]byte, 100), 0); err != nil {
		t.Fatal(err)
	}
}

func TestEngineCloseCancels(t *testing.T) {
	f := newFakeFetcher()
	f.latency = time.Hour
	doc := post(t, f, 1000, postFile{name: "slow.mkv", data: mkvBytes(1, 3000)})
	e := New(f, nil, Options{}, nil)
	done := make(chan error, 1)
	go func() {
		_, err := e.Open(context.Background(), "slow", doc)
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	_ = e.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("open succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not cancel the fetch")
	}
	if _, err := e.Open(context.Background(), "slow", doc); !errors.Is(err, ErrClosed) {
		t.Fatalf("open after close = %v", err)
	}
}

func TestOpenContextCancel(t *testing.T) {
	f := newFakeFetcher()
	f.latency = time.Hour
	doc := post(t, f, 1000, postFile{name: "slow.mkv", data: mkvBytes(1, 3000)})
	e := newTestEngine(t, f, nil, Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := e.Open(ctx, "slow", doc); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	// Not cached as a rejection: a later open with patience works.
	f.mu.Lock()
	f.latency = 0
	f.mu.Unlock()
	if _, err := e.Open(context.Background(), "slow", doc); err != nil {
		t.Fatalf("retry: %v", err)
	}
}
