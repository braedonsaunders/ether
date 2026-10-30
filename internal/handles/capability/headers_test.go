package capability_test

import (
	"context"
	"errors"
	"testing"

	"github.com/braedonsaunders/ether/internal/handles/capability"
)

func TestHeadersTransactionalScopeAndCollision(t *testing.T) {
	db, _, _ := fixture(t)
	ctx := context.Background()
	scope := capability.HeaderScope{Namespace: "experience.receipt", ViewerID: 1, Surface: "silo", DeviceID: "tv"}
	h := capability.Header{HeaderScope: scope, Digest: entry(77).Digest, ExpiresMillis: 123456789}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = capability.BindHeadersInTx(ctx, tx, []capability.Header{h}); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	tx, err = db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	foreign := scope
	foreign.DeviceID = "other"
	if err = capability.DropHeadersInTx(ctx, tx, foreign, []string{h.Digest}); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var n int
	if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM capability_headers WHERE namespace=? AND digest=?`, scope.Namespace, h.Digest).Scan(&n); err != nil || n != 1 {
		t.Fatalf("foreign drop: %d %v", n, err)
	}
	tx, err = db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	other := h
	other.Digest = entry(78).Digest
	if err = capability.BindHeadersInTx(ctx, tx, []capability.Header{other, h}); err == nil {
		t.Fatal("duplicate accepted")
	}
	_ = tx.Rollback()
	if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM capability_headers WHERE namespace=? AND digest=?`, scope.Namespace, other.Digest).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rollback: %d %v", n, err)
	}
	tx, err = db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = capability.DropHeadersInTx(ctx, tx, scope, []string{h.Digest, other.Digest}); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM capability_headers WHERE namespace=? AND digest=?`, scope.Namespace, h.Digest).Scan(&n); err != nil || n != 0 {
		t.Fatalf("own drop: %d %v", n, err)
	}
}

func TestHeadersRejectInvalidAndOversized(t *testing.T) {
	db, _, _ := fixture(t)
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	scope := capability.HeaderScope{Namespace: "ticket", ViewerID: 1, Surface: "silo", DeviceID: "tv"}
	h := capability.Header{HeaderScope: scope, Digest: entry(1).Digest, ExpiresMillis: 1}
	if err := capability.BindHeadersInTx(ctx, tx, []capability.Header{h}); !errors.Is(err, capability.ErrInvalidHeader) {
		t.Fatalf("ticket namespace: %v", err)
	}
	scope.Namespace = "experience.receipt"
	h.HeaderScope = scope
	batch := make([]capability.Header, capability.HeaderBatchMax+1)
	for i := range batch {
		batch[i] = h
	}
	if err := capability.BindHeadersInTx(ctx, tx, batch); !errors.Is(err, capability.ErrInvalidHeader) {
		t.Fatalf("batch: %v", err)
	}
}
