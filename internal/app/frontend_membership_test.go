package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/plextv"
)

func TestProductionMembershipReadsStoredViewerState(t *testing.T) {
	a := setupTestApp(t, true)
	ctx := context.Background()
	if err := a.users.SetOwner(ctx, plextv.Account{ID: 42, Username: "owner"}, "owner-token"); err != nil {
		t.Fatal(err)
	}
	owner, err := a.users.Owner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	title := core.Title{Key: core.MovieKey(777), Type: core.TypeMovie, IDs: core.IDs{TMDB: 777}, Title: "Test", ContentRating: "PG", Released: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)}
	if err = a.catalog.UpsertTitle(ctx, title, true); err != nil {
		t.Fatal(err)
	}
	if _, err = a.db.ExecContext(ctx, `UPDATE catalog_titles SET detail=1,detail_at=? WHERE key=?`, time.Now().Unix(), int64(title.Key)); err != nil {
		t.Fatal(err)
	}
	availability := a.availability
	a.availability = nil
	t.Cleanup(func() { a.availability = availability })
	exp, err := a.newSurfaceExperience("jellyfin")
	if err != nil {
		t.Fatal(err)
	}
	reader, ok := exp.(frontend.MembershipReader)
	if !ok {
		t.Fatal("production membership capability missing")
	}
	device := core.Device{ID: "tv"}
	before, err := reader.FavoriteItem(ctx, owner, device, title.Key)
	if err != nil || before.Present || before.AddedAt != nil {
		t.Fatal(before, err)
	}
	if err = exp.SetFavorite(ctx, owner, title.Key, true); err != nil {
		t.Fatal(err)
	}
	if err = exp.SetWatchlist(ctx, owner, title.Key, true); err != nil {
		t.Fatal(err)
	}
	for _, read := range []func(context.Context, *core.Viewer, core.Device, core.RatingKey) (frontend.Membership, error){
		reader.FavoriteItem, reader.WatchlistItem,
	} {
		got, err := read(ctx, owner, device, title.Key)
		if err != nil || !got.Present || got.AddedAt == nil || got.AddedAt.IsZero() {
			t.Fatal(got, err)
		}
		if _, err := read(ctx, &core.User{ID: owner.ID + 10000}, device, title.Key); !errors.Is(err, core.ErrNotFound) {
			t.Fatalf("inactive viewer got membership: %v", err)
		}
	}
}
