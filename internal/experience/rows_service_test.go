package experience

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/store"
	"testing"
	"time"
)

type testRowSource struct{}

func (testRowSource) Rows(context.Context, frontend.RowRequest) ([]frontend.Row, error) {
	return []frontend.Row{{ID: "row", Purpose: "recommendation", Entries: []frontend.RowEntry{
		{Kind: frontend.RowEntryTitle, Title: &core.Title{Key: core.MovieKey(1), Adult: true}},
		{Kind: frontend.RowEntryTitle, Title: &core.Title{Key: core.MovieKey(2)}},
	}, Items: []core.Title{{Adult: true}}}}, nil
}
func TestRowsServiceFiltersAndIssuesScopedReceipts(t *testing.T) {
	db := store.OpenTest(t)
	ctx := context.Background()
	receipts, err := NewReceipts(ReceiptOptions{DB: db, TTL: time.Minute, MaxPerDevice: 10, Active: func(_ context.Context, id int64) (*core.User, error) { return &core.User{ID: id}, nil }, Allowed: func(context.Context, *core.User, core.Device, core.Protocol, ReceiptSubject) bool { return true }, Record: func(context.Context, *core.User, core.Device, core.Protocol, ReceiptSubject, frontend.RowInteractionKind) error {
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewRowsService(RowServiceDeps{Source: testRowSource{}, Visibility: Visibility{GetTitle: func(context.Context, core.RatingKey) (*core.Title, error) { return nil, core.ErrNotFound }, SectionFor: func(core.MediaType) (int, bool) { return 0, false }}, Receipts: receipts, Active: func(context.Context, *core.User) bool { return true }, AllowRow: func(context.Context, *core.User, frontend.Row) bool { return true }, AllowEntry: func(_ context.Context, _ *core.User, e frontend.RowEntry) bool {
		return e.Title != nil && !e.Title.Adult
	}})
	if err != nil {
		t.Fatal(err)
	}
	user := &core.User{ID: 1}
	device := core.Device{ID: "tv"}
	rows, err := s.Rows(ctx, frontend.RowRequest{Viewer: user, Device: device, Limit: 1}, "jellyfin")
	if err != nil || len(rows) != 1 || len(rows[0].Entries) != 1 || rows[0].Entries[0].Title.Key != core.MovieKey(2) || len(rows[0].Items) != 0 || rows[0].Handle == "" || rows[0].Entries[0].Handle == "" {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	if err = receipts.Report(ctx, frontend.RowInteraction{Viewer: user, Device: device, Protocol: "jellyfin", Handle: rows[0].Entries[0].Handle, Kind: frontend.RowClick}); err != nil {
		t.Fatal(err)
	}
}

type prefixRowSource struct{ calls []int }

func (s *prefixRowSource) Rows(_ context.Context, req frontend.RowRequest) ([]frontend.Row, error) {
	s.calls = append(s.calls, req.Limit)
	all := []frontend.RowEntry{{Kind: frontend.RowEntryTitle, Title: &core.Title{Key: core.MovieKey(1), Adult: true}}, {Kind: frontend.RowEntryTitle, Title: &core.Title{Key: core.MovieKey(2)}}}
	return []frontend.Row{{ID: "row", Kind: "taste", Purpose: "recommendation", Entries: all[:min(len(all), req.Limit)]}}, nil
}
func TestRowsServiceBackfillsBeforeLimit(t *testing.T) {
	db := store.OpenTest(t)
	source := &prefixRowSource{}
	receipts, err := NewReceipts(ReceiptOptions{DB: db, TTL: time.Minute, MaxPerDevice: 10, Active: func(_ context.Context, id int64) (*core.User, error) { return &core.User{ID: id}, nil }, Allowed: func(context.Context, *core.User, core.Device, core.Protocol, ReceiptSubject) bool { return true }, Record: func(context.Context, *core.User, core.Device, core.Protocol, ReceiptSubject, frontend.RowInteractionKind) error {
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewRowsService(RowServiceDeps{Source: source, Receipts: receipts, Visibility: Visibility{GetTitle: func(context.Context, core.RatingKey) (*core.Title, error) { return nil, core.ErrNotFound }, SectionFor: func(core.MediaType) (int, bool) { return 0, false }}, Active: func(context.Context, *core.User) bool { return true }, AllowRow: func(context.Context, *core.User, frontend.Row) bool { return true }, AllowEntry: func(context.Context, *core.User, frontend.RowEntry) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	got, err := service.Rows(context.Background(), frontend.RowRequest{Viewer: &core.User{ID: 1}, Device: core.Device{ID: "tv"}, Limit: 1}, "jellyfin")
	if err != nil || len(got) != 1 || got[0].Kind != "taste" || got[0].Entries[0].Title.Key != core.MovieKey(2) || len(source.calls) != 2 {
		t.Fatalf("rows=%+v calls=%v err=%v", got, source.calls, err)
	}
}
