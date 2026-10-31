package perfharness

import (
	"context"
	"testing"
	"time"

	_ "github.com/braedonsaunders/ether/internal/catalog" // migrations
	_ "github.com/braedonsaunders/ether/internal/state"   // migrations
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
