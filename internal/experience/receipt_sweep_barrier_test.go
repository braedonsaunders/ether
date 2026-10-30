package experience

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestReceiptSweepRechecksRenewedLegacyRow(t *testing.T) {
	if os.Getenv(store.TestPostgresEnv) == "" {
		t.Skip("requires PostgreSQL row wait observation")
	}
	db := store.OpenTest(t)
	now := time.Unix(1700000000, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	digest := strings.Repeat("a", 64)
	if _, err := db.ExecContext(ctx, `INSERT INTO experience_receipts(hash,viewer_id,device_id,protocol,expires,kind,subject,purpose,rank) VALUES(?,?,?,?,?,?,?,?,?)`, digest, 7, "tv", "jellyfin", now.Unix()-1, "title", "old", "", 0); err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE experience_receipts SET expires=? WHERE hash=?`, now.Add(time.Hour).Unix(), digest); err != nil {
		t.Fatal(err)
	}
	var holder int
	if err = tx.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&holder); err != nil {
		t.Fatal(err)
	}
	opts := relayOptions(db, &now, 10, false, nil)
	entered := make(chan int, 1)
	opts.beforeSweep = func(cleanup *sql.Tx) {
		var pid int
		if e := cleanup.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); e != nil {
			entered <- 0
			return
		}
		entered <- pid
	}
	svc, err := NewReceipts(opts)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		_, e := svc.Issue(ctx, &core.User{ID: 7}, core.Device{ID: "tv"}, "jellyfin", ReceiptSubject{Kind: "title", ID: "new"})
		done <- e
	}()
	defer func() {
		cancel()
		_ = tx.Rollback()
		select {
		case <-finished:
		case <-time.After(2 * time.Second):
			t.Error("worker did not exit")
		}
	}()
	var cleanup int
	select {
	case cleanup = <-entered:
		if cleanup == 0 {
			t.Fatal("cleanup pid unavailable")
		}
	case <-ctx.Done():
		t.Fatal("cleanup not started")
	}
	timer := time.NewTicker(50 * time.Millisecond)
	defer timer.Stop()
	observed := false
	for !observed {
		select {
		case e := <-done:
			t.Fatalf("finished before wait: %v", e)
		case <-ctx.Done():
			t.Fatal("row wait not observed")
		case <-timer.C:
			if err = db.QueryRowContext(ctx, `SELECT ? = ANY(pg_blocking_pids(?))`, holder, cleanup).Scan(&observed); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("cleanup timeout")
	}
	var n int
	if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM experience_receipts WHERE hash=? AND expires>?`, digest, now.Unix()).Scan(&n); err != nil || n != 1 {
		t.Fatalf("renewed=%d err=%v", n, err)
	}
}
