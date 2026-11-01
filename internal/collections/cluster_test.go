package collections_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/collections"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestImportedCollectionIDsAreUniqueAcrossNodes(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ether.db")
	var dbs []*sql.DB
	for i := 0; i < 2; i++ {
		db, err := store.OpenPath(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		dbs = append(dbs, db)
	}
	var mu sync.Mutex
	ids := map[int64]bool{}
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 10; i++ {
		for _, db := range dbs {
			wg.Add(1)
			go func(db *sql.DB) {
				defer wg.Done()
				tx, err := db.BeginTx(ctx, nil)
				if err != nil {
					errs <- err
					return
				}
				defer tx.Rollback()
				id, err := collections.ImportTitlesTx(ctx, tx, 0, 1, "List", []core.RatingKey{core.MovieKey(1)}, time.Unix(1, 0))
				if err == nil {
					err = tx.Commit()
				}
				if err != nil {
					errs <- err
					return
				}
				mu.Lock()
				ids[id] = true
				mu.Unlock()
			}(db)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	var n int
	if err := dbs[0].QueryRow(`SELECT COUNT(*) FROM collections_specs WHERE owner_user_id=1`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if len(ids) != 20 || n != 20 {
		t.Fatalf("distinct ids %d, rows %d, want 20", len(ids), n)
	}
}
