package stream

import (
	"context"
	"sync"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

// keyRecorder notes the core.StreamKey of every live article fetch.
type keyRecorder struct {
	*fakeFetcher
	mu   sync.Mutex
	keys []any
}

func (r *keyRecorder) BodyPart(ctx context.Context, id string, groups []string, prio core.Priority) ([]byte, core.ArticlePart, error) {
	if prio == core.PriorityLive {
		r.mu.Lock()
		r.keys = append(r.keys, core.StreamKey(ctx))
		r.mu.Unlock()
	}
	return r.fakeFetcher.BodyPart(ctx, id, groups, prio)
}

func (r *keyRecorder) take() []any {
	r.mu.Lock()
	defer r.mu.Unlock()
	keys := r.keys
	r.keys = nil
	return keys
}

// Live article fetches carry their playback stream's key through the
// engine's coalesced flights, so the Usenet fetcher scopes its head-of-line
// overtaken signal to one viewer: the plan probe is keyed by its release and
// each source's reads by that source.
func TestLiveFetchesCarryTheirStreamKey(t *testing.T) {
	data := mkvBytes(41, 3_000_000)
	r := &keyRecorder{fakeFetcher: newFakeFetcher()}
	doc := post(t, r.fakeFetcher, 100_000, postFile{name: "video.mkv", data: data})
	e := newTestEngine(t, r, nil, Options{ReadAheadMin: 1, ReadAheadMax: 1})

	first := openSource(t, e, "one", doc).(*source)
	probe := r.take()
	if len(probe) == 0 {
		t.Fatal("open fetched nothing live")
	}
	for _, k := range probe {
		if k != any(first.rel) {
			t.Fatalf("plan probe key = %v, want its release", k)
		}
	}
	second := openSource(t, e, "two", doc).(*source)
	r.take()

	buf := make([]byte, 4096)
	for _, c := range []struct {
		src *source
		off int64
	}{{first, 1_000_000}, {second, 2_500_000}} {
		// No read-ahead, so no speculative fetch can pre-empt the next read.
		if _, err := c.src.ReadMetadataAtContext(context.Background(), buf, c.off); err != nil {
			t.Fatal(err)
		}
		keys := r.take()
		if len(keys) == 0 {
			t.Fatalf("read at %d fetched nothing live", c.off)
		}
		for _, k := range keys {
			if k != any(c.src) {
				t.Fatalf("read at %d carried key %v, want its own source", c.off, k)
			}
		}
	}
}
