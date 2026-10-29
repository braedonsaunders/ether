package debrid

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

const testBlock = 64 << 10

type engineRig struct {
	*harness
	eng   *Engine
	cache *memCache
	inner *fakeInner
	pl    []byte
	rel   core.Release
}

func newEngineRig(t testing.TB, size int, hedge time.Duration) *engineRig {
	h := newHarness(t, size, "realdebrid")
	h.cacheEverywhere(testHash)
	r := &engineRig{harness: h, cache: newMemCache(), inner: &fakeInner{}, rel: torrentRelease("rel-e")}
	r.eng = NewEngine(h.svc, EngineOptions{Inner: r.inner, Cache: r.cache, HedgeDelay: hedge, BlockSize: testBlock, Logger: h.svc.log})
	t.Cleanup(func() { r.eng.Close() })
	var err error
	if r.pl, err = h.svc.Acquire(context.Background(), r.rel); err != nil {
		t.Fatal(err)
	}
	return r
}

func readAll(t testing.TB, src core.MediaSource) []byte {
	t.Helper()
	out := make([]byte, src.Size())
	rc := src.(interface {
		ReadAtContext(context.Context, []byte, int64) (int, error)
	})
	for off := 0; off < len(out); {
		n, err := rc.ReadAtContext(context.Background(), out[off:min(off+100_000, len(out))], int64(off))
		if err != nil && err != io.EOF {
			t.Fatalf("read at %d: %v", off, err)
		}
		if n == 0 {
			t.Fatalf("no progress at %d", off)
		}
		off += n
	}
	return out
}

func TestEngineDelegatesNonDebridPayloads(t *testing.T) {
	r := newEngineRig(t, 256<<10, -1)
	nzb := []byte("<?xml version=\"1.0\"?><nzb/>")
	if _, err := r.eng.Open(context.Background(), "u", nzb); err == nil || r.inner.opened.Load() != 1 {
		t.Fatal("open not delegated")
	}
	_ = r.eng.Prefetch(context.Background(), "u", nzb, 0, 10)
	if ok, err := r.eng.Check(context.Background(), nzb); !ok || err != nil || r.inner.prefetched.Load() != 1 || r.inner.checked.Load() != 1 {
		t.Fatal("prefetch/check not delegated")
	}
}

func TestEngineReadsWholeFileWithRanges(t *testing.T) {
	size := 1<<20 + 12345 // not block aligned
	r := newEngineRig(t, size, -1)
	src, err := r.eng.Open(context.Background(), r.rel.ID, r.pl)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if src.Size() != int64(size) || src.Name() != "Movie.2024.1080p.mkv" || src.ReleaseID() != r.rel.ID {
		t.Fatalf("source %d %q %q", src.Size(), src.Name(), src.ReleaseID())
	}
	if got := readAll(t, src); !bytes.Equal(got, r.media.data) {
		t.Fatal("content mismatch")
	}
	// Odd offsets, block boundaries and EOF.
	buf := make([]byte, 1000)
	for _, off := range []int64{0, testBlock - 10, int64(size) - 500, 3*testBlock + 7} {
		n, err := src.ReadAt(buf, off)
		want := r.media.data[off:min(off+1000, int64(size))]
		if n != len(want) || !bytes.Equal(buf[:n], want) || (n < 1000 && err != io.EOF) {
			t.Fatalf("readat %d: n=%d err=%v", off, n, err)
		}
	}
	if _, err := src.ReadAt(buf, int64(size)); err != io.EOF {
		t.Fatalf("past EOF: %v", err)
	}
	// Optional interfaces the playback layer type-asserts.
	if _, ok := src.(interface {
		ReadAtPrefetchContext(context.Context, []byte, int64) (int, error)
	}); !ok {
		t.Fatal("no ReadAtPrefetchContext")
	}
	if _, ok := src.(interface {
		ReadMetadataAtContext(context.Context, []byte, int64) (int, error)
	}); !ok {
		t.Fatal("no ReadMetadataAtContext")
	}
	if r.cache.puts.Load() == 0 {
		t.Fatal("blocks were not cached")
	}
}

func TestEngineOpenWithFreshLinkMakesNoCalls(t *testing.T) {
	r := newEngineRig(t, 512<<10, -1)
	prov := r.provs["realdebrid-0"]
	calls, reqs := prov.totalCalls(), r.media.requests.Load()
	src, err := r.eng.Open(context.Background(), r.rel.ID, r.pl)
	if err != nil {
		t.Fatal(err)
	}
	if prov.totalCalls() != calls || r.media.requests.Load() != reqs {
		t.Fatalf("open made %d provider calls and %d media requests", prov.totalCalls()-calls, r.media.requests.Load()-reqs)
	}
	src.Close()
}

