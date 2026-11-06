package clusterplayback

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/nodewire"
	"github.com/braedonsaunders/ether/internal/store"
)

// Runs within the real source/controller mTLS fixture, before any placement.
func testPreparationReceiptReplay(t *testing.T, ctx context.Context, front, source *Runtime, owner, controller nodewire.Identity, endpoints map[nodewire.Identity]string) {
	t.Helper()
	request := PrepareRequest{Operation: "receipt-before-allocation", Viewer: 9, Key: core.MovieKey(1), Caps: core.ClientCaps{ClientID: "receipt-source-client"}, Protocol: "hls"}
	prepared, err := front.PrepareSource(ctx, owner, request)
	if err != nil {
		t.Fatal(err)
	}
	duplicate := request
	duplicate.Operation = "separate-operation-same-native-source"
	if _, err := source.prepare(ctx, nodewire.Identity{Cluster: controller.Cluster, NodeID: "other-controller", Boot: "other-controller-boot"}, &duplicate); err == nil {
		t.Fatal("different controller operation shared native source ownership")
	}
	_, end, err := source.beginPreparationUse(controller, request.Operation)
	if err != nil {
		t.Fatal(err)
	}
	if retired, err := front.RetirePreparedSource(ctx, owner, request.Operation); err == nil && retired {
		t.Fatal("source callback was not joined")
	}
	end()
	// A failed durable write must retain the exact operation, even after the
	// actual source backend is nil-terminal. Drain cannot acknowledge zero.
	if store.DialectOf(source.o.DB) == store.SQLite {
		if _, err = source.o.DB.ExecContext(ctx, `CREATE TRIGGER reject_preparation_receipt BEFORE INSERT ON clusterplayback_preparation_receipts BEGIN SELECT RAISE(ABORT,'fixture preparation write'); END`); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 20; i++ {
			_, _ = front.RetirePreparedSource(ctx, owner, request.Operation)
			source.Cleanup()
			if details, err := source.PendingDetails(ctx); err != nil && details.PreparationReceipts > 0 {
				break
			}
			if i == 19 {
				t.Fatal("failed receipt write did not block source drain")
			}
			time.Sleep(time.Millisecond)
		}
		if proven, err := front.PreparedSourceRetired(ctx, owner, request.Operation); err != nil || proven {
			t.Fatal("failed write became proof", err)
		}
		if _, err = source.o.DB.ExecContext(ctx, `DROP TRIGGER reject_preparation_receipt`); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 30; i++ {
		_, _ = front.RetirePreparedSource(ctx, owner, request.Operation)
		source.Cleanup()
		_, _ = source.PendingDetails(ctx)
		if proven, err := front.PreparedSourceRetired(ctx, owner, request.Operation); err == nil && proven {
			break
		}
		if i == 29 {
			t.Fatal("exact preparation receipt was not published")
		}
		time.Sleep(time.Millisecond)
	}
	receipt, err := front.LookupPreparationReceipt(ctx, owner, request.Operation)
	raw, _ := json.Marshal(request)
	if err != nil || receipt.Kind != "source-retired" || receipt.RequestHash != sha256.Sum256(raw) || receipt.PlayID != prepared.PlayID || receipt.Revision != prepared.Revision || receipt.Size != 10 || receipt.LifetimeID == "" {
		t.Fatal("receipt lost original source/lifetime identity", receipt, err)
	}
	// Simulate a committed write whose acknowledgement was lost locally. The
	// immutable exact receipt reconciles with the original source unreachable.
	source.mu.Lock()
	entry := source.preparations[controller.NodeID+"/"+controller.Boot+"/"+request.Operation]
	entry.receipt = false
	source.mu.Unlock()
	if err := source.publishPreparationReceipt(ctx, entry); err != nil {
		t.Fatal("lost acknowledgement reconciliation", err)
	}
	endpoint := endpoints[owner]
	delete(endpoints, owner)
	defer func() { endpoints[owner] = endpoint }()
	if retired, err := front.RetirePreparedSource(ctx, owner, request.Operation); err != nil || !retired {
		t.Fatal("source boot replacement lost exact receipt", err)
	}
	wrongBoot := owner
	wrongBoot.Boot = "different-source-boot"
	if proven, err := front.PreparedSourceRetired(ctx, wrongBoot, request.Operation); err != nil || proven {
		t.Fatal("foreign source boot inherited receipt", err)
	}
	wrongController := &Runtime{o: front.o}
	wrongController.o.Self.Boot = "different-controller-boot"
	if proven, err := wrongController.PreparedSourceRetired(ctx, owner, request.Operation); err != nil || proven {
		t.Fatal("foreign controller inherited receipt", err)
	}
	if proven, err := front.PreparedSourceRetired(ctx, owner, "unknown-operation"); err != nil || proven {
		t.Fatal("absence became terminal proof", err)
	}
	endpoints[owner] = endpoint
	if _, err := front.PrepareSource(ctx, owner, request); err == nil {
		t.Fatal("terminal receipt allowed delayed preparation replay")
	}
	// A real rejected viewer is an authenticated no-acquisition barrier, rather
	// than an inferred missing operation. It too survives source boot rollover.
	rejected := request
	rejected.Operation = "receipt-rejected-viewer"
	rejected.Viewer = 19
	if _, err := front.PrepareSource(ctx, owner, rejected); err == nil {
		t.Fatal("invalid viewer accepted")
	}
	if _, err := source.PendingDetails(ctx); err != nil {
		t.Fatal(err)
	}
	rejection, err := front.LookupPreparationReceipt(ctx, owner, rejected.Operation)
	if err != nil || rejection.Kind != "no-source-barrier" || rejection.PlayID != "" || rejection.RequestHash == ([32]byte{}) {
		t.Fatal("missing authentic pre-acquire barrier", err)
	}
}
