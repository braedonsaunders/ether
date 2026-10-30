package catalog

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/tmdb"
)

type parentBatchTMDB struct {
	TMDB
	calls int
}

func (f *parentBatchTMDB) TVDetails(_ context.Context, id int64) (*tmdb.TVDetails, error) {
	f.calls++
	if id == 99 {
		return nil, errors.New("missing")
	}
	return &tmdb.TVDetails{TVShow: tmdb.TVShow{ID: id, Name: "Refreshed", FirstAirDate: "2000-01-01"}}, nil
}
func TestResolveShowParentsMatchesGet(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	client := &parentBatchTMDB{}
	c, err := New(db, client, config.Catalog{}, nil, WithNow(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	warm, stale, cold, missing := core.ShowKey(1), core.ShowKey(2), core.ShowKey(3), core.ShowKey(99)
	for _, key := range []core.RatingKey{warm, stale} {
		if err := c.UpsertTitle(ctx, core.Title{Key: key, Type: core.TypeShow, IDs: core.IDs{TMDB: key.TMDB()}, Title: "Original", Released: now.AddDate(-2, 0, 0)}, true); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `UPDATE catalog_titles SET detail=1, detail_at=CASE WHEN key=? THEN ? ELSE ? END WHERE key IN (?,?)`, int64(warm), now.Unix(), now.Add(-48*time.Hour).Unix(), int64(warm), int64(stale)); err != nil {
		t.Fatal(err)
	}
	got, err := c.ResolveShowParents(ctx, []core.RatingKey{warm, stale, cold, missing})
	if err != nil {
		t.Fatal(err)
	}
	if got[warm].Title != "Original" || got[stale].Title != "Refreshed" || got[cold].Title != "Refreshed" || got[missing].Key != 0 || client.calls != 3 {
		t.Fatalf("results %+v calls=%d", got, client.calls)
	}
	for _, key := range []core.RatingKey{warm, stale, cold, missing} {
		legacy, e := c.Get(ctx, key)
		if e != nil {
			if _, ok := got[key]; ok {
				t.Fatalf("unexpected key %s", key)
			}
			continue
		}
		if got[key].Key != legacy.Key || got[key].Title != legacy.Title {
			t.Fatalf("mismatch %s", key)
		}
	}
}

func TestResolveShowParentsPreservesHomeAdmission(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c, err := New(db, nil, config.Catalog{GateMoviesByHomeRelease: true}, nil, WithNow(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	key := core.MovieKey(10)
	movie := core.Title{Key: key, Type: core.TypeMovie, IDs: core.IDs{TMDB: 10}, Title: "Eligible", Released: now.AddDate(-1, 0, 0)}
	if err = c.UpsertTitle(ctx, movie, true); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO catalog_home_release(key,checked,earliest_ts,fallback_admitted,attempted_at) VALUES(?,1,0,0,0)`, int64(key)); err != nil {
		t.Fatal(err)
	}
	got, err := c.ResolveShowParents(ctx, []core.RatingKey{key})
	if err != nil || got[key].Key != key {
		t.Fatalf("eligible %+v %v", got, err)
	}
	var admitted int
	if err = db.QueryRowContext(ctx, `SELECT fallback_admitted FROM catalog_home_release WHERE key=?`, int64(key)).Scan(&admitted); err != nil || admitted != 1 {
		t.Fatalf("admission=%d err=%v", admitted, err)
	}
	legacy, err := c.Get(ctx, key)
	if err != nil || legacy.Key != got[key].Key {
		t.Fatalf("legacy %+v %v", legacy, err)
	}
}
