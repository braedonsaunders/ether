package app

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/plextv"
	"testing"
	"time"
)

type missingCollectionRowSource struct{ rowViewProductionSource }

func (s missingCollectionRowSource) Rows(ctx context.Context, req frontend.RowRequest) ([]frontend.Row, error) {
	rows, err := s.rowViewProductionSource.Rows(ctx, req)
	for i := range rows {
		rows[i].Collection = core.CollectionKey(42)
	}
	return rows, err
}
func TestRuntimeRowViewsMissingCollectionFailsClosed(t *testing.T) {
	a := setupTestApp(t, true)
	ctx := context.Background()
	if err := a.users.SetOwner(ctx, plextv.Account{ID: 321, Username: "owner"}, "token"); err != nil {
		t.Fatal(err)
	}
	viewer, err := a.users.Owner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	title := core.Title{Key: core.MovieKey(42), Type: core.TypeMovie, IDs: core.IDs{TMDB: 42}, Title: "Visible", Released: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)}
	if err = a.catalog.UpsertTitle(ctx, title, true); err != nil {
		t.Fatal(err)
	}
	// Old detail data must still be revalidated from the cache without a
	// network refresh during row-view listing or paging.
	if _, err = a.db.ExecContext(ctx, `UPDATE catalog_titles SET detail=1,detail_at=? WHERE key=?`, int64(1), int64(title.Key)); err != nil {
		t.Fatal(err)
	}
	libraries := []experience.Library{{ID: 1, Name: "Movies", Type: core.TypeMovie}}
	visibility := experienceVisibility(a.safety, a.catalog.Get, nil, nil, libraries)
	receipts, err := experience.NewReceipts(experience.ReceiptOptions{DB: a.db, TTL: time.Minute, MaxPerDevice: 100, Active: a.users.ActiveUser, Allowed: func(context.Context, *core.User, core.Device, core.Protocol, experience.ReceiptSubject) bool {
		return true
	}, AllowedAction: func(context.Context, *core.User, core.Device, core.Protocol, experience.ReceiptSubject, frontend.RowInteractionKind) bool {
		return true
	}, Record: func(context.Context, *core.User, core.Device, core.Protocol, experience.ReceiptSubject, frontend.RowInteractionKind) error {
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := experience.NewRowsService(experience.RowServiceDeps{Source: missingCollectionRowSource{rowViewProductionSource{title}}, Visibility: visibility, Receipts: receipts, RankedSource: true, Active: func(context.Context, *core.User) bool { return true }, AllowRow: func(context.Context, *core.User, frontend.Row) bool { return true }, AllowEntry: func(context.Context, *core.User, frontend.RowEntry) bool { return true }, Situation: func(ctx context.Context, _ frontend.RowRequest) (core.HomeSituation, error) {
		return ctx.Value(experienceSituationKey{}).(core.HomeSituation), nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	a.collections = nil
	service, err := a.newRowViews("jellyfin", rows, receipts, visibility, libraries)
	if err != nil {
		t.Fatal(err)
	}

	views, err := service.ListRowViews(ctx, frontend.RowViewRequest{Viewer: viewer, Device: core.Device{ID: "tv"}, Protocol: "jellyfin", Placement: frontend.PlacementHome})
	if err != nil || len(views) != 0 {
		t.Fatalf("missing collection source: views=%+v err=%v", views, err)
	}
}