func TestEnginePrefetchWarmsHeadAndTail(t *testing.T) {
	size := 4 << 20
	r := newEngineRig(t, size, -1)
	ctx := context.Background()
	if err := r.eng.Prefetch(ctx, r.rel.ID, r.pl, 0, 256<<10); err != nil {
		t.Fatal(err)
	}
	for _, idx := range []int{0, 3, (size - 1) / testBlock, (size - tailPrefetchBytes) / testBlock} {
		if !r.cache.Has(r.rel.ID, idx) {
			t.Fatalf("block %d not warm", idx)
		}
	}
	if r.cache.Has(r.rel.ID, 10) {
		t.Fatal("middle of the file was prefetched")
	}
	// Play: head and tail reads are served without touching the network.
	reqs := r.media.requests.Load()
	src, err := r.eng.Open(ctx, r.rel.ID, r.pl)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	meta := src.(interface {
		ReadMetadataAtContext(context.Context, []byte, int64) (int, error)
	})
	buf := make([]byte, 4096)
	if _, err := meta.ReadMetadataAtContext(ctx, buf, 0); err != nil || !bytes.Equal(buf, r.media.data[:4096]) {
		t.Fatalf("head: %v", err)
	}
	if _, err := meta.ReadMetadataAtContext(ctx, buf, int64(size-4096)); err != nil || !bytes.Equal(buf, r.media.data[size-4096:]) {
		t.Fatalf("tail: %v", err)
	}
	if got := r.media.requests.Load() - reqs; got != 0 {
		t.Fatalf("warm reads made %d media requests", got)
	}
}

func TestEngineRefreshesRevokedLinks(t *testing.T) {
	r := newEngineRig(t, 512<<10, -1)
	prov := r.provs["realdebrid-0"]
	r.media.revokeAll() // the CDN now answers 403 for the stored link
	src, err := r.eng.Open(context.Background(), r.rel.ID, r.pl)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	links := prov.callCount("link")
	if got := readAll(t, src); !bytes.Equal(got, r.media.data) {
		t.Fatal("content after refresh")
	}
	if prov.callCount("link") <= links {
		t.Fatal("link was not refreshed")
	}
	if r.eng.Stats().LinkRefreshes == 0 {
		t.Fatal("refresh not counted")
	}
}

func TestEngineErrorClassification(t *testing.T) {
	t.Run("link keeps failing is missing", func(t *testing.T) {
		r := newEngineRig(t, 256<<10, -1)
		r.media.mu.Lock()
		r.media.status["realdebrid-1"] = http.StatusNotFound
		r.media.mu.Unlock()
		prov := r.provs["realdebrid-0"]
		// Every new link 404s too.
		src, err := r.eng.Open(context.Background(), r.rel.ID, r.pl)
		if err != nil {
			t.Fatal(err)
		}
		defer src.Close()
		r.media.mu.Lock()
		for i := 1; i <= 20; i++ {
			r.media.status["realdebrid-"+strconv.Itoa(i)] = http.StatusNotFound
		}
		r.media.mu.Unlock()
		_, err = src.ReadAt(make([]byte, 10), 0)
		if !errors.Is(err, core.ErrArticleMissing) {
			t.Fatalf("got %v (link calls %d)", err, prov.callCount("link"))
		}
	})
	t.Run("size mismatch is corrupt", func(t *testing.T) {
		r := newEngineRig(t, 256<<10, -1)
		r.media.mu.Lock()
		r.media.size = 999 << 20
		r.media.mu.Unlock()
		src, err := r.eng.Open(context.Background(), r.rel.ID, r.pl)
		if err != nil {
			t.Fatal(err)
		}
		defer src.Close()
		if _, err = src.ReadAt(make([]byte, 10), 0); !errors.Is(err, core.ErrArticleCorrupt) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("server errors are transient", func(t *testing.T) {
		r := newEngineRig(t, 256<<10, -1)
		r.media.mu.Lock()
		r.media.status["realdebrid-1"] = http.StatusBadGateway
		r.media.mu.Unlock()
		src, err := r.eng.Open(context.Background(), r.rel.ID, r.pl)
		if err != nil {
			t.Fatal(err)
		}
		defer src.Close()
		_, err = src.ReadAt(make([]byte, 10), 0)
		if !errors.Is(err, core.ErrSourceUnavailable) || errors.Is(err, core.ErrArticleMissing) {
			t.Fatalf("got %v", err)
		}
	})
}

func TestEngineCheck(t *testing.T) {
	r := newEngineRig(t, 256<<10, -1)
	ok, err := r.eng.Check(context.Background(), r.pl)
	if !ok || err != nil {
		t.Fatalf("check %v %v", ok, err)
	}
	// The head block the check read is where Open looks for it.
	if !r.cache.Has(r.rel.ID, 0) {
		t.Fatal("check did not warm the head block")
	}
	r.media.mu.Lock()
	r.media.size = 1
	r.media.mu.Unlock()
	r2 := newEngineRig(t, 256<<10, -1)
	r2.media.mu.Lock()
	r2.media.size = 5
	r2.media.mu.Unlock()
	if ok, err := r2.eng.Check(context.Background(), r2.pl); ok || !errors.Is(err, core.ErrArticleCorrupt) {
		t.Fatalf("mismatch check %v %v", ok, err)
	}
}

func TestEngineHedgesSlowFirstByte(t *testing.T) {
	r := newEngineRig(t, 256<<10, 30*time.Millisecond)
	r.media.mu.Lock()
	r.media.stall["realdebrid-1|bytes=0-65535"] = 3 * time.Second // the head request stalls
	r.media.mu.Unlock()
	src, err := r.eng.Open(context.Background(), r.rel.ID, r.pl)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	start := time.Now()
	buf := make([]byte, 1000)
	if _, err := src.ReadAt(buf, 0); err != nil || !bytes.Equal(buf, r.media.data[:1000]) {
		t.Fatalf("read %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("hedge did not rescue a stalled request (%v)", d)
	}
	st := r.eng.Stats()
	if st.Hedges == 0 || st.HedgeWins == 0 {
		t.Fatalf("stats %+v", st)
	}
}

func TestEngineConcurrentReadersShareBlocks(t *testing.T) {
	r := newEngineRig(t, 1<<20, -1)
	src, err := r.eng.Open(context.Background(), r.rel.ID, r.pl)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, 4096)
			if _, err := src.ReadAt(buf, 5*testBlock); err != nil || !bytes.Equal(buf, r.media.data[5*testBlock:5*testBlock+4096]) {
				t.Errorf("read %v", err)
			}
		}()
	}
	wg.Wait()
	fetched := r.eng.Stats().BlocksFetched
	// Eight readers of one block plus read-ahead; never eight fetches of it.
	if fetched > int64(1<<20/testBlock) {
		t.Fatalf("fetched %d blocks", fetched)
	}
}

