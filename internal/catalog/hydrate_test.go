package catalog

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/tmdb"
)

// slowTMDB answers movie details only when released.
type slowTMDB struct {
	TMDB
	release chan struct{}
	calls   atomic.Int64
}

func (s *slowTMDB) MovieDetails(ctx context.Context, id int64) (*tmdb.MovieDetails, error) {
	s.calls.Add(1)
	select {
	case <-s.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &tmdb.MovieDetails{Movie: tmdb.Movie{ID: id, Title: "Fresh", ReleaseDate: "2001-01-01"}}, nil
}

// A read never waits on TMDB for stored details, waits at most hydrateWait
// for a title with none, and the detached fetch still lands in the mirror.
func TestReadsDoNotWaitOnSlowMetadata(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	slow := &slowTMDB{release: make(chan struct{})}
	c, err := New(db, slow, config.Catalog{}, nil, WithNow(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	stale, summary := core.MovieKey(1), core.MovieKey(2)
	for _, key := range []core.RatingKey{stale, summary} {
		if err := c.UpsertTitle(ctx, core.Title{Key: key, Type: core.TypeMovie, IDs: core.IDs{TMDB: key.TMDB()}, Title: "Stored", Released: now.AddDate(0, -1, 0)}, true); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `UPDATE catalog_titles SET detail=1, detail_at=? WHERE key=?`, now.Add(-48*time.Hour).Unix(), int64(stale)); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	got, err := c.Get(ctx, stale)
	if err != nil || got.Title != "Stored" {
		t.Fatalf("stale read = %+v, %v", got, err)
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("stale read waited %s on TMDB", d)
	}

	start = time.Now()
	got, err = c.Get(ctx, summary)
	if err != nil || got.Title != "Stored" {
		t.Fatalf("summary read = %+v, %v", got, err)
	}
	if d := time.Since(start); d < hydrateWait || d > hydrateWait+time.Second {
		t.Fatalf("summary read took %s, want about %s", d, hydrateWait)
	}

	close(slow.release)
	c.WaitRefreshes()
	for _, key := range []core.RatingKey{stale, summary} {
		if got, err := c.Get(ctx, key); err != nil || got.Title != "Fresh" {
			t.Fatalf("after refresh %s = %+v, %v", key, got, err)
		}
	}
	if n := slow.calls.Load(); n != 2 {
		t.Fatalf("TMDB calls = %d, want one per title", n)
	}
}

// Concurrent readers of one title share a single refresh.
func TestConcurrentReadsShareOneRefresh(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	slow := &slowTMDB{release: make(chan struct{})}
	c, err := New(db, slow, config.Catalog{}, nil, WithNow(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	key := core.MovieKey(7)
	if err := c.UpsertTitle(ctx, core.Title{Key: key, Type: core.TypeMovie, IDs: core.IDs{TMDB: 7}, Title: "Stored", Released: now.AddDate(0, -1, 0)}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE catalog_titles SET detail=1, detail_at=? WHERE key=?`, now.Add(-48*time.Hour).Unix(), int64(key)); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	for range 20 {
		go func() {
			_, _ = c.Get(ctx, key)
			done <- struct{}{}
		}()
	}
	for range 20 {
		<-done
	}
	close(slow.release)
	c.WaitRefreshes()
	if n := slow.calls.Load(); n != 1 {
		t.Fatalf("TMDB calls = %d, want 1", n)
	}
}
