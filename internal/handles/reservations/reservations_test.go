package reservations

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/store"
)

func reservationDB(t *testing.T) *sql.DB {
	t.Helper()
	if os.Getenv(store.TestPostgresEnv) != "" {
		return store.OpenTest(t)
	}
	db, err := store.OpenPath(context.Background(), filepath.Join(t.TempDir(), "reservations.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(8)
	t.Cleanup(func() { _ = db.Close() })
	return db
}
func TestReservationsBoundsRollbackAndExpiry(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db := reservationDB(t)
	p := ReservationPolicy{Namespace: "ticket", Shards: 2, PerShard: 2, PerViewerSurface: 1}
	insert := func(id int, viewer int64, surface string, now, expires int64, commit bool) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err = Reserve(ctx, db, tx, p, Reservation{Digest: fmt.Sprintf("%064x", id), ViewerID: viewer, Surface: surface, Now: now, Expires: expires}); err != nil {
			return err
		}
		if commit {
			return tx.Commit()
		}
		return nil
	}
	if err := insert(1, 1, "silo", 1, 10, false); err != nil {
		t.Fatal(err)
	}
	if err := insert(2, 1, "silo", 1, 10, true); err != nil {
		t.Fatal(err)
	}
	var capErr *CapacityError
	if err := insert(3, 1, "silo", 1, 10, true); !errors.As(err, &capErr) || capErr.Scope != "viewer" {
		t.Fatalf("viewer quota: %v", err)
	}
	if err := insert(4, 3, "emby", 1, 10, true); err != nil {
		t.Fatal(err)
	}
	if err := insert(5, 5, "silo", 1, 10, true); !errors.As(err, &capErr) || capErr.Scope != "shard" {
		t.Fatalf("shard quota: %v", err)
	}
	if err := insert(6, 2, "silo", 1, 10, true); err != nil {
		t.Fatalf("different shard: %v", err)
	}
	if err := insert(7, 1, "silo", 10, 20, true); err != nil {
		t.Fatalf("expired admission: %v", err)
	}
}
func TestReservationsConcurrentAdmission(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db := reservationDB(t)
	p := ReservationPolicy{Namespace: "ticket", Shards: 4, PerShard: 4, PerViewerSurface: 3}
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for i := 1; i <= 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				results <- err
				return
			}
			defer tx.Rollback()
			err = Reserve(ctx, db, tx, p, Reservation{Digest: fmt.Sprintf("%064x", i), ViewerID: 1, Surface: "silo", Now: 1, Expires: 10})
			if err == nil {
				err = tx.Commit()
			}
			results <- err
		}(i)
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
			continue
		}
		var capacity *CapacityError
		if !errors.As(err, &capacity) {
			t.Fatalf("non-capacity error: %v", err)
		}
	}
	if success != 3 {
		t.Fatalf("committed %d reservations, want 3", success)
	}
}
