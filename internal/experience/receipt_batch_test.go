package experience

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/handles/capability"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestReceiptBatchAtomicOnLaterBindFailure(t *testing.T) {
	db := store.OpenTest(t)
	now := time.Unix(1700000000, 0)
	opts := relayOptions(db, &now, 100, true, nil)
	calls := 0
	failure := errors.New("later bind")
	opts.bindHeaders = func(ctx context.Context, tx *sql.Tx, hs []capability.Header) error {
		calls++
		if calls == 2 {
			return failure
		}
		return capability.BindHeadersInTx(ctx, tx, hs)
	}
	s, err := NewReceipts(opts)
	if err != nil {
		t.Fatal(err)
	}
	subjects := make([]ReceiptSubject, 41)
	for i := range subjects {
		subjects[i] = ReceiptSubject{Kind: "title", ID: fmt.Sprint(i)}
	}
	tokens, err := s.IssueBatch(context.Background(), &core.User{ID: 1}, core.Device{ID: "tv"}, "jellyfin", subjects)
	if !errors.Is(err, failure) || tokens != nil || calls != 2 {
		t.Fatalf("tokens=%v err=%v calls=%d", tokens, err, calls)
	}
	for _, table := range []string{"experience_receipts", "capability_headers"} {
		var n int
		if err = db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s count=%d err=%v", table, n, err)
		}
	}
}

func TestReceiptBatchEnforcesQuotaBeforeAnyIssue(t *testing.T) {
	db := store.OpenTest(t)
	now := time.Unix(1700000000, 0)
	opts := relayOptions(db, &now, 1, true, nil)
	s, err := NewReceipts(opts)
	if err != nil {
		t.Fatal(err)
	}
	subjects := []ReceiptSubject{{Kind: "title", ID: "1"}, {Kind: "title", ID: "2"}}
	if tokens, err := s.IssueBatch(context.Background(), &core.User{ID: 1}, core.Device{ID: "tv"}, "jellyfin", subjects); err == nil || tokens != nil {
		t.Fatalf("tokens=%v err=%v", tokens, err)
	}
	var n int
	if err = db.QueryRow(`SELECT COUNT(*) FROM experience_receipts`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("count=%d err=%v", n, err)
	}
}
