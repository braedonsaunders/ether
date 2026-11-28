package sessions

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func retireTestQueues(t *testing.T) (*PlayQueues, *sql.DB) {
	t.Helper()
	db := store.OpenTest(t)
	qs := NewPlayQueues()
	var counter int64
	qs.Configure(nil, &PlayQueuePersistence{
		DB: db,
		Counter: func(_ context.Context, _ string, n int) (int64, error) {
			counter += int64(n)
			return counter - int64(n) + 1, nil
		},
		Invalidate: func(int64) {},
		Warn:       func(string, error) {},
	})
	return qs, db
}

func countLockedDecodes(t *testing.T) *int {
	t.Helper()
	n := 0
	orig := lockedQueueClient
	lockedQueueClient = func(ctx context.Context, tx *sql.Tx, id int64) (string, error) {
		n++
		return orig(ctx, tx, id)
	}
	t.Cleanup(func() { lockedQueueClient = orig })
	return &n
}

// Creating a queue must not decode every other queue blob while it holds the
// cluster-wide counter lock.
func TestCreateSharedDecodesNoQueueBlobsUnderCounterLock(t *testing.T) {
	qs, db := retireTestQueues(t)
	now := time.Now()
	for i := 0; i < 20; i++ {
		if _, err := qs.CreateWith(1, fmt.Sprintf("client-%d", i), []core.RatingKey{core.MovieKey(1)}, now, nil); err != nil {
			t.Fatal(err)
		}
	}
	decodes := countLockedDecodes(t)
	if _, err := qs.CreateWith(1, "client-3", []core.RatingKey{core.MovieKey(2)}, now, nil); err != nil {
		t.Fatal(err)
	}
	if *decodes != 0 {
		t.Fatalf("%d queue blobs decoded under the counter lock", *decodes)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM plex_playqueues WHERE user_id=1`).Scan(&n); err != nil || n != 20 {
		t.Fatalf("client-3's old queue was not retired: %d queues, %v", n, err)
	}
}

// A queue created or taken after the pre-lock read is still judged by its
// current owner under the lock.
func TestRetireQueueClientRechecksRowsChangedSincePreRead(t *testing.T) {
	qs, db := retireTestQueues(t)
	now := time.Now()
	a, err := qs.CreateWith(1, "other", []core.RatingKey{core.MovieKey(1)}, now, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := qs.CreateWith(1, "late", []core.RatingKey{core.MovieKey(1)}, now, nil)
	if err != nil {
		t.Fatal(err)
	}
	stale := map[int64]queueClient{a.ID: {rev: a.Rev, client: "other"}} // b is missing
	// a is taken by "mine" after the read: its rev moves on.
	if _, ok := qs.Take(a.ID, 1, "mine", now); !ok {
		t.Fatal("take failed")
	}
	qs.Forget(a.ID)
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, c := range []struct {
		client string
		want   int64
	}{{"mine", a.ID}, {"late", b.ID}} {
		got, err := retireQueueClient(ctx, tx, 1, c.client, 0, stale)
		if err != nil || len(got) != 1 || got[0] != c.want {
			t.Fatalf("client %q: retired %v, %v; want [%d]", c.client, got, err, c.want)
		}
	}
}
