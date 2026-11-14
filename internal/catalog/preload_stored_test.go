package catalog

import (
	"context"
	"reflect"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// PreloadStored caches what loadByKey would read, detail time included, so a
// list's series checks are cache hits that Get still judges fresh or stale
// exactly as before.
func TestPreloadStoredMatchesLoadByKey(t *testing.T) {
	ctx := context.Background()
	c := newChunkTestCatalog(t)
	// This test deletes the backing rows; keep its cache minute stable.
	c.FreezeCacheMinute()
	now := c.now()
	var keys []core.RatingKey
	for id := int64(1); id <= 3; id++ {
		show := core.Title{Key: core.ShowKey(id), Type: core.TypeShow, Title: "Show", Genres: []string{"Drama"}, Released: now.AddDate(-1, 0, 0)}
		if err := c.upsert(ctx, show, upsertOpts{browsable: true, detail: id != 2}); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, show.Key)
	}
	want := map[core.RatingKey]*storedTitle{}
	for _, k := range keys {
		st, err := c.loadByKey(ctx, k)
		if err != nil {
			t.Fatal(err)
		}
		want[k] = st
	}
	c.cacheMu.Lock()
	c.titleCache = nil
	c.cacheMu.Unlock()
	c.PreloadStored(ctx, append(keys, keys[0], core.ShowKey(99)))
	// Served from the cache: the rows are gone, the generation is not.
	if _, err := c.db.ExecContext(ctx, `DELETE FROM catalog_titles WHERE type='show'`); err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		got, err := c.loadByKey(ctx, k)
		if err != nil {
			t.Fatalf("%v not preloaded: %v", k, err)
		}
		if !reflect.DeepEqual(got.t, want[k].t) || !got.detailAt.Equal(want[k].detailAt) || got.detail != want[k].detail {
			t.Fatalf("%v preloaded %+v, loaded %+v", k, got, want[k])
		}
	}
}

// Crossing titleCacheMax inside PreloadStored (whether filled by stale
// generations or earlier live reads) must retain the preloaded batch and recent
// entries instead of wiping the cache mid-batch and falling back to per-title
// loadByKey queries.
func TestPreloadStoredAcrossCacheBoundRetainsBatch(t *testing.T) {
	ctx := context.Background()
	c := newChunkTestCatalog(t)
	c.FreezeCacheMinute()
	now := c.now()
	var batch []core.RatingKey
	for id := int64(1); id <= 50; id++ {
		show := core.Title{Key: core.ShowKey(id), Type: core.TypeShow, Title: "Favorite Show", Released: now.AddDate(-1, 0, 0)}
		if err := c.upsert(ctx, show, upsertOpts{browsable: true, detail: true}); err != nil {
			t.Fatal(err)
		}
		batch = append(batch, show.Key)
	}
	for _, staleFill := range []bool{true, false} {
		gen, minute := c.writeGen.Load(), c.minuteBucket()
		fillGen := gen
		if staleFill && gen > 0 {
			fillGen = gen - 1
		}
		c.cacheMu.Lock()
		c.titleCache = map[core.RatingKey]cachedStoredTitle{}
		c.titleSeq = 0
		// Leave 39 slots free before titleCacheMax, matching CI run 42729089213
		// where PreloadStored(48) hit 8192 at item 39.
		for id := int64(1); id <= titleCacheMax-39; id++ {
			k := core.MovieKey(100_000 + id)
			c.putSnapshotLocked(k, fillGen, minute, storedTitle{t: core.Title{Key: k, Type: core.TypeMovie, Title: "Warm"}})
		}
		c.cacheMu.Unlock()

		c.PreloadStored(ctx, batch)

		qctx, stats := store.WithQueryStats(ctx)
		for _, k := range batch {
			if _, err := c.loadByKey(qctx, k); err != nil {
				t.Fatalf("staleFill=%v loadByKey(%v): %v", staleFill, k, err)
			}
		}
		if got := stats.Queries(); got != 0 {
			t.Fatalf("staleFill=%v: PreloadStored across titleCacheMax issued %d follow-up queries, want 0", staleFill, got)
		}
	}
}