func TestEngineSeekStartsFreshRequest(t *testing.T) {
	r := newEngineRig(t, 32<<20, -1)
	src, err := r.eng.Open(context.Background(), r.rel.ID, r.pl)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	buf := make([]byte, 8192)
	if _, err := src.ReadAt(buf, 0); err != nil {
		t.Fatal(err)
	}
	far := int64(24 << 20) // past the read-ahead window
	if _, err := src.ReadAt(buf, far); err != nil || !bytes.Equal(buf, r.media.data[far:far+8192]) {
		t.Fatalf("seek read %v", err)
	}
	// The seek target was requested directly by one request starting at
	// its block, not after draining read-ahead from the old position.
	r.media.mu.Lock()
	defer r.media.mu.Unlock()
	found := false
	for _, rg := range r.media.ranges {
		if strings.HasPrefix(rg, fmt.Sprintf("bytes=%d-", far)) {
			found = true
		}
	}
	if !found {
		t.Fatalf("no request at the seek target: %v", r.media.ranges)
	}
}

func TestBackgroundHealthPreservesBudgetContext(t *testing.T) {
	r := newEngineRig(t, 256<<10, -1)
	before := r.media.requests.Load()
	denied := core.WithBackgroundBudget(t.Context(), func(context.Context, string, string, int64) error { return core.ErrBudgetExhausted })
	if ok, err := r.eng.Check(denied, r.pl); ok || !errors.Is(err, core.ErrBudgetExhausted) {
		t.Fatalf("denied check: %v %v", ok, err)
	}
	if r.media.requests.Load() != before {
		t.Fatal("background request escaped its allowance")
	}
	var reserved int64
	allowed := core.WithBackgroundBudget(t.Context(), func(_ context.Context, kind, _ string, n int64) error {
		if kind == "bytes" {
			reserved += n
		}
		return nil
	})
	if ok, err := r.eng.Check(allowed, r.pl); !ok || err != nil {
		t.Fatalf("allowed check: %v %v", ok, err)
	}
	if reserved != 65536 {
		t.Fatalf("range not accounted: %d", reserved)
	}
}

func TestHTTPSourceOriginNamesService(t *testing.T) {
	for kind, want := range map[string]string{"realdebrid": "Real-Debrid", "torbox": "TorBox", KindHTTP: "HTTP", "newsvc": "newsvc", "": "Debrid"} {
		s := &httpSource{f: &remoteFile{pl: Payload{Kind: kind}}}
		if got := s.Origin(); got != want {
			t.Errorf("Origin() for kind %q = %q, want %q", kind, got, want)
		}
	}
}
