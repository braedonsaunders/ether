package service

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/tmdb"
)

func TestArtworkVariantsQueueIsNonblockingAndCoalesces(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	finish := make(chan struct{})
	var calls atomic.Int32
	var ready atomic.Bool
	q := NewVariantsQueue(ctx, func(*core.Title, string) (*tmdb.Images, bool) {
		if ready.Load() {
			return &tmdb.Images{Posters: []tmdb.ImageFile{{FilePath: "/a.jpg"}}}, true
		}
		return nil, false
	}, func(ctx context.Context, title *core.Title, language string) (*tmdb.Images, error) {
		calls.Add(1)
		if title.Key != core.MovieKey(42) || language != "en" {
			t.Errorf("unexpected request: %+v %q", title, language)
		}
		close(started)
		select {
		case <-finish:
			ready.Store(true)
			return &tmdb.Images{}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	title := &core.Title{Key: core.MovieKey(42), Type: core.TypeMovie}
	if _, ok := q.Get(title, "en"); ok {
		t.Fatal("miss reported as complete")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("fetch not started")
	}
	for i := 0; i < 10; i++ {
		if _, ok := q.Get(title, "en"); ok {
			t.Fatal("pending reported as complete")
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("duplicate fetches: %d", calls.Load())
	}
	close(finish)
	deadline := time.Now().Add(time.Second)
	for !ready.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if images, ok := q.Get(title, "en"); !ok || len(images.Posters) != 1 {
		t.Fatal("filled entry unavailable")
	}
}
