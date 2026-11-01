package capability_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

func TestClaimsAndActiveRenewalPreserveOverboundState(t *testing.T) {
	db, _, _ := fixture(t)
	ctx := context.Background()
	h := capability.Header{HeaderScope: capability.HeaderScope{Namespace: "renewal", ViewerID: 1, Surface: "silo", DeviceID: "tv"}, Digest: entry(1900).Digest, ExpiresMillis: 10000}
	other := h
	other.Digest = entry(1901).Digest
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	// Model a preserved migration set above the new admission quota.
	if err = capability.BindHeadersInTx(ctx, tx, []capability.Header{h, other}); err != nil {
		t.Fatal(err)
	}
	if claimed, e := capability.ClaimHeaderInTx(ctx, db, tx, h, "impression", 1); e != nil || !claimed {
		t.Fatalf("claim %v %v", claimed, e)
	}
	h.ExpiresMillis = 20000
	if renewed, e := capability.RenewHeaderInTx(ctx, db, tx, h, 1); e != nil || !renewed {
		t.Fatalf("active renewal %v %v", renewed, e)
	}
	if claimed, e := capability.ClaimHeaderInTx(ctx, db, tx, h, "impression", 1); e != nil || claimed {
		t.Fatalf("claim reset by renewal %v %v", claimed, e)
	}
	fresh := h
	fresh.Digest = entry(1902).Digest
	var capacity *capability.CapacityError
	if e := capability.AdmitHeadersInTx(ctx, db, tx, []capability.Header{fresh}, capability.Limits{PerScope: 1, PerShard: 1}); !errors.As(e, &capacity) {
		t.Fatalf("fresh overbound admission %v", e)
	}
	foreign := h
	foreign.DeviceID = "foreign"
	if renewed, e := capability.RenewHeaderInTx(ctx, db, tx, foreign, 1); e != nil || renewed {
		t.Fatalf("foreign renewal %v %v", renewed, e)
	}
	h.ExpiresMillis = 30000
	if renewed, e := capability.RenewHeaderInTx(ctx, db, tx, h, 20000); e != nil || renewed {
		t.Fatalf("expired renewal %v %v", renewed, e)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestHeaderScopeLockOrdersAdmission(t *testing.T) {
	db, _, _ := fixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	scope := capability.HeaderScope{Namespace: "proof", ViewerID: 1 + capability.HeaderShardCount, Surface: "plex"}
	if err := capability.LockHeaderScopeInTx(ctx, db, tx, scope); err != nil {
		t.Fatal(err)
	}
	if err := capability.LockHeaderScopeInTx(ctx, db, tx, capability.HeaderScope{}); !errors.Is(err, capability.ErrInvalidHeader) {
		t.Fatalf("invalid=%v", err)
	}
	done := make(chan error, 1)
	started := make(chan struct{})
	go func() {
		close(started)
		other, err := db.BeginTx(ctx, nil)
		if err != nil {
			done <- err
			return
		}
		defer other.Rollback()
		h := capability.Header{HeaderScope: capability.HeaderScope{Namespace: "proof", ViewerID: 1, Surface: "silo"}, Digest: entry(1210).Digest, ExpiresMillis: 9999999999999}
		if err := capability.AdmitHeadersInTx(ctx, db, other, []capability.Header{h}, capability.Limits{PerScope: 2, PerShard: 2}); err != nil {
			done <- err
			return
		}
		done <- other.Commit()
	}()
	<-started
	select {
	case err := <-done:
		t.Fatalf("admission passed held shard lock: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
