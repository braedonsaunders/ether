package resolver

// Intent: ETHE-18's accepted background refill must recover only missing/expired
// candidates, deduplicate by episode across clients, cap work, and cancel/join
// at shutdown without descriptor/health/provider/console side effects.

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

type refillSearch struct {
	ctx context.Context
	req core.SearchRequest
}

type refillIndexer struct {
	started  chan refillSearch
	release  chan struct{}
	canceled chan struct{}
	exit     chan struct{}
	results  []core.Release
	err      error
	searches atomic.Int32
	fetches  atomic.Int32
}

func (i *refillIndexer) Name() string { return "offline-refill" }
func (i *refillIndexer) Search(ctx context.Context, req core.SearchRequest) ([]core.Release, error) {
	i.searches.Add(1)
	i.started <- refillSearch{ctx, req}
	if i.release != nil {
		select {
		case <-i.release:
		case <-ctx.Done():
			if i.canceled != nil {
				i.canceled <- struct{}{}
			}
			if i.exit != nil {
				<-i.exit
			}
			return nil, ctx.Err()
		}
	}
	return append([]core.Release(nil), i.results...), i.err
}
func (i *refillIndexer) FetchNZB(context.Context, core.Release) ([]byte, error) {
	i.fetches.Add(1)
	return nil, errors.New("refill must never fetch a descriptor")
}
func waitRefill(t *testing.T, done <-chan struct{}) {
	t.Helper()
	if done == nil {
		t.Fatal("refill skipped")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("refill did not finish")
	}
}
func startedRefill(t *testing.T, i *refillIndexer) refillSearch {
	t.Helper()
	select {
	case call := <-i.started:
		return call
	case <-time.After(5 * time.Second):
		t.Fatal("search did not start")
		return refillSearch{}
	}
}
func refillEpisode(episode int) *core.Title {
	return &core.Title{Key: core.EpisodeKey(400063, 3, episode), Type: core.TypeEpisode,
		ShowTitle: "Taskmaster", Title: "Taskmaster", Season: 3, Episode: episode, Runtime: 45 * time.Minute}
}

func TestRefillSearchCacheDedupCapacityAndNoQueue(t *testing.T) {
	idx := &refillIndexer{started: make(chan refillSearch, 8), release: make(chan struct{}), results: variantReleases()}
	var health atomic.Int64
	var observed atomic.Int32
	s := New(idx, fakeHealth{checks: &health}, nil, testCfg(), Options{
		SearchObserved: func(context.Context, *core.Title, []core.Release) { observed.Add(1) },
		Discovered:     func(context.Context, *core.Title) []core.Release { observed.Add(1); return nil },
		Observed:       func(context.Context, *core.Title, core.Release) { observed.Add(1) },
	})
	defer s.Close()
	first := s.RefillSearchCache(refillEpisode(5))
	call := startedRefill(t, idx)
	if call.req.Episode != 5 || call.req.Season != 3 || call.req.IDs.TMDB != 400063 {
		t.Fatalf("wrong episode search: %+v", call.req)
	}
	// Different title objects/viewers/clients share the existing search key;
	// no request identity enters this resolver seam.
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := s.RefillSearchCache(refillEpisode(5)); got != first {
				t.Error("duplicate refill did not join")
			}
		}()
	}
	wg.Wait()
	second := s.RefillSearchCache(refillEpisode(6))
	startedRefill(t, idx)
	if second == nil || second == first {
		t.Fatal("different episodes must use distinct slots")
	}
	if s.RefillSearchCache(refillEpisode(7)) != nil {
		t.Fatal("third refill must skip, not queue")
	}
	close(idx.release)
	waitRefill(t, first)
	waitRefill(t, second)
	if idx.searches.Load() != 2 {
		t.Fatal("skipped title was queued or duplicates searched")
	}
	third := s.RefillSearchCache(refillEpisode(7))
	waitRefill(t, third) // only a fresh admission starts skipped work
	if idx.searches.Load() != 3 || idx.fetches.Load() != 0 || health.Load() != 0 || observed.Load() != 0 || len(s.Traces()) != 0 {
		t.Fatal("refill performed extra searches, source validation or console work")
	}
}

