package perfharness

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	_ "github.com/braedonsaunders/ether/internal/state" // migrations
	"github.com/braedonsaunders/ether/internal/store"
)

func TestGenerateBuildsTheScale(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := SmallScale.Divide(50)
	s.Viewers, s.HistoryPerViewer = 2, 30
	if err := Generate(ctx, db, s, time.Now()); err != nil {
		t.Fatal(err)
	}
	var titles, history, fts, triggers int
	for q, dst := range map[string]*int{
		`SELECT COUNT(*) FROM catalog_titles`:                                                   &titles,
		`SELECT COUNT(*) FROM state_history`:                                                    &history,
		`SELECT COUNT(*) FROM catalog_titles_fts WHERE title LIKE '%Episode 1%'`:                &fts,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND tbl_name='catalog_titles'`: &triggers,
	} {
		if err := db.QueryRowContext(ctx, q).Scan(dst); err != nil {
			t.Fatal(q, err)
		}
	}
	if titles != s.Rows() || history != 3*30 || fts == 0 || triggers == 0 {
		t.Fatalf("titles %d (want %d) history %d fts %d triggers %d", titles, s.Rows(), history, fts, triggers)
	}
}

func TestMeasureSortTitleFold540k(t *testing.T) {
	if os.Getenv("ETHER_PERF_SORT_FOLD") == "" {
		t.Skip("set ETHER_PERF_SORT_FOLD=1 to measure 540k sort_title fold startup cost")
	}
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	genStart := time.Now()
	if err := Generate(ctx, db, LargeScale, time.Now()); err != nil {
		t.Fatal(err)
	}
	t.Logf("generated %d rows in %s", LargeScale.Rows(), time.Since(genStart).Round(time.Millisecond))

	// 1. Measure cold startup scan over all 540,000 rows (270 keyset pages of 2,000 rows).
	coldStart := time.Now()
	if _, err := catalog.New(db, nil, config.Catalog{}, nil); err != nil {
		t.Fatal(err)
	}
	coldScanDur := time.Since(coldStart)
	t.Logf("1. cold 540k scan (0 unfolded rows, 270 keyset pages of 2,000): %s", coldScanDur.Round(time.Millisecond))

	// 2. Measure warm startup (meta marker already set).
	warmStart := time.Now()
	if _, err := catalog.New(db, nil, config.Catalog{}, nil); err != nil {
		t.Fatal(err)
	}
	warmDur := time.Since(warmStart)
	t.Logf("2. warm 540k startup (sort_title_folded=1 marker present): %s", warmDur.Round(time.Microsecond))

	// 3. Seed unfolded accented titles across the 540k table (5,000 spread across
	// movie keys + a full 2,000-row contiguous page) with triggers suspended during
	// fixture mutation, then clear the meta marker to measure keyset-paged migration with writes.
	restore, err := suspendTriggers(ctx, db, "catalog_titles")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= LargeScale.Movies; i += 50 {
		key := int64(core.MovieKey(int64(i)))
		title := fmt.Sprintf("Léon %d", i)
		rawSort := fmt.Sprintf("léon %d", i)
		if _, err := tx.ExecContext(ctx, `UPDATE catalog_titles SET title=?, sort_title=? WHERE key=?`, title, rawSort, key); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= 2000; i++ {
		key := int64(core.MovieKey(int64(i)))
		title := fmt.Sprintf("Été %d", i)
		rawSort := fmt.Sprintf("été %d", i)
		if _, err := tx.ExecContext(ctx, `UPDATE catalog_titles SET title=?, sort_title=? WHERE key=?`, title, rawSort, key); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM catalog_meta WHERE k='sort_title_folded'`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := restore(); err != nil {
		t.Fatal(err)
	}

	writeStart := time.Now()
	if _, err := catalog.New(db, nil, config.Catalog{}, nil); err != nil {
		t.Fatal(err)
	}
	writeDur := time.Since(writeStart)
	t.Logf("3. cold 540k scan + folding 6,960 accented rows across keyset batches: %s", writeDur.Round(time.Millisecond))
}
