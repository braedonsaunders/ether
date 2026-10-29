package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/livetv"
	"github.com/braedonsaunders/ether/internal/plextv"
)

func TestFrontendLiveTVUsesViewerPolicy(t *testing.T) {
	ctx := context.Background()
	a := setupTestApp(t, true)
	if err := a.users.SetOwner(ctx, plextv.Account{ID: 42, Username: "owner"}, "owner-token"); err != nil {
		t.Fatal(err)
	}
	owner, err := a.users.Owner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	local, _ := identity.NewLocal(a.db)
	viewerID, err := local.CreateViewer(ctx, "viewer", "password")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.users.RefreshViewers(ctx); err != nil {
		t.Fatal(err)
	}
	viewer, err := a.users.ActiveUser(ctx, viewerID)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.catalog.UpsertTitle(ctx, core.Title{Key: core.MovieKey(1), Type: core.TypeMovie, IDs: core.IDs{TMDB: 1}, Title: "Programme", Released: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)}, true); err != nil {
		t.Fatal(err)
	}
	c, err := a.livetv.SaveChannel(ctx, livetv.Channel{ID: "private-channel", Number: "1", Name: "Channel", Kind: "virtual", Enabled: true, AllowedUsers: []int64{owner.ID}, Items: []livetv.Item{{Key: core.MovieKey(1), Title: "Programme", DurationMS: 3600000}}})
	if err != nil {
		t.Fatal(err)
	}
	facade := surfaceLiveTV{app: a}
	if rows, err := facade.Channels(ctx, viewer); err != nil || len(rows) != 0 {
		t.Fatal(rows, err)
	}
	channelGUID, _ := ids.StringGUID(ids.Channel, c.ID)
	if _, err := facade.ResolveChannelGUID(ctx, viewer, channelGUID); !errors.Is(err, core.ErrNotFound) {
		t.Fatal("restricted channel leaked", err)
	}
	rows, err := facade.Channels(ctx, owner)
	if err != nil || len(rows) != 1 || rows[0].ID != c.ID {
		t.Fatal(rows, err)
	}
	if _, err := facade.ResolveChannelGUID(ctx, owner, channelGUID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	q := frontend.LiveGuideQuery{From: now, Until: now.Add(2 * time.Hour), Limit: 2}
	guide, err := facade.Guide(ctx, owner, q)
	if err != nil || len(guide) == 0 || len(guide) > 2 {
		t.Fatal(guide, err)
	}
	programmeGUID, _ := ids.StringGUID(ids.Programme, guide[0].ID)
	if got, err := facade.ResolveProgrammeGUID(ctx, owner, programmeGUID, q); err != nil || got.ID != guide[0].ID {
		t.Fatal(got, err)
	}
	if rows, err := facade.Guide(ctx, viewer, q); err != nil || len(rows) != 0 {
		t.Fatal(rows, err)
	}
	q.Until = q.From.Add(8 * 24 * time.Hour)
	if _, err := facade.Guide(ctx, owner, q); err == nil {
		t.Fatal("unbounded window")
	}
	if _, err := facade.Channels(ctx, nil); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatal(err)
	}
}
