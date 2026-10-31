package stream

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// altID is the message-ID of a repost ("fill") of part n of a file.
func altID(name string, n int) string {
	return fmt.Sprintf("%s.fill%d@ether.test", sanitizeID(name), n)
}

// withRepost lists a repost message-ID for part n of name right after the
// primary, and stores the repost article in f with the given header.
func withRepost(t *testing.T, f *fakeFetcher, doc []byte, name string, n int, part func(core.ArticlePart) core.ArticlePart) []byte {
	t.Helper()
	primary := postID(name, n)
	f.mu.Lock()
	a, ok := f.articles[primary]
	if !ok {
		f.mu.Unlock()
		t.Fatalf("no article %s", primary)
	}
	f.articles[altID(name, n)] = fakeArticle{data: bytes.Clone(a.data), part: part(a.part)}
	f.mu.Unlock()
	marker := ">" + primary + "</segment>"
	if !bytes.Contains(doc, []byte(marker)) {
		t.Fatalf("segment %s not in NZB", primary)
	}
	repost := fmt.Sprintf(`%s<segment bytes="1" number="%d">%s</segment>`, marker, n, altID(name, n))
	return bytes.Replace(doc, []byte(marker), []byte(repost), 1)
}

func samePart(p core.ArticlePart) core.ArticlePart { return p }

type memSegmentCache struct {
	mu   sync.Mutex
	segs map[string][]byte
}

func (c *memSegmentCache) key(id string, seg int) string { return fmt.Sprintf("%s/%d", id, seg) }
func (c *memSegmentCache) Get(id string, seg int) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, ok := c.segs[c.key(id, seg)]
	return b, ok
}
func (c *memSegmentCache) Put(id string, seg int, b []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.segs == nil {
		c.segs = map[string][]byte{}
	}
	c.segs[c.key(id, seg)] = bytes.Clone(b)
	return nil
}
func (c *memSegmentCache) Pin(string, string) error   { return nil }
func (c *memSegmentCache) Unpin(string, string) error { return nil }
func (c *memSegmentCache) Usage() (int64, int64)      { return 0, 0 }

// The primary article of a part 430s on every provider; the repost listed
// for the same part number serves it, and the cached copy is the slot's.
func TestRepostAlternateServesMissingPart(t *testing.T) {
	f := newFakeFetcher()
	data := mkvBytes(3, 200_000)
	doc := post(t, f, 10_000, postFile{name: "r.mkv", data: data})
	doc = withRepost(t, f, doc, "r.mkv", 7, samePart)
	f.setMissing(postID("r.mkv", 7))
	cache := &memSegmentCache{}
	e := newTestEngine(t, f, cache, Options{})
	src := openSource(t, e, "rel1", doc)
	got, err := io.ReadAll(io.NewSectionReader(src, 0, int64(len(data))))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("bytes served through the repost differ from the post")
	}
	if f.calls(altID("r.mkv", 7)) == 0 {
		t.Fatal("repost was never fetched")
	}
	// Close the engine so the cache write has landed, then read the slot
	// back from a fresh engine whose fetcher has nothing.
	_ = src.Close()
	_ = e.Close()
	cold := newFakeFetcher()
	cold.failAll = true
	e2 := newTestEngine(t, cold, cache, Options{})
	// One posted file, so global segment numbers equal its slots.
	seg, ok := e2.cachedSegment("rel1", 6)
	if !ok {
		t.Fatal("repost slot not in the segment cache")
	}
	if want := data[6*10_000 : 7*10_000]; seg.begin != 6*10_000 || !bytes.Equal(seg.data, want) {
		t.Fatalf("cached repost slot begin=%d len=%d, not the part's bytes", seg.begin, len(seg.data))
	}
}

// A repost whose yEnc header places it elsewhere (another part, another
// range or another file size) must never stand in for the slot.
func TestRepostWithMismatchedHeaderIsRejected(t *testing.T) {
	cases := map[string]func(core.ArticlePart) core.ArticlePart{
		"part":     func(p core.ArticlePart) core.ArticlePart { p.Part++; return p },
		"range":    func(p core.ArticlePart) core.ArticlePart { p.Begin += 10; p.End += 10; return p },
		"filesize": func(p core.ArticlePart) core.ArticlePart { p.FileSize += 10_000; return p },
		"total":    func(p core.ArticlePart) core.ArticlePart { p.Total++; return p },
	}
	for name, mangle := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeFetcher()
			data := mkvBytes(4, 200_000)
			doc := post(t, f, 10_000, postFile{name: "m.mkv", data: data})
			doc = withRepost(t, f, doc, "m.mkv", 7, mangle)
			f.setMissing(postID("m.mkv", 7))
			e := newTestEngine(t, f, nil, Options{Retries: 0})
			src := openSource(t, e, "rel1", doc)
			// Learn the neighbouring ranges first, as playback would.
			buf := make([]byte, 10_000)
			for _, off := range []int64{50_000, 70_000} {
				if _, err := src.ReadAt(buf, off); err != nil {
					t.Fatalf("read %d: %v", off, err)
				}
			}
			_, err := src.ReadAt(buf, 60_000)
			if err == nil {
				t.Fatal("a mismatched repost served the missing part")
			}
			if f.calls(altID("m.mkv", 7)) == 0 {
				t.Fatal("repost was never tried")
			}
		})
	}
}

