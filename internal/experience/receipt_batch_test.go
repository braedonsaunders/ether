package experience

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
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

// This is a pending consumer regression: the current RowView implementation
// commits each receipt separately, so it is expected to fail until integrated.
func TestRowViewLaterReceiptFailureLeavesNoCommittedReceipts(t *testing.T) {
	db := store.OpenTest(t)
	now := time.Unix(1700000000, 0)
	opts := relayOptions(db, &now, 100, true, nil)
	failure := errors.New("second header")
	writes := 0
	opts.bindHeaders = func(ctx context.Context, tx *sql.Tx, hs []capability.Header) error {
		for _, h := range hs {
			writes++
			if writes == 2 {
				return failure
			}
			if err := capability.BindHeadersInTx(ctx, tx, []capability.Header{h}); err != nil {
				return err
			}
		}
		return nil
	}
	receipts, err := NewReceipts(opts)
	if err != nil {
		t.Fatal(err)
	}
	active := func(_ context.Context, id int64) (*core.User, error) { return &core.User{ID: id}, nil }
	rowOpts := RowViewOptions{DB: db, TTL: time.Minute, Now: func() time.Time { return now }, Active: active, Receipts: receipts,
		Build: func(_ context.Context, r frontend.RowViewRequest) ([]RowViewSnapshot, error) {
			return []RowViewSnapshot{{Row: frontend.Row{ID: "taste", Kind: "taste", Purpose: frontend.RowRecommendation}, Placement: r.Placement, Entries: []core.NeutralEntryRef{{Kind: "title", ID: core.MovieKey(1).String()}}}}, nil
		},
		ValidRow: func(context.Context, *core.User, core.Device, core.Protocol, RowViewSnapshot) bool { return true },
		ResolveEntry: func(_ context.Context, _ *core.User, _ core.Device, _ core.Protocol, _ core.NeutralEntryRef) (frontend.RowEntry, error) {
			title := core.Title{Key: core.MovieKey(1)}
			return frontend.RowEntry{Kind: frontend.RowEntryTitle, Title: &title}, nil
		},
	}
	service, err := NewRowViewService(rowOpts)
	if err != nil {
		t.Fatal(err)
	}
	viewer := &core.User{ID: 7}
	device := core.Device{ID: "tv"}
	ctx := context.Background()
	views, err := service.ListRowViews(ctx, frontend.RowViewRequest{Viewer: viewer, Device: device, Protocol: "jellyfin", Placement: frontend.PlacementHome})
	if err != nil || len(views) != 1 {
		t.Fatalf("views=%+v err=%v", views, err)
	}
	page, err := service.ResolveRowView(ctx, frontend.RowViewPageRequest{Viewer: viewer, Device: device, Protocol: "jellyfin", ID: views[0].ID, Limit: 1})
	if !errors.Is(err, failure) || page.ImpressionHandle != "" || len(page.Entries) != 0 {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	for _, table := range []string{"experience_receipts", "capability_headers"} {
		var n int
		if err = db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s count=%d err=%v", table, n, err)
		}
	}
}

func TestReceiptBatchFinalTransactionGuardRollsBack(t *testing.T) {
	db := store.OpenTest(t)
	now := time.Unix(1700000000, 0)
	opts := relayOptions(db, &now, 100, true, nil)
	s, err := NewReceipts(opts)
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("stale snapshot")
	guard := func(ctx context.Context, tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM experience_receipts`).Scan(&n); err != nil {
			return err
		}
		if n != 2 {
			t.Fatalf("guard saw %d receipts", n)
		}
		return failure
	}
	subjects := []ReceiptSubject{{Kind: "title", ID: "1"}, {Kind: "title", ID: "2"}}
	tokens, err := s.issueBatchGuarded(context.Background(), &core.User{ID: 1}, core.Device{ID: "tv"}, "jellyfin", subjects, guard)
	if !errors.Is(err, failure) || tokens != nil {
		t.Fatalf("tokens=%v err=%v", tokens, err)
	}
	for _, table := range []string{"experience_receipts", "capability_headers"} {
		var n int
		if err = db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s count=%d err=%v", table, n, err)
		}
	}
}

func TestReceiptBatchRowViewSnapshotGuard(t *testing.T) {
	db := store.OpenTest(t)
	ctx := context.Background()
	now := time.Unix(1700000000, 0)
	_, err := db.ExecContext(ctx, `INSERT INTO experience_row_views(id,scope_hash,viewer_id,device_id,protocol,snapshot,expires,payload) VALUES(?,?,?,?,?,?,?,?)`, "view", "scope", 7, "tv", "jellyfin", "snap", now.Add(time.Minute).UnixMilli(), "{}")
	if err != nil {
		t.Fatal(err)
	}
	opts := relayOptions(db, &now, 10, true, nil)
	s, err := NewReceipts(opts)
	if err != nil {
		t.Fatal(err)
	}
	guard := func(ctx context.Context, tx *sql.Tx) error {
		return guardRowViewSnapshot(ctx, tx, db, "snap", 7, "tv", "jellyfin", now.Add(time.Minute).UnixMilli())
	}
	tokens, err := s.issueBatchGuarded(ctx, &core.User{ID: 7}, core.Device{ID: "tv"}, "jellyfin", []ReceiptSubject{{Kind: "title", ID: "1"}}, guard)
	if !errors.Is(err, ErrRowViewStale) || tokens != nil {
		t.Fatalf("tokens=%v err=%v", tokens, err)
	}
	var n int
	if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM experience_receipts`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("receipts=%d err=%v", n, err)
	}
}
