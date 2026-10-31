package resolver

import (
	"context"
	"errors"
	"github.com/braedonsaunders/ether/internal/core"
	"sync/atomic"
	"testing"
)

type outageHealth struct{ down atomic.Bool }

func (h *outageHealth) Check(context.Context, []byte) (bool, error) {
	if h.down.Load() {
		return false, errors.New("provider unavailable")
	}
	return true, nil
}

func TestProviderOutageNeverCachesTitleUnavailable(t *testing.T) {
	h := &outageHealth{}
	h.down.Store(true)
	s := New(&fakeIndexer{releases: movieReleases()}, h, nil, testCfg(), Options{})
	defer s.Close()
	req := core.ResolveRequest{Title: matrixTitle(), Client: appleTVCaps()}
	if _, err := s.Resolve(context.Background(), req); err == nil || errors.Is(err, core.ErrNotAvailable) {
		t.Fatalf("outage classified as permanent: %v", err)
	}
	if known, available := s.Available(context.Background(), req.Title.Key); known && !available {
		t.Fatal("outage hid title")
	}
	h.down.Store(false)
	if _, err := s.Resolve(context.Background(), req); err != nil {
		t.Fatalf("recovered provider still unavailable: %v", err)
	}
}

func TestOptimisticOutageDoesNotBlacklistRelease(t *testing.T) {
	h := &outageHealth{}
	h.down.Store(true)
	s := New(&fakeIndexer{}, h, nil, testCfg(), Options{})
	s.optimisticCheck(core.Release{ID: "test"}, []byte("nzb"))
	s.bgWG.Wait()
	if s.isBad("test") {
		t.Fatal("outage permanently blacklisted release")
	}
	s.Close()
}

type backgroundHealth struct{ background atomic.Bool }

func (h *backgroundHealth) Check(ctx context.Context, _ []byte) (bool, error) {
	h.background.Store(core.IsBackgroundCheck(ctx))
	return true, nil
}

func TestOptimisticCheckRunsInBackground(t *testing.T) {
	h := &backgroundHealth{}
	s := New(&fakeIndexer{}, h, nil, testCfg(), Options{})
	s.optimisticCheck(core.Release{ID: "test"}, []byte("nzb"))
	s.bgWG.Wait()
	s.Close()
	if !h.background.Load() {
		t.Fatal("optimistic health check competed at live priority")
	}
}