// With no repost listed, a missing part still fails as before.
func TestMissingPartWithoutRepostStillFails(t *testing.T) {
	f := newFakeFetcher()
	data := mkvBytes(5, 200_000)
	doc := post(t, f, 10_000, postFile{name: "n.mkv", data: data})
	f.setMissing(postID("n.mkv", 7))
	e := newTestEngine(t, f, nil, Options{})
	src := openSource(t, e, "rel1", doc)
	buf := make([]byte, 10_000)
	if _, err := src.ReadAt(buf, 60_000); !errors.Is(err, core.ErrArticleMissing) {
		t.Fatalf("missing part read err = %v", err)
	}
}

// The health check samples the last part. Its primary is gone but a
// repost is present: the release is healthy. Without the repost it is not.
func TestCheckHealthyWhenSampledPrimaryHasRepost(t *testing.T) {
	for _, repostPresent := range []bool{true, false} {
		f := newFakeFetcher()
		doc := post(t, f, 10_000, postFile{name: "h.mkv", data: mkvBytes(1, 1_000_000)})
		doc = withRepost(t, f, doc, "h.mkv", 100, samePart) // the last part is always sampled
		f.setMissing(postID("h.mkv", 100))
		if !repostPresent {
			f.setMissing(altID("h.mkv", 100))
		}
		e := newTestEngine(t, f, nil, Options{HealthSamples: 5})
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		ok, err := e.Check(ctx, doc)
		cancel()
		if repostPresent && (!ok || err != nil) {
			t.Fatalf("repost present: Check = %v %v, want healthy", ok, err)
		}
		if !repostPresent && (ok || !errors.Is(err, core.ErrArticleMissing)) {
			t.Fatalf("repost missing too: Check = %v %v, want missing", ok, err)
		}
	}
}

// A mid-file sample is BODY-probed (where takedowns hide behind STAT
// 223). Every primary probe reports a takedown and every repost probe
// succeeds: the release is healthy through its reposts, each probed with
// the slot's expected yEnc header.
type repostOnlyBodyFetcher struct {
	*fakeFetcher
	mu    sync.Mutex
	wants map[string]core.ArticlePart
}

func (f *repostOnlyBodyFetcher) CheckBodyPart(_ context.Context, id string, _ []string, _ core.Priority, want core.ArticlePart) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.wants[id] = want
	if strings.Contains(id, ".fill") {
		return nil
	}
	return fmt.Errorf("fake BODY <%s> 451: %w", id, core.ErrArticleMissing)
}

func TestCheckBodyProbeFallsBackToRepost(t *testing.T) {
	f := newFakeFetcher()
	doc := post(t, f, 10_000, postFile{name: "b.mkv", data: mkvBytes(1, 1_000_000)})
	// Every part gets a repost, so the probed ones have one whichever
	// bodyProbeSlots picks.
	for n := 1; n <= 100; n++ {
		doc = withRepost(t, f, doc, "b.mkv", n, samePart)
	}
	bf := &repostOnlyBodyFetcher{fakeFetcher: f, wants: map[string]core.ArticlePart{}}
	e := newTestEngine(t, bf, nil, Options{HealthSamples: 12, HealthParallel: 12})
	ok, err := e.Check(context.Background(), doc)
	if !ok || err != nil {
		t.Fatalf("Check = %v %v, want healthy through reposts", ok, err)
	}
	bf.mu.Lock()
	defer bf.mu.Unlock()
	reposts := 0
	for id, want := range bf.wants {
		if !strings.Contains(id, ".fill") {
			continue
		}
		reposts++
		// The expected header only grows as the plan learns sizes, so
		// the repost is held to at least what the primary was.
		primary := bf.wants[strings.Replace(id, ".fill", ".p", 1)]
		if want.Part != primary.Part || want.Total != primary.Total || (want.FileSize != 0 && want.FileSize != 1_000_000) {
			t.Fatalf("repost %s probed with %+v, primary with %+v", id, want, primary)
		}
	}
	if reposts == 0 {
		t.Fatal("no repost was BODY-probed")
	}
}
