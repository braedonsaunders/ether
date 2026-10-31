package store

import (
	"bytes"
	"context"
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
		// The commit paid for the checkpoint only when it lasted about as long
		// as the checkpoint. A third of a short checkpoint is scheduler delay
		// while the suite is busy.
		if maxCommit > bound && maxCommit > ckptDur*3/4 {
			t.Fatalf("commit paid for the checkpoint: %s", last)
		}
		return
	}
}
