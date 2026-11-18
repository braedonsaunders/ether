package stream

import (
	"context"
	"errors"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/nzb"
)

func twoPartFile() *postedFile {
	f := &nzb.File{Name: "x.mkv", Segments: []nzb.Segment{{Number: 1, MessageID: "a"}, {Number: 2, MessageID: "b"}}}
	return newPostedFile(0, f, 0, nil)
}

// A yEnc header is posted by strangers: a short article claiming a huge
// part must not be zero-filled into a process-killing allocation.
func TestNormalizeRejectsImplausiblePartHeader(t *testing.T) {
	e := newTestEngine(t, newFakeFetcher(), nil, Options{})
	pf := twoPartFile()
	data := []byte("short article")
	_, err := e.normalize(pf, 0, data, core.ArticlePart{Name: "x.mkv", FileSize: 1 << 40, Part: 1, Total: 2, Begin: 1, End: 1 << 40})
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("implausible header accepted: %v", err)
	}
	// A damaged decoded body is never silently turned into container bytes.
	for _, data := range [][]byte{data, make([]byte, 200)} {
		_, err = e.normalize(pf, 0, data, core.ArticlePart{FileSize: 1000, Begin: 1, End: 100})
		if !errors.Is(err, ErrCorrupt) {
			t.Fatalf("length mismatch: %v", err)
		}
	}
}

func TestLearnIgnoresAbsurdPartTotal(t *testing.T) {
	pf := twoPartFile()
	pf.learn(0, 0, 100, 150, 1<<30)
	if n := pf.numSlots(); n != 2 {
		t.Fatalf("slots = %d after an absurd total, want 2", n)
	}
	pf.learn(0, 0, 100, 350, 4)
	if n := pf.numSlots(); n != 4 {
		t.Fatalf("slots = %d after a plausible total, want 4", n)
	}
	// Growth is bounded by the NZB listing, so headers cannot compound it.
	pf.learn(1, 100, 200, 350, 2000)
	if n := pf.numSlots(); n != 4 {
		t.Fatalf("slots = %d after a compounding total, want 4", n)
	}
}

type retryOwningFetcher struct{ *fakeFetcher }

func (*retryOwningFetcher) OwnsArticleRetries() {}

func TestFetcherRetryBudgetIsNotMultiplied(t *testing.T) {
	f := newFakeFetcher()
	f.failures["a"] = 100
	e := newTestEngine(t, &retryOwningFetcher{f}, nil, Options{Retries: 2})
	pf := twoPartFile()
	_, _, err := e.load(context.Background(), pf, 0, "a", 0, core.PriorityLive, fetchOpts{})
	if err == nil {
		t.Fatal("expected failure")
	}
	if got := f.bodyCalls["a"]; got != 1 {
		t.Fatalf("fetch calls = %d, want 1", got)
	}
}

// Some uploaders randomise =ybegin size/total/name per article while keeping
// =ypart begin/end truthful. Here the announced size is large enough to pass
// an extent check, so only arithmetic on the article's own bytes reveals the
// contradiction. The extents must still place the bytes.
func TestNormalizeDropsContradictoryFileMetadata(t *testing.T) {
	e := newTestEngine(t, newFakeFetcher(), nil, Options{})
	pf := twoPartFile()
	const seg = 768_000
	data := make([]byte, seg)
	part := core.ArticlePart{
		Name: "Q0bFusc4ted", FileSize: 10_888_513, Part: 1, Total: 829,
		Begin: 1, End: seg,
	}
	seg2, err := e.normalize(pf, 0, data, part)
	if err != nil {
		t.Fatalf("contradictory metadata rejected a good article: %v", err)
	}
	if seg2.begin != 0 || seg2.end() != seg {
		t.Fatalf("extents not kept: begin=%d end=%d", seg2.begin, seg2.end())
	}
	if seg2.fileSize != 0 || seg2.total != 0 || seg2.name != "" {
		t.Fatalf("contradictory metadata survived: fileSize=%d total=%d name=%q",
			seg2.fileSize, seg2.total, seg2.name)
	}
	// The article must not pin the posted file's size from the bad header.
	if pf.SizeKnown() && pf.Size() != int64(seg) {
		t.Fatalf("size learned from contradictory header: %d", pf.Size())
	}
}

// A header that agrees with the bytes it carries is kept as-is.
func TestNormalizeKeepsConsistentFileMetadata(t *testing.T) {
	e := newTestEngine(t, newFakeFetcher(), nil, Options{})
	pf := twoPartFile()
	const seg = 768_000
	data := make([]byte, seg)
	part := core.ArticlePart{
		Name: "real.mkv", FileSize: 3 * seg, Part: 1, Total: 3,
		Begin: 1, End: seg,
	}
	seg2, err := e.normalize(pf, 0, data, part)
	if err != nil {
		t.Fatal(err)
	}
	if seg2.fileSize != 3*seg || seg2.total != 3 || seg2.name != "real.mkv" {
		t.Fatalf("consistent metadata dropped: fileSize=%d total=%d name=%q",
			seg2.fileSize, seg2.total, seg2.name)
	}
	if !seg2.rangeExact {
		t.Fatal("consistent range not marked exact")
	}
}
