package capability_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/braedonsaunders/ether/internal/handles/capability"
)

func TestAdmissionRollbackCapacityAndScopedConsume(t *testing.T) {
	db, _, _ := fixture(t)
	ctx := context.Background()
	h := capability.Header{HeaderScope: capability.HeaderScope{Namespace: "proof", ViewerID: 1, Surface: "silo"}, Digest: entry(910).Digest, ExpiresMillis: 9999999999999}
	limits := capability.Limits{PerScope: 1, PerShard: 1}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = capability.AdmitHeadersInTx(ctx, db, tx, []capability.Header{h}, limits); err != nil {
		t.Fatal(err)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	tx, err = db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = capability.AdmitHeadersInTx(ctx, db, tx, []capability.Header{h}, limits); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	tx, err = db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	other := h
	other.Digest = entry(911).Digest
	var capacity *capability.CapacityError
	if err = capability.AdmitHeadersInTx(ctx, db, tx, []capability.Header{other}, limits); !errors.As(err, &capacity) {
		t.Fatalf("capacity: %v", err)
	}
	tx.Rollback()
	tx, err = db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	foreign := h
	foreign.DeviceID = "foreign"
	if ok, e := capability.ConsumeHeaderInTx(ctx, db, tx, foreign, 1); e != nil || ok {
		t.Fatalf("foreign consume: %v %v", ok, e)
	}
	if ok, e := capability.ConsumeHeaderInTx(ctx, db, tx, h, 1); e != nil || !ok {
		t.Fatalf("consume: %v %v", ok, e)
	}
	if ok, e := capability.ConsumeHeaderInTx(ctx, db, tx, h, 1); e != nil || ok {
		t.Fatalf("replay: %v %v", ok, e)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestAdmissionConcurrentLastSlot(t *testing.T) {
	db, _, _ := fixture(t)
	ctx := context.Background()
	var won atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Error(err)
				return
			}
			defer tx.Rollback()
			h := capability.Header{HeaderScope: capability.HeaderScope{Namespace: "proof", ViewerID: 1, Surface: "silo"}, Digest: entry(1000 + i).Digest, ExpiresMillis: 9999999999999}
			err = capability.AdmitHeadersInTx(ctx, db, tx, []capability.Header{h}, capability.Limits{PerScope: 1, PerShard: 1})
			var capacity *capability.CapacityError
			if errors.As(err, &capacity) {
				return
			}
			if err != nil {
				t.Error(err)
				return
			}
			if err = tx.Commit(); err != nil {
				t.Error(err)
				return
			}
			won.Add(1)
		}(i)
	}
	wg.Wait()
	if won.Load() != 1 {
		t.Fatalf("admitted %d", won.Load())
	}
}
