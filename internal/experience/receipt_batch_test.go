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

// A later receipt bind failure must roll back the entire row-view batch.
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

// A policy change after the receipt transaction returns no handles, but the
// already committed receipts remain until their normal expiry and scope sweep.
func TestRowViewPostCommitStaleReceiptsExpire(t *testing.T) {
	db := store.OpenTest(t)
	now := time.Unix(1700000000, 0)
	opts := relayOptions(db, &now, 100, true, nil)
	valid := true
	opts.bindHeaders = func(ctx context.Context, tx *sql.Tx, hs []capability.Header) error {
		if err := capability.BindHeadersInTx(ctx, tx, hs); err != nil {
			return err
		}
		valid = false
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
		ValidRow: func(context.Context, *core.User, core.Device, core.Protocol, RowViewSnapshot) bool { return valid },
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
	if !errors.Is(err, ErrRowViewStale) || page.ImpressionHandle != "" || len(page.Entries) != 0 {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	var n int
	for _, table := range []string{"experience_receipts", "capability_headers"} {
		if err = db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil || n != 2 {
			t.Fatalf("%s count=%d err=%v", table, n, err)
		}
	}
	now = now.Add(time.Minute)
	valid = true
	if _, err = receipts.Issue(ctx, viewer, device, "jellyfin", ReceiptSubject{Kind: "title", ID: "next"}); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"experience_receipts", "capability_headers"} {
		if err = db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil || n != 1 {
			t.Fatalf("after expiry %s count=%d err=%v", table, n, err)
		}
	}
}

func TestReceiptBoundedSweepIgnoresExpiredBacklogForQuota(t *testing.T) {
	db := store.OpenTest(t)
	now := time.Unix(1700000000, 0)
	opts := relayOptions(db, &now, 1, true, nil)
	s, err := NewReceipts(opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	scope := capability.HeaderScope{Namespace: ReceiptCapabilityNamespace, ViewerID: 7, Surface: "jellyfin", DeviceID: "tv"}
	for i := 0; i < 129; i++ {
		digest := fmt.Sprintf("%064x", i+1)
		_, err = tx.ExecContext(ctx, `INSERT INTO experience_receipts(hash,viewer_id,device_id,protocol,expires,kind,subject,purpose,rank) VALUES(?,?,?,?,?,?,?,?,?)`, digest, 7, "tv", "jellyfin", now.Unix()-1, "title", "old", "", 0)
		if err != nil {
			t.Fatal(err)
		}
		h := capability.Header{HeaderScope: scope, Digest: digest, ExpiresMillis: now.Add(-time.Second).UnixMilli()}
		if err = capability.BindHeadersInTx(ctx, tx, []capability.Header{h}); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	viewer := &core.User{ID: 7}
	device := core.Device{ID: "tv"}
	if _, err = s.Issue(ctx, viewer, device, "jellyfin", ReceiptSubject{Kind: "title", ID: "new"}); err != nil {
		t.Fatalf("expired backlog blocked live quota: %v", err)
	}
	var expired, live, headers int
	if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM experience_receipts WHERE expires<=?`, now.Unix()).Scan(&expired); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM experience_receipts WHERE expires>?`, now.Unix()).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM capability_headers WHERE namespace=?`, ReceiptCapabilityNamespace).Scan(&headers); err != nil {
		t.Fatal(err)
	}
	if expired != 1 || live != 1 || headers != 2 {
		t.Fatalf("expired=%d live=%d headers=%d", expired, live, headers)
	}
	if _, err = s.Issue(ctx, viewer, device, "jellyfin", ReceiptSubject{Kind: "title", ID: "another"}); err == nil {
		t.Fatal("live quota bypassed")
	}
}
