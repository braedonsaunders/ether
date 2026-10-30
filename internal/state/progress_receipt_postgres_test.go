package state

import (
	"context"
	"database/sql"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
	"os"
	"testing"
	"time"
)

func TestPostgresProgressReceiptUnavailableDatabase(t *testing.T) {
	if os.Getenv(store.TestPostgresEnv) == "" {
		t.Skip("PostgreSQL fixture required")
	}
	db := store.OpenTest(t)
	s := New(db, Options{Availability: store.NewAvailability(nil)})
	t.Cleanup(func() {
		if err := s.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	if _, err := db.Exec(`CREATE TABLE test_receipt(value INTEGER)`); err != nil {
		t.Fatal(err)
	}
	blocker, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback()
	if _, err = blocker.Exec(`LOCK TABLE state_watch IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	receipt := func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO test_receipt VALUES(1)`)
		return err
	}
	if err = s.ProgressWithReceipt(ctx, 1, core.MovieKey(1), time.Minute, 10*time.Minute, "playing", receipt); err == nil {
		t.Fatal("unavailable write acknowledged")
	}
	if err = blocker.Rollback(); err != nil {
		t.Fatal(err)
	}
	var n int
	if err = db.QueryRow(`SELECT COUNT(*) FROM test_receipt`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("receipt persisted after timeout: %d %v", n, err)
	}
	ctx = context.Background()
	if err = s.ProgressWithReceipt(ctx, 1, core.MovieKey(1), time.Minute, 10*time.Minute, "playing", receipt); err != nil {
		t.Fatal(err)
	}
}
