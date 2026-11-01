package catalog

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/tmdb"
)

type parentBatchTMDB struct {
	TMDB
	calls atomic.Int64 // refreshes run behind reads, concurrently
}

func (f *parentBatchTMDB) TVDetails(_ context.Context, id int64) (*tmdb.TVDetails, error) {
	f.calls.Add(1)
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
	if _, err := db.ExecContext(ctx, `UPDATE catalog_titles SET detail=1, detail_at=CASE WHEN key=? THEN CAST(? AS BIGINT) ELSE CAST(? AS BIGINT) END WHERE key IN (?,?)`, int64(warm), now.Unix(), now.Add(-48*time.Hour).Unix(), int64(warm), int64(stale)); err != nil {
		t.Fatal(err)
	}
	// Stale stored details are served at once and refreshed behind the read;
	// a show with no details waits for its fetch.
	got, err := c.ResolveShowParents(ctx, []core.RatingKey{warm, stale, cold, missing})
	if err != nil {
		t.Fatal(err)
	}
	c.waitHydrations()
	if got[warm].Title != "Original" || got[stale].Title != "Original" || got[cold].Title != "Refreshed" || got[missing].Key != 0 || client.calls.Load() != 3 {
		t.Fatalf("results %+v calls=%d", got, client.calls.Load())
	}
	if got, err = c.ResolveShowParents(ctx, []core.RatingKey{warm, stale, cold, missing}); err != nil {
		t.Fatal(err)
	}
	if got[stale].Title != "Refreshed" {
		t.Fatalf("stale show not refreshed behind the read: %+v", got[stale])
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

func TestResolveShowParentsIndependentControls(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	batchDB, legacyDB := store.OpenTest(t), store.OpenTest(t)
	makeCatalog := func(db *sql.DB) (*Catalog, *parentBatchTMDB) {
		client := &parentBatchTMDB{}
		c, err := New(db, client, config.Catalog{}, nil, WithNow(func() time.Time { return now }))
		if err != nil {
			t.Fatal(err)
		}
		return c, client
	}
	batch, bc := makeCatalog(batchDB)
	legacy, lc := makeCatalog(legacyDB)
	warm, stale, cold, missing := core.ShowKey(21), core.ShowKey(22), core.ShowKey(23), core.ShowKey(99)
	for _, dbcat := range []struct {
		db *sql.DB
		c  *Catalog
	}{{batchDB, batch}, {legacyDB, legacy}} {
		for _, key := range []core.RatingKey{warm, stale} {
			title := core.Title{Key: key, Type: core.TypeShow, IDs: core.IDs{TMDB: key.TMDB()}, Title: "Stored", Released: now.AddDate(-2, 0, 0), ContentRating: "TV-PG", Genres: []string{"Drama"}, Keywords: []string{"forest"}}
			if err := dbcat.c.UpsertTitle(ctx, title, true); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := dbcat.db.ExecContext(ctx, `UPDATE catalog_titles SET detail=1, detail_at=CASE WHEN key=? THEN CAST(? AS BIGINT) ELSE CAST(? AS BIGINT) END WHERE key IN (?,?)`, int64(warm), now.Unix(), now.Add(-48*time.Hour).Unix(), int64(warm), int64(stale)); err != nil {
			t.Fatal(err)
		}
	}
	keys := []core.RatingKey{warm, stale, cold, missing}
	got, err := batch.ResolveShowParents(ctx, keys)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range keys {
		want, e := legacy.Get(ctx, key)
		actual, ok := got[key]
		if e != nil {
			if ok {
				t.Fatalf("%s unexpectedly visible", key)
			}
			continue
		}
		if !ok || !reflect.DeepEqual(actual, *want) {
			t.Fatalf("%s batch=%+v legacy=%+v", key, actual, want)
		}
	}
	batch.WaitRefreshes()
	legacy.WaitRefreshes()
	if bc.calls.Load() != lc.calls.Load() || bc.calls.Load() != 3 {
		t.Fatalf("refresh calls batch=%d legacy=%d", bc.calls.Load(), lc.calls.Load())
	}
}

type parentFaultDriver struct {
	base driver.Driver
	fail *atomic.Bool
}

func (d parentFaultDriver) Open(name string) (driver.Conn, error) {
	c, e := d.base.Open(name)
	if e != nil {
		return nil, e
	}
	return &parentFaultConn{Conn: c, fail: d.fail}, nil
}

type parentFaultConn struct {
	driver.Conn
	fail *atomic.Bool
}

func (c *parentFaultConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	if strings.Contains(q, "detail_at FROM catalog_titles WHERE key IN") && c.fail.CompareAndSwap(true, false) {
		return nil, errors.New("injected batch failure")
	}
	if d, ok := c.Conn.(driver.QueryerContext); ok {
		return d.QueryContext(ctx, q, args)
	}
	return nil, driver.ErrSkip
}
func TestResolveShowParentsBatchFailureUsesLegacy(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "parents.db")
	base, err := store.OpenPath(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	drv := base.Driver()
	if err = base.Close(); err != nil {
		t.Fatal(err)
	}
	var fail atomic.Bool
	sql.Register("parent_batch_fault", parentFaultDriver{base: drv, fail: &fail})
	db, err := sql.Open("parent_batch_fault", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	c, err := New(db, nil, config.Catalog{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	key := core.ShowKey(51)
	title := core.Title{Key: key, Type: core.TypeShow, Title: "Fallback", Released: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), ContentRating: "TV-G", Genres: []string{"Drama"}}
	if err = c.UpsertTitle(ctx, title, true); err != nil {
		t.Fatal(err)
	}
	want, err := c.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	fail.Store(true)
	got, err := c.ResolveShowParents(ctx, []core.RatingKey{key, core.ShowKey(999)})
	if err != nil || fail.Load() || !reflect.DeepEqual(got[key], *want) || len(got) != 1 {
		t.Fatalf("fallback=%+v want=%+v fail=%v err=%v", got, want, fail.Load(), err)
	}
}
