package stream

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

func damageElement(id []byte, payload []byte) []byte {
	n := len(payload)
	size := []byte{0x10 | byte(n>>24), byte(n >> 16), byte(n >> 8), byte(n)}
	b := append(bytes.Clone(id), size...)
	return append(b, payload...)
}

func damageMKV(flags byte, crcBefore, crcAfter bool) []byte {
	frame := append([]byte{0x81, 0, 0, flags}, bytes.Repeat([]byte{0x55}, 256<<10)...)
	cluster := damageElement([]byte{0xa3}, frame)
	crc := damageElement([]byte{0xbf}, []byte{0, 0, 0, 0})
	if crcBefore {
		cluster = append(bytes.Clone(crc), cluster...)
	}
	if crcAfter {
		cluster = append(cluster, crc...)
	}
	segment := damageElement([]byte{0x1f, 0x43, 0xb6, 0x75}, cluster)
	header := damageElement([]byte{0x1a, 0x45, 0xdf, 0xa3}, damageElement([]byte{0x42, 0x82}, []byte("matroska")))
	return append(header, damageElement([]byte{0x18, 0x53, 0x80, 0x67}, segment)...)
}

func TestMatroskaDamagePreservesStructureAndRepeatedRanges(t *testing.T) {
	f := newFakeFetcher()
	data := damageMKV(0x80, false, false)
	doc := post(t, f, 32<<10, postFile{name: "video.mkv", data: data})
	f.setMissing(postID("video.mkv", 4)) // exact payload-only range [96KiB,128KiB)
	e := newTestEngine(t, f, nil, Options{ReadAheadParallel: 1})
	src := openSource(t, e, "damage", doc).(*source)
	var notices, gaps atomic.Int32
	src.SetDamageHandler(func(off, n int64) bool {
		if n == 0 {
			notices.Add(1)
			return false
		}
		gaps.Add(1)
		if off != 96<<10 || n != 32<<10 {
			t.Errorf("wrong proved gap: [%d,+%d)", off, n)
		}
		return true
	})
	want := bytes.Clone(data)
	clear(want[96<<10 : 128<<10])
	// Concurrent, overlapping reads must share one concealment decision.
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got := make([]byte, len(data))
			n, err := src.ReadAtContext(context.Background(), got, 0)
			if err != nil || n != len(got) || !bytes.Equal(got, want) {
				t.Errorf("damaged read n=%d err=%v matches=%v", n, err, bytes.Equal(got, want))
			}
		}()
	}
	wg.Wait()
	if notices.Load() != 1 || gaps.Load() != 1 {
		t.Fatalf("notices=%d decisions=%d", notices.Load(), gaps.Load())
	}
	// A returning article must not change this source's HTTP representation.
	f.mu.Lock()
	delete(f.missing, postID("video.mkv", 4))
	f.mu.Unlock()
	got := make([]byte, 64<<10)
	if _, err := src.ReadAt(got, 80<<10); err != nil || !bytes.Equal(got, want[80<<10:144<<10]) {
		t.Fatalf("range changed after upstream recovery: %v", err)
	}
	// Padding is source-local, never a successful segment in the shared cache.
	other := openSource(t, e, "damage", doc)
	original := make([]byte, 32<<10)
	if _, err := other.ReadAt(original, 96<<10); err != nil || !bytes.Equal(original, data[96<<10:128<<10]) {
		t.Fatalf("shared cache polluted: %v", err)
	}
}

func damageSmallFrames() []byte {
	var blocks []byte
	for range 24 {
		blocks = append(blocks, damageElement([]byte{0xa3}, append([]byte{0x81, 0, 0, 0x80}, bytes.Repeat([]byte{0x55}, 16<<10)...))...)
	}
	header := damageElement([]byte{0x1a, 0x45, 0xdf, 0xa3}, damageElement([]byte{0x42, 0x82}, []byte("matroska")))
	return append(header, damageElement([]byte{0x18, 0x53, 0x80, 0x67}, damageElement([]byte{0x1f, 0x43, 0xb6, 0x75}, blocks))...)
}

