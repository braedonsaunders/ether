package watchlist

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestWatchlistedIncludesUnreleasedTitlesPerViewer(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	s := New(db, Options{Now: func() time.Time { return time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC) }})
	future := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	add := func(uid int64, source string, key core.RatingKey) {
		t.Helper()
		if err := s.upsert(ctx, uid, source, core.Title{Key: key, Released: future}); err != nil {
			t.Fatal(err)
		}
	}
	add(1, "plex", core.MovieKey(10))
	add(1, "local", core.MovieKey(10)) // the same title from two sources counts once
	add(1, "plex", core.SeasonKey(20, 2))
	add(1, "plex", core.ShowKey(20))
	add(2, "plex", core.ShowKey(30))
	add(2, "plex", core.CollectionKey(5)) // not a title the calendar can date
	got, err := s.Watchlisted(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got[1]) != fmt.Sprint([]core.RatingKey{core.MovieKey(10), core.ShowKey(20)}) || fmt.Sprint(got[2]) != fmt.Sprint([]core.RatingKey{core.ShowKey(30)}) {
		t.Fatalf("watchlisted: %v", got)
	}
}
