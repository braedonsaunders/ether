package experience

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
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

func TestRowViewPolicyPreflightRetry(t *testing.T) {
	for _, denial := range []bool{false, true} {
		t.Run(map[bool]string{false: "unrelated change", true: "real denial"}[denial], func(t *testing.T) {
			db := store.OpenTest(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			now := time.Unix(1700000000, 0)
			opts := relayOptions(db, &now, 10, true, nil)
			changed, denied := false, false
			opts.Allowed = func(context.Context, *core.User, core.Device, core.Protocol, ReceiptSubject) bool {
				if !changed {
					changed = true
					done := make(chan error, 1)
					go func() { done <- viewerpolicy.Bump(ctx, db) }()
					if err := <-done; err != nil {
						t.Fatal(err)
					}
					denied = denial
				}
				return true
			}
			receipts, err := NewReceipts(opts)
			if err != nil {
				t.Fatal(err)
			}
			service, err := NewRowViewService(RowViewOptions{DB: db, TTL: time.Minute, Now: func() time.Time { return now }, Active: opts.Active, Receipts: receipts,
				Build: func(_ context.Context, r frontend.RowViewRequest) ([]RowViewSnapshot, error) {
					return []RowViewSnapshot{{Row: frontend.Row{ID: "r", Kind: "taste", Purpose: frontend.RowRecommendation}, Placement: r.Placement, Entries: []core.NeutralEntryRef{{Kind: "title", ID: core.MovieKey(1).String()}}}}, nil
				},
				ValidRow: func(context.Context, *core.User, core.Device, core.Protocol, RowViewSnapshot) bool { return !denied },
				ResolveEntry: func(context.Context, *core.User, core.Device, core.Protocol, core.NeutralEntryRef) (frontend.RowEntry, error) {
					return frontend.RowEntry{Kind: frontend.RowEntryTitle, Title: &core.Title{Key: core.MovieKey(1)}}, nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			viewer, device := &core.User{ID: 7}, core.Device{ID: "tv"}
			views, err := service.ListRowViews(ctx, frontend.RowViewRequest{Viewer: viewer, Device: device, Protocol: "jellyfin", Placement: frontend.PlacementHome})
			if err != nil || len(views) != 1 {
				t.Fatalf("views=%v err=%v", views, err)
			}
			page, err := service.ResolveRowView(ctx, frontend.RowViewPageRequest{Viewer: viewer, Device: device, Protocol: "jellyfin", ID: views[0].ID, Limit: 1})
			expected := 2
			if denial {
				expected = 0
				if !errors.Is(err, ErrRowViewStale) {
					t.Fatalf("denial=%v", err)
				}
			} else if err != nil || page.ImpressionHandle == "" || len(page.Entries) != 1 {
				t.Fatalf("page=%+v err=%v", page, err)
			}
			for _, table := range []string{"experience_receipts", "capability_headers"} {
				var n int
				if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil || n != expected {
					t.Fatalf("%s=%d %v", table, n, err)
				}
			}
		})
	}
}