func TestDamageNeverConcealsUnprovedBytes(t *testing.T) {
	for _, tc := range []struct {
		name        string
		data        []byte
		noHeader    bool
		missingPart int
	}{
		{"missing-frame-header", damageSmallFrames(), false, 4},
		{"laced", damageMKV(0x82, false, false), false, 4},
		{"crc-before", damageMKV(0x80, true, false), false, 4},
		{"crc-after", damageMKV(0x80, false, true), false, 4},
		{"estimated-ranges", damageMKV(0x80, false, false), true, 4},
		{"unknown-container", mkvBytes(1, 300<<10), false, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeFetcher()
			doc := post(t, f, 32<<10, postFile{name: "video.mkv", data: tc.data})
			f.noHeader = tc.noHeader
			f.setMissing(postID("video.mkv", tc.missingPart))
			e := newTestEngine(t, f, nil, Options{ReadAheadParallel: 1})
			src := openSource(t, e, "safe", doc).(*source)
			var notices, gaps atomic.Int32
			src.SetDamageHandler(func(_, n int64) bool {
				if n == 0 {
					notices.Add(1)
				} else {
					gaps.Add(1)
				}
				return true
			})
			_, err := src.ReadAt(make([]byte, 32<<10), 96<<10)
			if !errors.Is(err, core.ErrArticleMissing) || gaps.Load() != 0 || notices.Load() != 1 {
				t.Fatalf("err=%v decisions=%d notices=%d", err, gaps.Load(), notices.Load())
			}
		})
	}
}

func TestDamagePolicyDeclineAndPrefetchRemainFailures(t *testing.T) {
	f := newFakeFetcher()
	data := damageMKV(0x80, false, false)
	doc := post(t, f, 32<<10, postFile{name: "video.mkv", data: data})
	f.setMissing(postID("video.mkv", 4))
	e := newTestEngine(t, f, nil, Options{ReadAheadParallel: 1})
	src := openSource(t, e, "bounded", doc).(*source)
	var calls atomic.Int32
	src.SetDamageHandler(func(_, n int64) bool {
		if n > 0 {
			calls.Add(1)
		}
		return false
	})
	_, err := src.ReadAt(make([]byte, 32<<10), 96<<10)
	if !errors.Is(err, core.ErrArticleMissing) || calls.Load() != 1 {
		t.Fatalf("declined gap: %v %d", err, calls.Load())
	}
	_, err = src.ReadAt(make([]byte, 32<<10), 96<<10)
	if !errors.Is(err, core.ErrArticleMissing) || calls.Load() != 1 {
		t.Fatalf("declined gap was counted twice: %v %d", err, calls.Load())
	}
	src.SetDamageHandler(func(_, n int64) bool { calls.Add(1); return true })
	_, err = src.ReadAtPrefetchContext(context.Background(), make([]byte, 32<<10), 96<<10)
	if !errors.Is(err, core.ErrArticleMissing) || calls.Load() != 1 {
		t.Fatalf("prefetch concealed bytes: %v %d", err, calls.Load())
	}
}

func TestDamageDoesNotHideProviderFailureOrCancellation(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		f := newFakeFetcher()
		data := damageMKV(0x80, false, false)
		doc := post(t, f, 32<<10, postFile{name: "video.mkv", data: data})
		e := newTestEngine(t, f, nil, Options{Retries: 1, ReadAheadParallel: 1})
		src := openSource(t, e, "outage", doc).(*source)
		var calls atomic.Int32
		src.SetDamageHandler(func(_, n int64) bool { calls.Add(1); return true })
		f.mu.Lock()
		f.failAll = true
		f.mu.Unlock()
		ctx, cancel := context.WithCancel(context.Background())
		if cancelled {
			cancel()
		}
		_, err := src.ReadAtContext(ctx, make([]byte, 32<<10), 96<<10)
		cancel()
		if err == nil || errors.Is(err, core.ErrArticleMissing) || calls.Load() != 0 {
			t.Fatalf("cancelled=%v err=%v notifications=%d", cancelled, err, calls.Load())
		}
	}
}

type damageErrorFetcher struct {
	*fakeFetcher
	failure error
}

func (f *damageErrorFetcher) BodyPart(ctx context.Context, id string, groups []string, prio core.Priority) ([]byte, core.ArticlePart, error) {
	if id == postID("video.mkv", 4) {
		return nil, core.ArticlePart{}, f.failure
	}
	return f.fakeFetcher.BodyPart(ctx, id, groups, prio)
}

