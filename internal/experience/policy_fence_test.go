package experience

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/viewerpolicy"
)

func TestReceiptConcurrentPolicyChangeRetriesBoundedly(t *testing.T) {
	for _, churn := range []bool{false, true} {
		t.Run(map[bool]string{false: "one change", true: "continuous churn"}[churn], func(t *testing.T) {
			db := store.OpenTest(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			now := time.Unix(1700000000, 0)
			opts := relayOptions(db, &now, 10, true, nil)
			calls := 0
			opts.Allowed = func(context.Context, *core.User, core.Device, core.Protocol, ReceiptSubject) bool {
				calls++
				if calls == 1 || churn {
					done := make(chan error, 1)
					go func() {
						_, err := db.ExecContext(ctx, `UPDATE viewer_policy_epoch SET generation=generation+1 WHERE id=1`)
						done <- err
					}()
					if err := <-done; err != nil {
						t.Fatal(err)
					}
				}
				return true
			}
			svc, err := NewReceipts(opts)
			if err != nil {
				t.Fatal(err)
			}
			tokens, err := svc.IssueBatch(ctx, &core.User{ID: 7}, core.Device{ID: "tv"}, "jellyfin", []ReceiptSubject{{Kind: "title", ID: "one"}})
			expected := 1
			if churn {
				expected = 0
				if !errors.Is(err, viewerpolicy.ErrChanged) {
					t.Fatalf("churn err=%v", err)
				}
			} else if err != nil || len(tokens) != 1 {
				t.Fatalf("tokens=%v err=%v", tokens, err)
			}
			if calls != 2 {
				t.Fatalf("preflights=%d", calls)
			}
			for _, table := range []string{"experience_receipts", "capability_headers"} {
				var n int
				if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil || n != expected {
					t.Fatalf("%s=%d err=%v", table, n, err)
				}
			}
		})
	}
}
