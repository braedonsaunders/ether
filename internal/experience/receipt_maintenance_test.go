package experience

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/handles/capability"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestReceiptMaintenanceRecoversAbandonedDeviceQuotaAndPreservesLive(t *testing.T) {
	db := store.OpenTest(t)
	ctx := context.Background()
	now := time.Unix(1700000000, 0)
	calls := 0
	svc, err := NewReceipts(relayOptions(db, &now, 100, true, &calls))
	if err != nil {
		t.Fatal(err)
	}
	viewer := &core.User{ID: 7}
	abandoned := core.Device{ID: "abandoned"}
	subjects := make([]ReceiptSubject, capability.HeaderBatchMax+1)
	for i := range subjects {
		subjects[i] = ReceiptSubject{Kind: "title", ID: "expired"}
	}
	tokens, err := svc.IssueBatch(ctx, viewer, abandoned, "jellyfin", subjects)
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.Report(ctx, frontend.RowInteraction{Viewer: viewer, Device: abandoned, Protocol: "jellyfin", Handle: tokens[0], Kind: frontend.RowImpression}); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Issue(ctx, &core.User{ID: 8}, abandoned, "jellyfin", ReceiptSubject{Kind: "title", ID: "other shard"}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(30 * time.Second)
	liveDevice := core.Device{ID: "live"}
	live, err := svc.Issue(ctx, viewer, liveDevice, "jellyfin", ReceiptSubject{Kind: "title", ID: "live"})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(30 * time.Second)
	h := capability.Header{HeaderScope: capability.HeaderScope{Namespace: ReceiptCapabilityNamespace, ViewerID: 7, Surface: "jellyfin", DeviceID: "fresh"}, Digest: strings.Repeat("f", 64), ExpiresMillis: now.Add(time.Minute).UnixMilli()}
	admit := func() error {
		tx, e := db.BeginTx(ctx, nil)
		if e != nil {
			return e
		}
		defer tx.Rollback()
		e = capability.AdmitHeadersInTx(ctx, db, tx, []capability.Header{h}, capability.Limits{PerScope: 42, PerShard: 42})
		if e != nil {
			return e
		}
		_, e = tx.ExecContext(ctx, `INSERT INTO experience_receipts(hash,viewer_id,device_id,protocol,expires,kind,subject,purpose,rank) VALUES(?,?,?,?,?,?,?,?,?)`, h.Digest, 7, "fresh", "jellyfin", h.ExpiresMillis/1000, "title", "fresh", "", 0)
		if e != nil {
			return e
		}
		return tx.Commit()
	}
	var capacity *capability.CapacityError
	if e := admit(); !errors.As(e, &capacity) {
		t.Fatalf("abandoned device did not retain quota: %v", e)
	}
	if n, e := PruneReceiptShard(ctx, db, now, 7); e != nil || n != capability.HeaderBatchMax {
		t.Fatalf("bounded maintenance %d %v", n, e)
	}
	if e := admit(); e != nil {
		t.Fatalf("quota not recovered: %v", e)
	}
	if n, e := PruneReceiptShard(ctx, db, now, 7); e != nil || n != 1 {
		t.Fatalf("remaining expiry %d %v", n, e)
	}
	if n, e := PruneReceiptShard(ctx, db, now, 7); e != nil || n != 0 {
		t.Fatalf("live rows touched %d %v", n, e)
	}
	var remaining int
	if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM experience_receipts WHERE viewer_id=7 AND device_id='abandoned'`).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("abandoned payload=%d %v", remaining, err)
	}
	if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM capability_claims WHERE namespace=?`, ReceiptCapabilityNamespace).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("abandoned claims=%d %v", remaining, err)
	}
	if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM experience_receipts WHERE viewer_id=8`).Scan(&remaining); err != nil || remaining != 1 {
		t.Fatalf("foreign shard=%d %v", remaining, err)
	}
	if err = svc.Report(ctx, frontend.RowInteraction{Viewer: viewer, Device: liveDevice, Protocol: "jellyfin", Handle: live, Kind: frontend.RowImpression}); err != nil || calls != 2 {
		t.Fatalf("live report calls=%d %v", calls, err)
	}
}

func TestReceiptMaintenanceConcurrentIssue(t *testing.T) {
	db := store.OpenTest(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	now := time.Unix(1700000000, 0)
	svc, err := NewReceipts(relayOptions(db, &now, 100, true, nil))
	if err != nil {
		t.Fatal(err)
	}
	viewer := &core.User{ID: 7}
	device := core.Device{ID: "tv"}
	if _, err = svc.Issue(ctx, viewer, device, "jellyfin", ReceiptSubject{Kind: "title", ID: "expired"}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				if i%2 == 0 {
					if _, e := PruneReceiptShard(ctx, db, now, 7); e != nil {
						t.Error(e)
						return
					}
				} else {
					if _, e := svc.Issue(ctx, viewer, device, "jellyfin", ReceiptSubject{Kind: "title", ID: "live"}); e != nil {
						t.Error(e)
						return
					}
				}
			}
		}(i)
	}
	wg.Wait()
	var n int
	if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM experience_receipts WHERE viewer_id=7 AND expires>?`, now.Unix()).Scan(&n); err != nil || n != 20 {
		t.Fatalf("live receipts=%d %v", n, err)
	}
}

func TestPruneReceiptShardSkipsWriteTxWhenIdle(t *testing.T) {
	db := store.OpenTest(t)
	ctx := context.Background()
	statsCtx, qs := store.WithQueryStats(ctx)
	n, err := PruneReceiptShard(statsCtx, db, time.Unix(1700000000, 0), 7)
	if err != nil || n != 0 {
		t.Fatalf("PruneReceiptShard = %d, %v", n, err)
	}
	if qs.LockWait() > 0 || qs.Writes() > 0 {
		t.Fatalf("idle PruneReceiptShard acquired write lock or wrote: lockWait=%s writes=%d", qs.LockWait(), qs.Writes())
	}
}