func TestRefillSearchCacheLifetimeAndFailures(t *testing.T) {
	for _, tc := range []struct {
		name    string
		results []core.Release
		err     error
		ttl     time.Duration
	}{
		{"results", variantReleases(), nil, 30 * time.Minute},
		{"empty", nil, nil, freshReleaseCooloff},
		{"error", nil, errors.New("offline search failed"), 0},
		{"budget", nil, core.ErrBudgetExhausted, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := &fakeClock{t: time.Now()}
			idx := &refillIndexer{started: make(chan refillSearch, 8), results: tc.results, err: tc.err}
			s := New(idx, nil, nil, testCfg(), Options{Now: clock.now})
			defer s.Close()
			title := matrixTitle()
			waitRefill(t, s.RefillSearchCache(title))
			if idx.searches.Load() != 1 {
				t.Fatal("automatic retry")
			}
			if tc.ttl > 0 {
				if s.RefillSearchCache(title) != nil {
					t.Fatal("valid cache must skip, including empty results")
				}
				clock.add(tc.ttl - time.Nanosecond)
				if s.RefillSearchCache(title) != nil {
					t.Fatal("cache expired early")
				}
				clock.add(time.Nanosecond)
			} else {
				if _, ok := s.peekSearchCache(searchCacheKey(searchRequestFor(title))); ok {
					t.Fatal("failed search cached as successful")
				}
			}
			// Expiry/errors allow a new explicit admission, never an internal retry.
			waitRefill(t, s.RefillSearchCache(title))
			if idx.searches.Load() != 2 || idx.fetches.Load() != 0 {
				t.Fatal("refill count/source work")
			}
		})
	}
}

func TestRefillSearchCacheDeadline(t *testing.T) {
	// Virtual time exercises the actual 45-second context deadline without
	// wall-clock sleeps, polling or a special production timeout setting.
	synctest.Test(t, func(t *testing.T) {
		idx := &refillIndexer{started: make(chan refillSearch, 1), release: make(chan struct{})}
		s := New(idx, nil, nil, testCfg(), Options{})
		defer s.Close()
		start := time.Now()
		done := s.RefillSearchCache(matrixTitle())
		call := <-idx.started
		deadline, ok := call.ctx.Deadline()
		if !ok || deadline.Sub(start) != 45*time.Second {
			t.Fatalf("deadline=%v present=%v", deadline.Sub(start), ok)
		}
		<-done // virtual time advances only when every goroutine is blocked
		if call.ctx.Err() != context.DeadlineExceeded || time.Since(start) != 45*time.Second || idx.searches.Load() != 1 {
			t.Fatal("refill did not stop at its ceiling without retry")
		}
	})
}

func TestRefillSearchCacheShutdownCancelsAndJoins(t *testing.T) {
	idx := &refillIndexer{started: make(chan refillSearch, 1), release: make(chan struct{}), canceled: make(chan struct{}, 1), exit: make(chan struct{})}
	s := New(idx, nil, nil, testCfg(), Options{})
	done := s.RefillSearchCache(matrixTitle())
	call := startedRefill(t, idx)
	closed := make(chan struct{})
	go func() { s.Close(); close(closed) }()
	select {
	case <-idx.canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not cancel search")
	}
	if call.ctx.Err() != context.Canceled {
		t.Fatal("search context not canceled")
	}
	select {
	case <-closed:
		t.Fatal("Close returned before joining worker")
	default:
	}
	if s.RefillSearchCache(refillEpisode(5)) != nil {
		t.Fatal("closed service admitted work")
	}
	close(idx.exit)
	waitRefill(t, done)
	waitRefill(t, closed)
	if s.RefillSearchCache(matrixTitle()) != nil || idx.searches.Load() != 1 {
		t.Fatal("work started after shutdown")
	}
}
