package sessions

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/state"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestSampleReceiptBoundsRetainActiveAndOrphans(t *testing.T) {
	db := store.OpenTest(t)
	ctx := context.Background()
	progress := state.New(db, state.Options{})
	b := &Broker{deps: BrokerDeps{DB: db}, sampleLimit: 2}
	sample := frontend.ProgressSample{InstallationID: "11111111-1111-4111-8111-111111111111", Sequence: 1, PositionSeconds: 60}
	write := func(id string) error {
		s := &brokerSession{public: frontend.Session{ID: id, Protocol: "silo"}}
		return progress.ProgressWithReceipt(ctx, 1, core.MovieKey(1), time.Minute, 10*time.Minute, "playing", b.sampleReceipt(ctx, s, &sampleReport{sample: sample}))
	}
	// An orphan from a crashed runtime is conservatively retained and charged.
	if err := write("crash-orphan"); err != nil {
		t.Fatal(err)
	}
	if err := write("active"); err != nil {
		t.Fatal(err)
	}
	if err := write("overflow"); !errors.Is(err, core.ErrNotAvailable) {
		t.Fatalf("overflow %v", err)
	}
	if err := write("active"); !errors.Is(err, errSampleNotApplied) {
		t.Fatalf("active protection lost %v", err)
	}
	// A new broker instance cannot sweep the prior runtime's receipts.
	restarted := &Broker{deps: BrokerDeps{DB: db}, sampleLimit: 2}
	if err := progress.ProgressWithReceipt(ctx, 1, core.MovieKey(1), time.Minute, 10*time.Minute, "playing", restarted.sampleReceipt(ctx, &brokerSession{public: frontend.Session{ID: "restart"}}, &sampleReport{sample: sample})); !errors.Is(err, core.ErrNotAvailable) {
		t.Fatalf("restart bypassed cap %v", err)
	}
	b.cleanupSampleReceipt(ctx, &brokerSession{public: frontend.Session{ID: "active", Protocol: "silo"}})
	for n := 0; n < 50; n++ {
		id := fmt.Sprint("churn-", n)
		if err := write(id); err != nil {
			t.Fatal(err)
		}
		b.cleanupSampleReceipt(ctx, &brokerSession{public: frontend.Session{ID: id, Protocol: "silo"}})
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sessions_sample_receipts`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("churn %d %v", count, err)
	}
}

func TestSampleReceiptCleanupRetainsUncertainClusterOwner(t *testing.T) {
	db := store.OpenTest(t)
	ctx := context.Background()
	cs := clusterStore{db: db, node: "a"}
	session := frontend.Session{ID: "active", Protocol: "silo", ViewerID: 1, Device: core.Device{ID: "tv"}, Item: core.MovieKey(1), StartedAt: time.Now()}
	if err := cs.put(ctx, session, "", core.Viewer{ID: 1}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sessions_sample_receipts VALUES('active','installation','00000000000000000001','sample')`); err != nil {
		t.Fatal(err)
	}
	b := &Broker{deps: BrokerDeps{DB: db}, cluster: &brokerCluster{store: cs}}
	// Local removal with failed durable retirement must retain the active receipt.
	b.cleanupSampleReceipt(ctx, &brokerSession{public: session})
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sessions_sample_receipts`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("active erased %d %v", count, err)
	}
	if err := cs.remove(ctx, session.ID); err != nil {
		t.Fatal(err)
	}
	b.cleanupSampleReceipt(ctx, &brokerSession{public: session})
	if err := db.QueryRow(`SELECT COUNT(*) FROM sessions_sample_receipts`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("retired retained %d %v", count, err)
	}
}
