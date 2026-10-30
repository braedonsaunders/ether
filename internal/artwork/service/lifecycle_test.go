package service

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/tmdb"
)

func TestVariantsWorkersJoinOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	q := NewVariantsQueue(ctx, func(*core.Title, string) (*tmdb.Images, bool) { return nil, false }, func(ctx context.Context, _ *core.Title, _ string) (*tmdb.Images, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	q.Get(&core.Title{Key: core.MovieKey(1)}, "en")
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("worker never started")
	}
	cancel()
	done := make(chan struct{})
	go func() { q.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("workers did not exit")
	}
	if _, ok := q.Get(&core.Title{Key: core.MovieKey(1)}, "en"); ok {
		t.Fatal("cancelled queue served a value")
	}
}

func TestMetadataWorkersJoinOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	q := NewMetadataQueue(ctx, nil, nil, nil)
	cancel()
	done := make(chan struct{})
	go func() { q.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("metadata workers did not exit")
	}
}
