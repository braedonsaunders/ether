package experience

import (
	"context"
	"errors"
	"regexp"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestRowViewsScopedStablePagedAndReauthorized(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	active := func(_ context.Context, id int64) (*core.User, error) { return &core.User{ID: id}, nil }
	var recorded *core.NeutralImpression
	receipts, err := NewReceipts(ReceiptOptions{DB: db, TTL: time.Hour, Now: func() time.Time { return now }, MaxPerDevice: 100, Active: active, Allowed: func(context.Context, *core.User, core.Device, core.Protocol, ReceiptSubject) bool { return true }, AllowedAction: func(context.Context, *core.User, core.Device, core.Protocol, ReceiptSubject, frontend.RowInteractionKind) bool {
		return true
	}, Record: func(_ context.Context, _ *core.User, _ core.Device, _ core.Protocol, s ReceiptSubject, _ frontend.RowInteractionKind) error {
		recorded = s.Impression
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	title := "Original"
	visible := true
	valid := true
	opt := RowViewOptions{DB: db, TTL: time.Minute, Now: func() time.Time { return now }, Active: active, Receipts: receipts,
		Build: func(_ context.Context, r frontend.RowViewRequest) ([]RowViewSnapshot, error) {
			return []RowViewSnapshot{{Row: frontend.Row{ID: "taste", Kind: "taste", Title: title, Purpose: frontend.RowRecommendation, Rank: 3}, Situation: core.HomeSituation{ClientID: "tv"}, Placement: r.Placement, ViewID: r.ViewID, Item: r.Item, Entries: []core.NeutralEntryRef{{Kind: "title", ID: core.MovieKey(1).String()}, {Kind: "collection", ID: core.CollectionKey(2).String()}, {Kind: "title", ID: core.MovieKey(3).String()}}, Truncated: true, Revision: "v1"}}, nil
		},
		ValidRow: func(context.Context, *core.User, core.Device, core.Protocol, RowViewSnapshot) bool { return valid },
		ResolveEntry: func(_ context.Context, _ *core.User, _ core.Device, _ core.Protocol, ref core.NeutralEntryRef) (frontend.RowEntry, error) {
			if !visible {
				return frontend.RowEntry{}, core.ErrNotFound
			}
			key, e := core.ParseRatingKey(ref.ID)
			if e != nil {
				return frontend.RowEntry{}, e
			}
			if ref.Kind == "collection" {
				return frontend.RowEntry{Kind: frontend.RowEntryCollection, Collection: &core.Collection{Key: key}}, nil
			}
			return frontend.RowEntry{Kind: frontend.RowEntryTitle, Title: &core.Title{Key: key}}, nil
		},
	}
	service, err := NewRowViewService(opt)
	if err != nil {
		t.Fatal(err)
	}
	otherNode, err := NewRowViewService(opt)
	if err != nil {
		t.Fatal(err)
	}
	viewer, device := &core.User{ID: 7}, core.Device{ID: "tv"}
	listReq := frontend.RowViewRequest{Viewer: viewer, Device: device, Protocol: "jellyfin", Placement: frontend.PlacementHome}
	views, err := service.ListRowViews(ctx, listReq)
	if err != nil || len(views) != 1 {
		t.Fatal(views, err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(views[0].ID) {
		t.Fatalf("row view is not a visit-scoped UUID: %q", views[0].ID)
	}
	title = "Changed"
	again, err := otherNode.ListRowViews(ctx, listReq)
	if err != nil || again[0].ID != views[0].ID || again[0].Title != "Original" {
		t.Fatal(again, err)
	}
	query := frontend.RowViewPageRequest{Viewer: viewer, Device: device, Protocol: "jellyfin", ID: views[0].ID, Offset: 1, Limit: 1, Kinds: []frontend.RowEntryKind{frontend.RowEntryTitle}}
	firstPage := query
	firstPage.Offset = 0
	if first, e := otherNode.ResolveRowView(ctx, firstPage); e != nil || !first.More || !first.Truncated {
		t.Fatalf("first bounded page=%+v err=%v", first, e)
	}
	page, err := otherNode.ResolveRowView(ctx, query)
	if err != nil || len(page.Entries) != 1 || page.Entries[0].Title.Key != core.MovieKey(3) || page.Total != 2 || page.TotalExact || page.More || !page.Truncated || page.ImpressionHandle == "" || page.Entries[0].Handle == "" {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	if err = receipts.Report(ctx, frontend.RowInteraction{Viewer: viewer, Device: device, Protocol: "jellyfin", Handle: page.ImpressionHandle, Kind: frontend.RowImpression}); err != nil || recorded == nil || len(recorded.Items) != 1 || recorded.Items[0] != core.MovieKey(3) || len(recorded.Entries) != 1 || recorded.Rank != 3 {
		t.Fatalf("receipt=%+v err=%v", recorded, err)
	}
	wrong := query
	wrong.Viewer = &core.User{ID: 8}
	if _, err = service.ResolveRowView(ctx, wrong); !errors.Is(err, ErrRowViewNotFound) {
		t.Fatal("viewer scope", err)
	}
	wrong = query
	wrong.Device.ID = "other"
	if _, err = service.ResolveRowView(ctx, wrong); !errors.Is(err, ErrRowViewNotFound) {
		t.Fatal("device scope", err)
	}
	wrong = query
	wrong.Protocol = "emby"
	if _, err = service.ResolveRowView(ctx, wrong); !errors.Is(err, ErrRowViewNotFound) {
		t.Fatal("surface scope", err)
	}
	visible = false
	if _, err = service.ResolveRowView(ctx, query); !errors.Is(err, ErrRowViewStale) {
		t.Fatal("revoked entry", err)
	}
	visible = true
	valid = false
	if _, err = service.ResolveRowView(ctx, query); !errors.Is(err, ErrRowViewStale) {
		t.Fatal("revoked row", err)
	}
	valid = true
	now = now.Add(2 * time.Minute)
	if _, err = service.ResolveRowView(ctx, query); !errors.Is(err, ErrRowViewStale) {
		t.Fatal("expired snapshot", err)
	}
	refreshed, err := service.ListRowViews(ctx, listReq)
	if err != nil || refreshed[0].ID == views[0].ID || refreshed[0].Title != "Changed" {
		t.Fatal(refreshed, err)
	}
	var stored int
	if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM experience_row_views WHERE viewer_id=? AND device_id=?`, viewer.ID, device.ID).Scan(&stored); err != nil || stored != 1 {
		t.Fatalf("expired row snapshots retained: %d %v", stored, err)
	}
	if _, err = otherNode.ResolveRowView(ctx, query); !errors.Is(err, ErrRowViewNotFound) && !errors.Is(err, ErrRowViewStale) {
		t.Fatalf("old visit ID resolved after renewal: %v", err)
	}
}

func TestRowViewsRecheckViewerBeforeReturningEmptyPagesAndLists(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	var active, revoke atomic.Bool
	active.Store(true)
	viewer := func(_ context.Context, id int64) (*core.User, error) {
		if !active.Load() {
			return nil, core.ErrUnauthorized
		}
		return &core.User{ID: id}, nil
	}
	receipts, err := NewReceipts(ReceiptOptions{DB: db, TTL: time.Minute, MaxPerDevice: 10, Active: viewer, Allowed: func(context.Context, *core.User, core.Device, core.Protocol, ReceiptSubject) bool { return true }, AllowedAction: func(context.Context, *core.User, core.Device, core.Protocol, ReceiptSubject, frontend.RowInteractionKind) bool {
		return true
	}, Record: func(context.Context, *core.User, core.Device, core.Protocol, ReceiptSubject, frontend.RowInteractionKind) error {
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	key := core.MovieKey(1)
	s, err := NewRowViewService(RowViewOptions{DB: db, TTL: time.Minute, Active: viewer, Receipts: receipts,
		Build: func(_ context.Context, r frontend.RowViewRequest) ([]RowViewSnapshot, error) {
			return []RowViewSnapshot{{Row: frontend.Row{ID: "one", Kind: "taste"}, Placement: r.Placement, Entries: []core.NeutralEntryRef{{Kind: "title", ID: key.String()}}}}, nil
		},
		ValidRow: func(context.Context, *core.User, core.Device, core.Protocol, RowViewSnapshot) bool { return true },
		ResolveEntry: func(context.Context, *core.User, core.Device, core.Protocol, core.NeutralEntryRef) (frontend.RowEntry, error) {
			if revoke.Swap(false) {
				active.Store(false)
			}
			return frontend.RowEntry{Kind: frontend.RowEntryTitle, Title: &core.Title{Key: key}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	u, d := &core.User{ID: 1}, core.Device{ID: "tv"}
	list := frontend.RowViewRequest{Viewer: u, Device: d, Protocol: "jellyfin", Placement: frontend.PlacementHome}
	views, err := s.ListRowViews(ctx, list)
	if err != nil {
		t.Fatal(err)
	}
	revoke.Store(true)
	page, err := s.ResolveRowView(ctx, frontend.RowViewPageRequest{Viewer: u, Device: d, Protocol: "jellyfin", ID: views[0].ID, Offset: 99, Limit: 1})
	if !errors.Is(err, ErrRowViewStale) || page.Total != 0 {
		t.Fatalf("empty page exposed after viewer revoked: %+v %v", page, err)
	}
	active.Store(true)
	revoke.Store(true)
	views, err = s.ListRowViews(ctx, list)
	if !errors.Is(err, ErrRowViewNotFound) || views != nil {
		t.Fatalf("list exposed after viewer revoked: %+v %v", views, err)
	}
}

func TestRowViewRejectsEntryPolicyChangeDuringEmptyPageResolution(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	active := func(_ context.Context, id int64) (*core.User, error) { return &core.User{ID: id}, nil }
	receipts, err := NewReceipts(ReceiptOptions{DB: db, TTL: time.Minute, MaxPerDevice: 10, Active: active, Allowed: func(context.Context, *core.User, core.Device, core.Protocol, ReceiptSubject) bool { return true }, AllowedAction: func(context.Context, *core.User, core.Device, core.Protocol, ReceiptSubject, frontend.RowInteractionKind) bool {
		return true
	}, Record: func(context.Context, *core.User, core.Device, core.Protocol, ReceiptSubject, frontend.RowInteractionKind) error {
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	var remaining atomic.Int32
	remaining.Store(-1)
	key := core.MovieKey(1)
	s, err := NewRowViewService(RowViewOptions{DB: db, TTL: time.Minute, Active: active, Receipts: receipts, Build: func(_ context.Context, r frontend.RowViewRequest) ([]RowViewSnapshot, error) {
		return []RowViewSnapshot{{Row: frontend.Row{ID: "row", Kind: "taste"}, Placement: r.Placement, Entries: []core.NeutralEntryRef{{Kind: "title", ID: key.String()}}}}, nil
	}, ValidRow: func(context.Context, *core.User, core.Device, core.Protocol, RowViewSnapshot) bool { return true }, ResolveEntry: func(context.Context, *core.User, core.Device, core.Protocol, core.NeutralEntryRef) (frontend.RowEntry, error) {
		if remaining.Load() >= 0 && remaining.Add(-1) < 0 {
			return frontend.RowEntry{}, core.ErrNotFound
		}
		return frontend.RowEntry{Kind: frontend.RowEntryTitle, Title: &core.Title{Key: key}}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	u, d := &core.User{ID: 1}, core.Device{ID: "tv"}
	views, err := s.ListRowViews(ctx, frontend.RowViewRequest{Viewer: u, Device: d, Protocol: "jellyfin", Placement: frontend.PlacementHome})
	if err != nil {
		t.Fatal(err)
	}
	remaining.Store(1)
	page, err := s.ResolveRowView(ctx, frontend.RowViewPageRequest{Viewer: u, Device: d, Protocol: "jellyfin", ID: views[0].ID, Offset: 99, Limit: 1})
	if !errors.Is(err, ErrRowViewStale) || page.Total != 0 {
		t.Fatalf("stale entry exposed in empty page: %+v %v", page, err)
	}
}

func TestRowViewBatchResolverChecksEachPass(t *testing.T) {
	var calls int
	key := core.MovieKey(1)
	s := &RowViewService{o: RowViewOptions{ResolveEntry: func(context.Context, *core.User, core.Device, core.Protocol, core.NeutralEntryRef) (frontend.RowEntry, error) {
		t.Fatal("individual lookup used")
		return frontend.RowEntry{}, nil
	}, ResolveEntries: func(_ context.Context, _ *core.User, _ core.Device, _ core.Protocol, refs []core.NeutralEntryRef) ([]frontend.RowEntry, error) {
		calls++
		if calls == 2 {
			return nil, core.ErrNotFound
		}
		return []frontend.RowEntry{{Kind: frontend.RowEntryTitle, Title: &core.Title{Key: key}}}, nil
	}}}
	refs := []core.NeutralEntryRef{{Kind: "title", ID: key.String()}}
	if _, err := s.resolveAll(context.Background(), &core.User{ID: 1}, core.Device{ID: "tv"}, "jellyfin", refs); err != nil {
		t.Fatal(err)
	}
	if _, err := s.resolveAll(context.Background(), &core.User{ID: 1}, core.Device{ID: "tv"}, "jellyfin", refs); !errors.Is(err, ErrRowViewStale) || calls != 2 {
		t.Fatalf("revocation hidden: calls=%d error=%v", calls, err)
	}
}

func TestRowViewRefreshDuringPageResolutionReturnsStale(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	active := func(_ context.Context, id int64) (*core.User, error) { return &core.User{ID: id}, nil }
	receipts, err := NewReceipts(ReceiptOptions{DB: db, TTL: time.Hour, MaxPerDevice: 20, Active: active, Allowed: func(context.Context, *core.User, core.Device, core.Protocol, ReceiptSubject) bool { return true }, AllowedAction: func(context.Context, *core.User, core.Device, core.Protocol, ReceiptSubject, frontend.RowInteractionKind) bool {
		return true
	}, Record: func(context.Context, *core.User, core.Device, core.Protocol, ReceiptSubject, frontend.RowInteractionKind) error {
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	started, proceed := make(chan struct{}), make(chan struct{})
	var block atomic.Bool
	key := core.MovieKey(1)
	s, err := NewRowViewService(RowViewOptions{DB: db, TTL: time.Minute, Active: active, Receipts: receipts,
		Build: func(_ context.Context, r frontend.RowViewRequest) ([]RowViewSnapshot, error) {
			return []RowViewSnapshot{{Row: frontend.Row{ID: "row", Kind: "taste", Purpose: frontend.RowRecommendation}, Placement: r.Placement, Entries: []core.NeutralEntryRef{{Kind: "title", ID: key.String()}}}}, nil
		},
		ValidRow: func(context.Context, *core.User, core.Device, core.Protocol, RowViewSnapshot) bool { return true },
		ResolveEntry: func(context.Context, *core.User, core.Device, core.Protocol, core.NeutralEntryRef) (frontend.RowEntry, error) {
			if block.CompareAndSwap(true, false) {
				close(started)
				<-proceed
			}
			return frontend.RowEntry{Kind: frontend.RowEntryTitle, Title: &core.Title{Key: key}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	viewer, device := &core.User{ID: 1}, core.Device{ID: "tv"}
	list := frontend.RowViewRequest{Viewer: viewer, Device: device, Protocol: "jellyfin", Placement: frontend.PlacementHome}
	views, err := s.ListRowViews(ctx, list)
	if err != nil {
		t.Fatal(err)
	}
	query := frontend.RowViewPageRequest{Viewer: viewer, Device: device, Protocol: "jellyfin", ID: views[0].ID, Limit: 1}
	block.Store(true)
	result := make(chan error, 1)
	go func() { _, err := s.ResolveRowView(ctx, query); result <- err }()
	<-started
	list.Refresh = true
	newViews, err := s.ListRowViews(ctx, list)
	if err != nil {
		close(proceed)
		t.Fatal(err)
	}
	close(proceed)
	if err = <-result; !errors.Is(err, ErrRowViewStale) {
		t.Fatalf("old page returned after refresh: %v", err)
	}
	if newViews[0].ID == views[0].ID {
		t.Fatal("refresh identity/generation", newViews)
	}
}

func TestRowViewRefreshDuringFinalPolicyPassReturnsStale(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	active := func(_ context.Context, id int64) (*core.User, error) { return &core.User{ID: id}, nil }
	receipts, err := NewReceipts(ReceiptOptions{DB: db, TTL: time.Minute, MaxPerDevice: 20, Active: active, Allowed: func(context.Context, *core.User, core.Device, core.Protocol, ReceiptSubject) bool { return true }, AllowedAction: func(context.Context, *core.User, core.Device, core.Protocol, ReceiptSubject, frontend.RowInteractionKind) bool {
		return true
	}, Record: func(context.Context, *core.User, core.Device, core.Protocol, ReceiptSubject, frontend.RowInteractionKind) error {
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	var remaining atomic.Int32
	var mode atomic.Int32
	key := core.MovieKey(1)
	s, err := NewRowViewService(RowViewOptions{DB: db, TTL: time.Minute, Active: active, Receipts: receipts,
		Build: func(_ context.Context, r frontend.RowViewRequest) ([]RowViewSnapshot, error) {
			return []RowViewSnapshot{{Row: frontend.Row{ID: "row", Kind: "taste"}, Placement: r.Placement, Entries: []core.NeutralEntryRef{{Kind: "title", ID: key.String()}}}}, nil
		},
		ValidRow: func(context.Context, *core.User, core.Device, core.Protocol, RowViewSnapshot) bool { return true },
		ResolveEntry: func(ctx context.Context, _ *core.User, _ core.Device, _ core.Protocol, _ core.NeutralEntryRef) (frontend.RowEntry, error) {
			if remaining.Load() > 0 && remaining.Add(-1) == 0 {
				query := `UPDATE experience_row_view_manifests SET generation='changed'`
				if mode.Load() == 2 {
					query = `UPDATE experience_row_views SET snapshot='changed'`
				}
				if _, err := db.ExecContext(ctx, query); err != nil {
					return frontend.RowEntry{}, err
				}
			}
			return frontend.RowEntry{Kind: frontend.RowEntryTitle, Title: &core.Title{Key: key}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	viewer, device := &core.User{ID: 1}, core.Device{ID: "tv"}
	list := frontend.RowViewRequest{Viewer: viewer, Device: device, Protocol: "jellyfin", Placement: frontend.PlacementHome}
	if _, err = s.ListRowViews(ctx, list); err != nil {
		t.Fatal(err)
	}
	mode.Store(1)
	remaining.Store(2)
	if _, err = s.ListRowViews(ctx, list); !errors.Is(err, ErrRowViewStale) {
		t.Fatalf("manifest changed during final policy pass: %v", err)
	}
	mode.Store(0)
	list.Refresh = true
	views, err := s.ListRowViews(ctx, list)
	if err != nil {
		t.Fatal(err)
	}
	mode.Store(2)
	remaining.Store(2)
	_, err = s.ResolveRowView(ctx, frontend.RowViewPageRequest{Viewer: viewer, Device: device, Protocol: "jellyfin", ID: views[0].ID, Offset: 99, Limit: 1})
	if !errors.Is(err, ErrRowViewStale) {
		t.Fatalf("snapshot changed during final policy pass: %v", err)
	}
}

func TestRowViewListUsesExistingSnapshotRanks(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	active := func(_ context.Context, id int64) (*core.User, error) { return &core.User{ID: id}, nil }
	receipts, err := NewReceipts(ReceiptOptions{DB: db, TTL: time.Minute, MaxPerDevice: 10, Active: active, Allowed: func(context.Context, *core.User, core.Device, core.Protocol, ReceiptSubject) bool { return true }, AllowedAction: func(context.Context, *core.User, core.Device, core.Protocol, ReceiptSubject, frontend.RowInteractionKind) bool {
		return true
	}, Record: func(context.Context, *core.User, core.Device, core.Protocol, ReceiptSubject, frontend.RowInteractionKind) error {
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	reversed := false
	s, err := NewRowViewService(RowViewOptions{DB: db, TTL: time.Minute, Active: active, Receipts: receipts, ValidRow: func(context.Context, *core.User, core.Device, core.Protocol, RowViewSnapshot) bool { return true }, ResolveEntry: func(_ context.Context, _ *core.User, _ core.Device, _ core.Protocol, r core.NeutralEntryRef) (frontend.RowEntry, error) {
		key, e := core.ParseRatingKey(r.ID)
		return frontend.RowEntry{Kind: frontend.RowEntryTitle, Title: &core.Title{Key: key}}, e
	}, Build: func(_ context.Context, r frontend.RowViewRequest) ([]RowViewSnapshot, error) {
		one := RowViewSnapshot{Row: frontend.Row{ID: "one", Kind: "taste", Rank: 0}, Placement: r.Placement, Entries: []core.NeutralEntryRef{{Kind: "title", ID: core.MovieKey(1).String()}}}
		two := RowViewSnapshot{Row: frontend.Row{ID: "two", Kind: "taste", Rank: 1}, Placement: r.Placement, Entries: []core.NeutralEntryRef{{Kind: "title", ID: core.MovieKey(2).String()}}}
		if reversed {
			two.Row.Rank = 0
			return []RowViewSnapshot{two}, nil
		}
		return []RowViewSnapshot{one, two}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	request := frontend.RowViewRequest{Viewer: &core.User{ID: 1}, Device: core.Device{ID: "tv"}, Protocol: "jellyfin", Placement: frontend.PlacementHome}
	first, err := s.ListRowViews(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	reversed = true
	second, err := s.ListRowViews(ctx, request)
	if err != nil || len(second) != 2 || second[0].ID != first[0].ID || second[1].ID != first[1].ID {
		t.Fatal(second, err)
	}
	request.Refresh = true
	third, err := s.ListRowViews(ctx, request)
	if err != nil || len(third) != 1 || third[0].ID == first[1].ID {
		t.Fatal(third, err)
	}
	_, err = s.ResolveRowView(ctx, frontend.RowViewPageRequest{Viewer: request.Viewer, Device: request.Device, Protocol: request.Protocol, ID: first[0].ID, Limit: 1})
	if !errors.Is(err, ErrRowViewStale) && !errors.Is(err, ErrRowViewNotFound) {
		t.Fatalf("removed row retained after refresh: %v", err)
	}
}
