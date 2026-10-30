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

func TestHeadersPruneIsBoundedAndNamespaceScoped(t *testing.T) {
	db, _, _ := fixture(t)
	ctx := context.Background()
	scope := capability.HeaderScope{Namespace: "experience.receipt", ViewerID: 1, Surface: "silo", DeviceID: "tv"}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	a := capability.Header{HeaderScope: scope, Digest: entry(21).Digest, ExpiresMillis: 10}
	b := capability.Header{HeaderScope: scope, Digest: entry(22).Digest, ExpiresMillis: 10}
	live := capability.Header{HeaderScope: scope, Digest: entry(23).Digest, ExpiresMillis: 11}
	foreign := a
	foreign.Namespace = "image.grant"
	if err = capability.BindHeadersInTx(ctx, tx, []capability.Header{a, b, live, foreign}); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	tx, err = db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	n, err := capability.PruneHeadersInTx(ctx, tx, scope.Namespace, 10, 1)
	if err != nil || n != 1 {
		t.Fatalf("prune=%d err=%v", n, err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM capability_headers WHERE namespace=?`, scope.Namespace).Scan(&count); err != nil || count != 2 {
		t.Fatalf("remaining=%d err=%v", count, err)
	}
	if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM capability_headers WHERE namespace=?`, foreign.Namespace).Scan(&count); err != nil || count != 1 {
		t.Fatalf("foreign=%d err=%v", count, err)
	}
}

func TestHeadersMaximumBatchAtomic(t *testing.T) {
	db, _, _ := fixture(t)
	ctx := context.Background()
	scope := capability.HeaderScope{Namespace: "experience.receipt", ViewerID: 1, Surface: "silo", DeviceID: "tv"}
	batch := make([]capability.Header, capability.HeaderBatchMax)
	for i := range batch {
		batch[i] = capability.Header{HeaderScope: scope, Digest: entry(i + 100).Digest, ExpiresMillis: 100}
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = capability.BindHeadersInTx(ctx, tx, batch); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var n int
	if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM capability_headers WHERE namespace=?`, scope.Namespace).Scan(&n); err != nil || n != len(batch) {
		t.Fatalf("batch count=%d err=%v", n, err)
	}
	tx, err = db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	replacement := batch[0]
	replacement.Digest = entry(999).Digest
	if err = capability.BindHeadersInTx(ctx, tx, []capability.Header{replacement, batch[1]}); err == nil {
		t.Fatal("collision succeeded")
	}
	_ = tx.Rollback()
	if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM capability_headers WHERE namespace=? AND digest=?`, scope.Namespace, replacement.Digest).Scan(&n); err != nil || n != 0 {
		t.Fatalf("partial batch=%d err=%v", n, err)
	}
}