func TestCorruptionPreparesAlternativeButIsNeverConcealed(t *testing.T) {
	for _, failure := range []error{ErrCorrupt, core.ErrArticleCorrupt} {
		t.Run(failure.Error(), func(t *testing.T) {
			f := newFakeFetcher()
			doc := post(t, f, 32<<10, postFile{name: "video.mkv", data: damageMKV(0x80, false, false)})
			e := newTestEngine(t, &damageErrorFetcher{fakeFetcher: f, failure: failure}, nil, Options{Retries: 1, ReadAheadParallel: 1})
			src := openSource(t, e, "corrupt", doc).(*source)
			var notices, gaps atomic.Int32
			src.SetDamageHandler(func(_, n int64) bool {
				if n == 0 {
					notices.Add(1)
				} else {
					gaps.Add(1)
				}
				return true
			})
			for range 2 {
				_, err := src.ReadAt(make([]byte, 32<<10), 96<<10)
				if !errors.Is(err, failure) {
					t.Fatalf("corruption hidden: %v", err)
				}
			}
			if notices.Load() != 1 || gaps.Load() != 0 {
				t.Fatalf("notices=%d concealment decisions=%d", notices.Load(), gaps.Load())
			}
		})
	}
}

// damageDeepMKV places one concealable frame after enough clusters that
// walking every block header exceeds the 16384-element budget. The gap is
// the frame payload, with a present segment on each side.
func damageDeepMKV() (data []byte, gapBegin, gapEnd int) {
	const clusters = 100
	const blocks = 180
	tiny := damageElement([]byte{0xa3}, []byte{0x81, 0, 0, 0x80, 0x11})
	var one []byte
	for range blocks {
		one = append(one, tiny...)
	}
	early := damageElement([]byte{0x1f, 0x43, 0xb6, 0x75}, one)
	var body []byte
	for range clusters {
		body = append(body, early...)
	}
	payload := bytes.Repeat([]byte{0x55}, 4096)
	frame := append([]byte{0x81, 0, 0, 0x80}, payload...)
	body = append(body, damageElement([]byte{0x1f, 0x43, 0xb6, 0x75}, damageElement([]byte{0xa3}, frame))...)
	header := damageElement([]byte{0x1a, 0x45, 0xdf, 0xa3}, damageElement([]byte{0x42, 0x82}, []byte("matroska")))
	file := append(header, damageElement([]byte{0x18, 0x53, 0x80, 0x67}, body)...)
	gapEnd = len(file)
	gapBegin = gapEnd - len(payload)
	return append(file, []byte("tail")...), gapBegin, gapEnd
}

func TestMatroskaDamageConcealsGapDeepInFile(t *testing.T) {
	data, gapBegin, gapEnd := damageDeepMKV()
	if clusters := bytes.Count(data, []byte{0x1f, 0x43, 0xb6, 0x75}); clusters < 100 {
		t.Fatalf("clusters = %d", clusters)
	}
	f := newFakeFetcher()
	doc := post(t, f, 0, postFile{name: "video.mkv", data: data, parts: []int{gapBegin, gapEnd - gapBegin, len(data) - gapEnd}})
	f.setMissing(postID("video.mkv", 2))
	e := newTestEngine(t, f, nil, Options{ReadAheadParallel: 1})
	src := openSource(t, e, "deep", doc).(*source)
	var gotBegin, gotLen int64
	src.SetDamageHandler(func(off, n int64) bool {
		if n > 0 {
			gotBegin, gotLen = off, n
		}
		return true
	})
	buf := make([]byte, gapEnd-gapBegin)
	n, err := src.ReadAtContext(context.Background(), buf, int64(gapBegin))
	if err != nil || n != len(buf) || !bytes.Equal(buf, make([]byte, len(buf))) {
		t.Fatalf("deep gap n=%d err=%v zeroed=%v", n, err, bytes.Equal(buf, make([]byte, len(buf))))
	}
	if gotBegin != int64(gapBegin) || gotLen != int64(gapEnd-gapBegin) {
		t.Fatalf("proved gap [%d,+%d), want [%d,+%d)", gotBegin, gotLen, gapBegin, gapEnd-gapBegin)
	}
}
