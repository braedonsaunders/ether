package store

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"
)

// A long read (the integrity check, a backup's VACUUM INTO) holds one SQLite
// read snapshot for its whole run. While it does, the checkpointer cannot
// copy frames past it and the WAL cannot restart, so a bulk writer running
// beside it grows the WAL without bound and every read searches a larger
// WAL index. Long reads therefore announce themselves, and bulk writers
// (WaitWALBudget) hold off until they finish and the checkpoint has drained.
// Interactive writes never wait here.
type longReads struct {
	mu      sync.Mutex
	active  map[*longRead]struct{}
	changed chan struct{} // closed and replaced whenever a long read ends
}

type longRead struct {
	label   string
	started time.Time
}

var (
	longReadsMu sync.Mutex
	longReadsBy = map[*sql.DB]*longReads{}
)

func longReadsFor(db *sql.DB) *longReads {
	longReadsMu.Lock()
	defer longReadsMu.Unlock()
	lr := longReadsBy[db]
	if lr == nil {
		lr = &longReads{active: map[*longRead]struct{}{}, changed: make(chan struct{})}
		longReadsBy[db] = lr
	}
	return lr
}

// BeginLongRead marks a read that holds one snapshot of the whole database,
// named by label in a bulk writer's wait error. The returned func ends it.
func BeginLongRead(db *sql.DB, label string) func() {
	if db == nil || DialectOf(db) == Postgres {
		return func() {}
	}
	lr := longReadsFor(db)
	r := &longRead{label: label, started: time.Now()}
	lr.mu.Lock()
	lr.active[r] = struct{}{}
	lr.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			lr.mu.Lock()
			delete(lr.active, r)
			close(lr.changed)
			lr.changed = make(chan struct{})
			lr.mu.Unlock()
		})
	}
}

// oldest is the longest-running long read, if any, and a channel closed when
// any long read ends.
func (lr *longReads) oldest() (*longRead, <-chan struct{}) {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	var first *longRead
	for r := range lr.active {
		if first == nil || r.started.Before(first.started) {
			first = r
		}
	}
	return first, lr.changed
}

// WALBlockedError is a bulk write that waited walWaitMax for the WAL to
// drain. Reader names the long read in the way when this process knows it.
type WALBlockedError struct {
	Pending int
	Waited  time.Duration
	Reader  string
}

func (e *WALBlockedError) Error() string {
	by := "a long-running reader"
	if e.Reader != "" {
		by = e.Reader
	}
	return fmt.Sprintf("sqlite: bulk writes paused for %s: the WAL checkpoint is blocked by %s (%d pending frames)", e.Waited.Round(time.Second), by, e.Pending)
}

// walWaitMax bounds how long a bulk writer waits for the checkpoint; past
// it the writer reports WALBlockedError so a stuck reader is visible.
var walWaitMax = 15 * time.Minute

// walWaitPoll spaces the WAL re-checks while a reader we do not know of
// (another process, a long request) holds the checkpoint back.
const walWaitPoll = time.Second

// WaitWALBudget is the bulk writers' back-pressure: it returns once no long
// read is running and the WAL is within journalSizeLimit, waiting (and
// checkpointing) as needed. It is not a failure to wait; it returns
// WALBlockedError only after walWaitMax, or ctx's error when cancelled.
// PostgreSQL returns at once.
func WaitWALBudget(ctx context.Context, db *sql.DB) error {
	if db == nil || DialectOf(db) == Postgres {
		return nil
	}
	lr := longReadsFor(db)
	start := time.Now()
	for {
		reader, ended := lr.oldest()
		pending := 0
		if reader == nil {
			over, n, err := walOverBudget(ctx, db, journalSizeLimit)
			if err != nil || !over {
				return err
			}
			pending = n
		}
		waited := time.Since(start)
		if waited >= walWaitMax {
			e := &WALBlockedError{Pending: pending, Waited: waited}
			if reader != nil {
				e.Reader = reader.label
			}
			return e
		}
		t := time.NewTimer(min(walWaitPoll, walWaitMax-waited))
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-ended:
		case <-t.C:
		}
		t.Stop()
	}
}
