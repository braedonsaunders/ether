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
	p := Policy{Namespace: "ticket", Shards: 2, PerShard: 2, PerViewerSurface: 1, TTL: time.Hour}
	ledger, err := New(db, p)
	if err != nil {
		t.Fatal(err)
	}
	insert := func(id int, viewer int64, surface string, now, expires int64, commit bool) error {
		tx, err := ledger.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err = tx.Reserve(ctx, Reservation{Digest: fmt.Sprintf("%064x", id), ViewerID: viewer, Surface: surface}); err != nil {
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
	if _, err := db.ExecContext(ctx, `UPDATE handle_reservations SET expires=0 WHERE namespace=? AND shard=?`, "ticket", 1); err != nil {
		t.Fatal(err)
	}
	if err := insert(7, 1, "silo", 10, 20, true); err != nil {
		t.Fatalf("expired admission: %v", err)
	}
}
func TestReservationsConcurrentAdmission(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db := reservationDB(t)
	p := Policy{Namespace: "ticket", Shards: 4, PerShard: 4, PerViewerSurface: 3, TTL: time.Hour}
	ledger, err := New(db, p)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for i := 1; i <= 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tx, err := ledger.Begin(ctx)
			if err != nil {
				results <- err
				return
			}
			defer tx.Rollback()
			_, err = tx.Reserve(ctx, Reservation{Digest: fmt.Sprintf("%064x", i), ViewerID: 1, Surface: "silo"})
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

func TestReservationsReleaseScopeAndDomainRollback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db := reservationDB(t)
	if _, err := db.ExecContext(ctx, `CREATE TABLE reservation_test_domain(digest TEXT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	ledger, err := New(db, Policy{Namespace: "ticket", Shards: 2, PerShard: 4, PerViewerSurface: 4, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	r := Reservation{Digest: fmt.Sprintf("%064x", 1), ViewerID: 1, Surface: "silo"}
	tx, err := ledger.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	expiry, err := tx.Reserve(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	var storedExpiry int64
	if err = tx.SQL().QueryRowContext(ctx, `SELECT expires FROM handle_reservations WHERE digest=?`, r.Digest).Scan(&storedExpiry); err != nil || expiry != storedExpiry {
		t.Fatalf("expiry=%d stored=%d err=%v", expiry, storedExpiry, err)
	}
	if _, err = tx.SQL().ExecContext(ctx, `INSERT INTO reservation_test_domain(digest) VALUES(?)`, r.Digest); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	tx, err = ledger.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	foreign := r
	foreign.Surface = "emby"
	if removed, releaseErr := tx.Release(ctx, foreign); releaseErr != nil || removed {
		t.Fatalf("foreign release removed=%t err=%v", removed, releaseErr)
	}
	missing := r
	missing.Digest = fmt.Sprintf("%064x", 99)
	if removed, releaseErr := tx.Release(ctx, missing); releaseErr != nil || removed {
		t.Fatalf("missing release removed=%t err=%v", removed, releaseErr)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var n int
	if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM handle_reservations WHERE digest=?`, r.Digest).Scan(&n); err != nil || n != 1 {
		t.Fatalf("foreign release count=%d err=%v", n, err)
	}
	tx, err = ledger.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	next := r
	next.Digest = fmt.Sprintf("%064x", 2)
	if _, err = tx.Reserve(ctx, next); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.SQL().ExecContext(ctx, `INSERT INTO reservation_test_domain(digest) VALUES(?)`, r.Digest); err == nil {
		t.Fatal("duplicate domain insert succeeded")
	}
	_ = tx.Rollback()
	if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM handle_reservations WHERE digest=?`, next.Digest).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rollback reservation count=%d err=%v", n, err)
	}
}

func TestReservationsRejectCrossShardTransaction(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db := reservationDB(t)
	ledger, err := New(db, Policy{Namespace: "ticket", Shards: 2, PerShard: 4, PerViewerSurface: 4, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := ledger.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	first := Reservation{Digest: fmt.Sprintf("%064x", 1), ViewerID: 1, Surface: "silo"}
	if _, err = tx.Reserve(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := first
	second.Digest = fmt.Sprintf("%064x", 2)
	second.ViewerID = 2
	if _, err = tx.Reserve(ctx, second); err == nil {
		t.Fatal("cross-shard reservation accepted")
	}
	if removed, err := tx.Release(ctx, second); err == nil || removed {
		t.Fatalf("cross-shard release removed=%t err=%v", removed, err)
	}
}
