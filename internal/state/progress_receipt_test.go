package state

import (
	"context"
	"database/sql"
	"errors"
	"github.com/braedonsaunders/ether/internal/core"
	"testing"
	"time"
)

func TestProgressReceiptCommitsAndRollsBackWithWatchState(t *testing.T) {
	s, _ := newTestState(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`CREATE TABLE test_receipt(value INTEGER)`); err != nil {
		t.Fatal(err)
	}
	rejected := errors.New("receipt rejected")
	for _, fail := range []bool{true, false} {
		err := s.ProgressWithReceipt(ctx, 1, core.MovieKey(1), time.Minute, 10*time.Minute, "playing", func(tx *sql.Tx) error {
			if _, err := tx.Exec(`INSERT INTO test_receipt VALUES(1)`); err != nil {
				return err
			}
			if fail {
				return rejected
			}
			return nil
		})
		if fail && !errors.Is(err, rejected) || !fail && err != nil {
			t.Fatal(err)
		}
		var receipts, history int
		s.db.QueryRow(`SELECT COUNT(*) FROM test_receipt`).Scan(&receipts)
		s.db.QueryRow(`SELECT COUNT(*) FROM state_history`).Scan(&history)
		ws, err := s.Get(ctx, 1, core.MovieKey(1))
		if err != nil {
			t.Fatal(err)
		}
		if fail && (receipts != 0 || history != 0 || ws.ViewOffset != 0) {
			t.Fatalf("rollback: %d %d %+v", receipts, history, ws)
		}
		if !fail && (receipts != 1 || history != 1 || ws.ViewOffset != time.Minute) {
			t.Fatalf("commit: %d %d %+v", receipts, history, ws)
		}
	}
}
