package state

import (
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	_ "github.com/braedonsaunders/ether/internal/favorites"
	_ "github.com/braedonsaunders/ether/internal/notinterested"
	"github.com/braedonsaunders/ether/internal/store"
	_ "github.com/braedonsaunders/ether/internal/watchlist"
)

func TestPageUserDataReadsThePageInOneQuery(t *testing.T) {
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s := New(db, Options{})
	played := core.MovieKey(2)
	fresh := core.MovieKey(3)
	if err := s.Scrobble(ctx, 7, played); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO favorites_items(viewer_id,kind,subject_id,added_at) VALUES(7,'title',?,1)`, fresh.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO notinterested_titles(viewer_id,title_key,added_at,handle) VALUES(7,?,1,'h')`, played.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO watchlist_items(user_id,item_key,source,title,added) VALUES(7,?,'local','Fresh',1)`, int64(fresh)); err != nil {
		t.Fatal(err)
	}
	got, err := s.PageUserData(ctx, 7, []core.RatingKey{played, fresh, core.MovieKey(9)})
	if err != nil {
		t.Fatal(err)
	}
	if got[played].ViewCount != 1 || !got[played].IsNotInterested || got[played].IsFavorite || got[played].InWatchlist {
		t.Fatalf("played %+v", got[played])
	}
	if got[fresh].ViewCount != 0 || !got[fresh].IsFavorite || !got[fresh].InWatchlist || got[fresh].IsNotInterested {
		t.Fatalf("fresh %+v", got[fresh])
	}
	if got[core.MovieKey(9)].ViewCount != 0 || got[core.MovieKey(9)].IsFavorite {
		t.Fatalf("untouched %+v", got[core.MovieKey(9)])
	}
	if got[played].LastViewedAt.IsZero() || got[played].LastViewedAt.After(time.Now().Add(time.Minute)) {
		t.Fatalf("played time %+v", got[played].LastViewedAt)
	}
}
