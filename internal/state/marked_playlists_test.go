package state

import (
	"context"
	"sync"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestEnsureMarkedPlaylistConcurrent(t *testing.T) {
	s, _ := newTestState(t)
	marked := s.Playlists().(core.MarkedPlaylists)
	const n = 8
	keys := make(chan core.RatingKey, n)
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, e := marked.EnsureMarkedPlaylist(context.Background(), 1, "favorites", "Favorites")
			if e != nil {
				errs <- e
				return
			}
			keys <- p.Key
		}()
	}
	wg.Wait()
	close(keys)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	var key core.RatingKey
	for k := range keys {
		if key == 0 {
			key = k
		}
		if k != key {
			t.Fatalf("duplicate marked playlist: %v %v", key, k)
		}
	}
	lists, err := s.Playlists().List(context.Background(), 1)
	if err != nil || len(lists) != 1 {
		t.Fatalf("lists %+v %v", lists, err)
	}
	if _, err = s.db.ExecContext(context.Background(), `UPDATE state_playlists SET title=? WHERE id=?`, "Renamed", int64(key)%1_000_000_000_000_000); err != nil {
		t.Fatal(err)
	}
	p, err := marked.MarkedPlaylist(context.Background(), 1, "favorites")
	if err != nil || p.Key != key || p.Title != "Renamed" {
		t.Fatalf("marker lost after rename: %+v %v", p, err)
	}
}
