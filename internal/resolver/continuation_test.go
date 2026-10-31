package resolver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

type continuationHealth struct {
	mu      sync.Mutex
	checks  map[string]int
	failure map[string]error
}

func (h *continuationHealth) Check(ctx context.Context, nzb []byte) (bool, error) {
	id := strings.TrimPrefix(string(nzb), "nzb:")
	h.mu.Lock()
	defer h.mu.Unlock()
	h.checks[id]++
	if err := h.failure[id]; err != nil {
		return false, err
	}
	return true, nil
}

func continuationReleases(n int) []core.Release {
	var releases []core.Release
	for i := 0; i < n; i++ {
		r := rel(fmt.Sprintf("source%02d", i), "The.Signal.1999.1080p.WEB-DL.H.264.AAC-GRP", 6000)
		r.PubDate = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
		releases = append(releases, r)
	}
	return releases
}

func TestResolveContinuesPastEightMissingReleasesAndSkipsThemOnRetry(t *testing.T) {
	ctx := context.Background()
	clock := &fakeClock{t: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)}
	h := &continuationHealth{checks: map[string]int{}, failure: map[string]error{}}
	for i := 0; i < 8; i++ {
		h.failure[fmt.Sprintf("source%02d", i)] = core.ErrArticleMissing
	}
	s := New(&fakeIndexer{releases: continuationReleases(10)}, h, nil, testCfg(), Options{Now: clock.now, HealthTTL: -1})
	defer s.Close()
	req := core.ResolveRequest{Title: matrixTitle(), Client: webCaps()}
	for attempt := 0; attempt < 2; attempt++ {
		res, err := s.Resolve(ctx, req)
		if err != nil || res.Release.ID != "source08" {
			t.Fatalf("attempt %d: resolution=%+v error=%v", attempt, res, err)
		}
	}
	h.mu.Lock()
	for i := 0; i < 8; i++ {
		id := fmt.Sprintf("source%02d", i)
		if h.checks[id] != 1 {
			t.Errorf("dead source %s was checked %d times", id, h.checks[id])
		}
	}
	delete(h.failure, "source00") // a repair or new provider makes it usable again
	h.mu.Unlock()
	clock.add(failedHealthTTL + time.Second)
	res, err := s.Resolve(ctx, req)
	if err != nil || res.Release.ID != "source00" {
		t.Fatalf("source did not recover after short pause: %+v %v", res, err)
	}
}

func TestVerifiedWorkingSourceIsTriedFirstAfterRestart(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	h := &continuationHealth{
		checks:  map[string]int{},
		failure: map[string]error{"source00": core.ErrArticleMissing},
	}
	idx := &fakeIndexer{releases: continuationReleases(2)}
	req := core.ResolveRequest{Title: matrixTitle(), Client: webCaps()}
	s := New(idx, h, db, testCfg(), Options{})
	if res, err := s.Resolve(ctx, req); err != nil || res.Release.ID != "source01" {
		t.Fatalf("initial working release: %+v %v", res, err)
	}
	s.Close()
	h.mu.Lock()
	before := h.checks["source00"]
	h.mu.Unlock()
	// The short failure cache is gone, but the successful health check is
	// persisted. A new service should pick that working source immediately.
	s = New(idx, h, db, testCfg(), Options{})
	defer s.Close()
	if res, err := s.Resolve(ctx, req); err != nil || res.Release.ID != "source01" {
		t.Fatalf("known working source was not preferred: %+v %v", res, err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.checks["source00"] != before {
		t.Fatal("restart re-probed the higher-ranked missing source")
	}
}

func TestValidationLimitDoesNotDeclareUntriedReleasesUnavailable(t *testing.T) {
	ctx := context.Background()
	h := &continuationHealth{checks: map[string]int{}, failure: map[string]error{}}
	for i := 0; i < maxHealthAttempts; i++ {
		h.failure[fmt.Sprintf("source%02d", i)] = core.ErrArticleMissing
	}
	s := New(&fakeIndexer{releases: continuationReleases(maxHealthAttempts + 1)}, h, nil, testCfg(), Options{})
	defer s.Close()
	req := core.ResolveRequest{Title: matrixTitle(), Client: webCaps()}
	if _, err := s.Resolve(ctx, req); !errors.Is(err, errHealthBudget) || errors.Is(err, core.ErrNotAvailable) {
		t.Fatalf("untried releases declared unavailable: %v", err)
	}
	if e, ok := s.getAvailability(ctx, int64(req.Title.Key)); ok && !e.available {
		t.Fatal("partial scan hid the title")
	}
	// A retry reaches the remaining release instead of fetching the first 32 again.
	res, err := s.Resolve(ctx, req)
	if err != nil || res.Release.ID != fmt.Sprintf("source%02d", maxHealthAttempts) {
		t.Fatalf("retry did not advance: %+v %v", res, err)
	}
}

func TestTransientHealthFailureIsNotSkippedOrCachedAsUnavailable(t *testing.T) {
	ctx := context.Background()
	h := &continuationHealth{
		checks:  map[string]int{},
		failure: map[string]error{"source00": context.DeadlineExceeded},
	}
	s := New(&fakeIndexer{releases: continuationReleases(1)}, h, nil, testCfg(), Options{})
	defer s.Close()
	req := core.ResolveRequest{Title: matrixTitle(), Client: webCaps()}
	// A timed-out check is not a confirmed failure: the release plays
	// unverified rather than leaving the viewer with nothing.
	if res, err := s.Resolve(ctx, req); err != nil || res.Release.ID != "source00" {
		t.Fatalf("timed-out source: %+v %v", res, err)
	}
	if s.recentlyVerified(ctx, "source00") || s.recentlyFailedHealth("source00") {
		t.Fatal("timed-out check was cached as verified or as failed")
	}
	if e, ok := s.getAvailability(ctx, int64(req.Title.Key)); ok {
		t.Fatalf("unverified play cached availability %+v", e)
	}
	h.mu.Lock()
	delete(h.failure, "source00")
	h.mu.Unlock()
	if res, err := s.Resolve(ctx, req); err != nil || res.Release.ID != "source00" {
		t.Fatalf("recovered provider was suppressed: %+v %v", res, err)
	}
}

func TestAllMissingSourcesAreRetriedAfterShortHealthPause(t *testing.T) {
	ctx := context.Background()
	clock := &fakeClock{t: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)}
	h := &continuationHealth{
		checks:  map[string]int{},
		failure: map[string]error{"source00": core.ErrArticleMissing},
	}
	s := New(&fakeIndexer{releases: continuationReleases(1)}, h, nil, testCfg(), Options{Now: clock.now})
	defer s.Close()
	req := core.ResolveRequest{Title: matrixTitle(), Client: webCaps()}
	if _, err := s.Resolve(ctx, req); !errors.Is(err, core.ErrNotAvailable) {
		t.Fatalf("missing source: %v", err)
	}
	h.mu.Lock()
	delete(h.failure, "source00")
	h.mu.Unlock()
	clock.add(failedHealthTTL + time.Second)
	if res, err := s.Resolve(ctx, req); err != nil || res.Release.ID != "source00" {
		t.Fatalf("source health failure hid title past the short pause: %+v %v", res, err)
	}
}
