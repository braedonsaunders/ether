package app

import (
	"context"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/tmdb"
)

// artworkVariantsQueue keeps item enrichment cache-only. Bounded workers
// fills missing TMDB responses and coalesces concurrent requests per item and
// language. It stops when the frontend runtime's context is cancelled.
type artworkVariantsQueue struct {
	ctx     context.Context
	cached  func(*core.Title, string) (*tmdb.Images, bool)
	fetch   func(context.Context, *core.Title, string) (*tmdb.Images, error)
	jobs    chan artworkVariantJob
	mu      sync.Mutex
	pending map[artworkVariantKey]bool
	retry   map[artworkVariantKey]time.Time
}
type artworkVariantKey struct {
	item     core.RatingKey
	language string
}
type artworkVariantJob struct {
	key   artworkVariantKey
	title core.Title
}

func newArtworkVariantsQueue(ctx context.Context, cached func(*core.Title, string) (*tmdb.Images, bool), fetch func(context.Context, *core.Title, string) (*tmdb.Images, error)) *artworkVariantsQueue {
	q := &artworkVariantsQueue{ctx: ctx, cached: cached, fetch: fetch, jobs: make(chan artworkVariantJob, 128), pending: make(map[artworkVariantKey]bool), retry: make(map[artworkVariantKey]time.Time)}
	for i := 0; i < 4; i++ {
		go q.run()
	}
	return q
}

func (q *artworkVariantsQueue) get(title *core.Title, language string) (*tmdb.Images, bool) {
	if title == nil || q.ctx.Err() != nil {
		return nil, false
	}
	if value, ok := q.cached(title, language); ok {
		return value, true
	}
	key := artworkVariantKey{title.Key, language}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.pending[key] || time.Now().Before(q.retry[key]) || len(q.pending) >= cap(q.jobs) {
		return nil, false
	}
	// discoveryPath needs only these fields. Do not retain caller-owned slices.
	copy := core.Title{Key: title.Key, Type: title.Type, IDs: title.IDs, ShowKey: title.ShowKey, Season: title.Season, Episode: title.Episode}
	select {
	case q.jobs <- artworkVariantJob{key: key, title: copy}:
		q.pending[key] = true
	default:
	}
	return nil, false
}

func (q *artworkVariantsQueue) run() {
	for {
		select {
		case <-q.ctx.Done():
			return
		case job := <-q.jobs:
			ctx, cancel := context.WithTimeout(q.ctx, 20*time.Second)
			_, err := q.fetch(ctx, &job.title, job.key.language)
			cancel()
			q.mu.Lock()
			delete(q.pending, job.key)
			if err != nil && q.ctx.Err() == nil {
				// Cap failed-key bookkeeping, too. Under load, a full cache drops
				// retry records instead of allowing unbounded memory growth.
				if len(q.retry) >= 1024 {
					for key, until := range q.retry {
						if !time.Now().Before(until) {
							delete(q.retry, key)
						}
					}
				}
				if len(q.retry) < 1024 {
					q.retry[job.key] = time.Now().Add(time.Minute)
				}
			} else {
				delete(q.retry, job.key)
			}
			q.mu.Unlock()
		}
	}
}
