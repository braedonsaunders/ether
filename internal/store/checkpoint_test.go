package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// TestCommitStaysBoundedWhileCheckpointRuns writes a WAL large enough that
// the checkpoint's fsync is visible, then commits on another connection
// while that checkpoint runs. A commit that still ran the checkpoint itself
// would last about as long as the checkpoint.
func TestCommitStaysBoundedWhileCheckpointRuns(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(4)
	if _, err := db.ExecContext(ctx, `CREATE TABLE checkpoint_latency(v BLOB)`); err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("x"), 1<<20)
	const bound = 40 * time.Millisecond
	var last string
	for _, mb := range []int{16, 48, 128} {
		if _, err := db.ExecContext(ctx, `DELETE FROM checkpoint_latency`); err != nil {
			t.Fatal(err)
		}
		if err := Checkpoint(ctx, db, true); err != nil {
			t.Fatal(err)
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < mb; i++ {
			if _, err := tx.ExecContext(ctx, `INSERT INTO checkpoint_latency(v) VALUES (?)`, payload); err != nil {
				tx.Rollback()
				t.Fatal(err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}

		ckptDone := make(chan struct{})
		var ckptDur time.Duration
		var ckptErr error
		go func() {
			t0 := time.Now()
			var busy, logFrames, checkpointed int
			ckptErr = db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(PASSIVE)`).Scan(&busy, &logFrames, &checkpointed)
			ckptDur = time.Since(t0)
			close(ckptDone)
		}()

		var maxCommit time.Duration
		n := 0
		for {
			t0 := time.Now()
			if _, err := db.ExecContext(ctx, `INSERT INTO checkpoint_latency(v) VALUES (?)`, []byte{1}); err != nil {
				t.Fatal(err)
			}
			if d := time.Since(t0); d > maxCommit {
				maxCommit = d
			}
			n++
			select {
			case <-ckptDone:
				goto measured
			default:
			}
		}
	measured:
		<-ckptDone
		if ckptErr != nil {
			t.Fatal(ckptErr)
		}
		last = fmt.Sprintf("%dMB checkpoint %s, max commit %s over %d commits", mb, ckptDur, maxCommit, n)
		t.Log(last)
		if n == 0 {
			t.Fatal("no commit ran during the checkpoint")
		}
		if ckptDur < bound && mb < 128 {
			continue
		}
		// The commit paid for the checkpoint only when it lasted about as
		// long as the checkpoint. Under a busy suite a short checkpoint
		// can stall one commit for roughly three quarters of that fsync
		// (a 59ms commit beside a 77ms, 16MB checkpoint) without the
		// commit having performed it. Require nine tenths.
		if maxCommit > bound && maxCommit > ckptDur*9/10 {
			t.Fatalf("commit paid for the checkpoint: %s", last)
		}
		return
	}
}

// A reader pins old frames even though PASSIVE reports busy=0. Bulk writers
// wait for it rather than fail, report a typed error only past walWaitMax,
// and wait for an announced long read before writing at all.
func TestWALBudgetWaitsForPinnedReader(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `CREATE TABLE wal_budget(v BLOB); INSERT INTO wal_budget VALUES (zeroblob(1024))`); err != nil {
		t.Fatal(err)
	}
	reader, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if _, err := reader.ExecContext(ctx, `BEGIN`); err != nil {
		t.Fatal(err)
	}
	defer reader.ExecContext(ctx, `ROLLBACK`)
	var n int
	if err := reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM wal_budget`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO wal_budget VALUES (zeroblob(1048576))`); err != nil {
		t.Fatal(err)
	}
	if over, _, err := walOverBudget(ctx, db, 64<<10); err != nil || !over {
		t.Fatalf("pinned WAL beyond budget not seen: over=%v %v", over, err)
	}
	if _, err := reader.ExecContext(ctx, `ROLLBACK`); err != nil {
		t.Fatal(err)
	}
	if over, _, err := walOverBudget(ctx, db, 64<<10); err != nil || over {
		t.Fatalf("reader released, bulk writer still paused: over=%v %v", over, err)
	}
	// An announced long read holds bulk writers until it ends, and one that
	// outlasts walWaitMax is reported by name.
	defer func(d time.Duration) { walWaitMax = d }(walWaitMax)
	walWaitMax = 200 * time.Millisecond
	end := BeginLongRead(db, "integrity check")
	var blocked *WALBlockedError
	if err := WaitWALBudget(ctx, db); !errors.As(err, &blocked) || blocked.Reader != "integrity check" {
		t.Fatalf("long read did not hold the bulk writer: %v", err)
	}
	walWaitMax = 10 * time.Second
	go func() { time.Sleep(200 * time.Millisecond); end() }()
	started := time.Now()
	if err := WaitWALBudget(ctx, db); err != nil {
		t.Fatalf("long read ended, bulk writer still paused: %v", err)
	}
	if time.Since(started) < 150*time.Millisecond {
		t.Fatal("bulk writer did not wait for the long read")
	}
}
