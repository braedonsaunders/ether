package app

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/artwork"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/livetv"
	"github.com/braedonsaunders/ether/internal/plex"
	"github.com/braedonsaunders/ether/internal/plextv"
)

type testRemoteLogo struct{}

func (testRemoteLogo) GetUntrustedURL(context.Context, string) ([]byte, string, error) {
	return []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><rect width="10" height="10" fill="#f00"/></svg>`), "image/svg+xml", nil
}

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

func TestChannelArtworkGrantFollowsLiveTVPolicy(t *testing.T) {
	ctx := context.Background()
	a := setupTestApp(t, true)
	local, err := identity.NewLocal(a.db)
	if err != nil {
		t.Fatal(err)
	}
	viewerID, err := local.CreateViewer(ctx, "channelviewer", "password")
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
	title := core.Title{Key: core.MovieKey(1), Type: core.TypeMovie, IDs: core.IDs{TMDB: 1}, Title: "Programme", Released: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)}
	if err := a.catalog.UpsertTitle(ctx, title, true); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.ExecContext(ctx, `UPDATE catalog_titles SET detail=1,detail_at=? WHERE key=?`, time.Now().Unix(), int64(title.Key)); err != nil {
		t.Fatal(err)
	}
	channel, err := a.livetv.SaveChannel(ctx, livetv.Channel{ID: "scoped-channel", Number: "9", Name: "News", Kind: "virtual", Enabled: true, AllowedUsers: []int64{viewerID}, Items: []livetv.Item{{Key: core.MovieKey(1), Title: "Programme", DurationMS: 3600000}}})
	if err != nil {
		t.Fatal(err)
	}
	source := plex.FrontendArtwork{Server: a.plexServer, URLImages: testRemoteLogo{}, Raster: artwork.NewSVGRasterizer()}
	facade := surfaceLiveTV{app: a, channelArtwork: source.ChannelImage}
	channels, err := facade.Channels(ctx, viewer)
	if err != nil || len(channels) != 1 || len(channels[0].Images) != 1 {
		t.Fatalf("channels: %+v %v", channels, err)
	}
	grants, err := identity.NewGrants(a.db, a.users, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := grants.Issue(ctx, viewerID, nil, "jellyfin", core.Device{ID: "tv", Language: "en"})
	if err != nil {
		t.Fatal(err)
	}
	service, err := identity.NewArtworkImageGrantService(a.db, "jellyfin", grants, func() frontend.Experience { return nil }, source, time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := frontend.ImageGrantRequest{Credential: credential, Device: core.Device{ID: "tv", Language: "en"}, Channel: channel.ID, Image: channels[0].Images[0]}
	secret, err := service.Issue(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	service.Serve(w, httptest.NewRequest("GET", "/", nil), secret)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "News") {
		t.Fatalf("channel logo: %d %q", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	service.Serve(w, httptest.NewRequest("GET", "/?format=png&w=320", nil), secret)
	if w.Code != 200 || w.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("channel PNG: %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	channel.AllowedUsers = []int64{viewerID + 1}
	if _, err := a.livetv.SaveChannel(ctx, channel); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	service.Serve(w, httptest.NewRequest("GET", "/", nil), secret)
	if w.Code != 404 {
		t.Fatalf("hidden channel remained accessible: %d", w.Code)
	}
	if _, err := service.Issue(ctx, request); err == nil {
		t.Fatal("issued grant for hidden channel")
	}
	request.Item = core.MovieKey(1)
	if _, err := service.Issue(ctx, request); err == nil {
		t.Fatal("accepted both item and channel")
	}
	channel.AllowedUsers = []int64{viewerID}
	channel.Logo = "https://example.invalid/logo.png?token=private"
	if _, err := a.livetv.SaveChannel(ctx, channel); err != nil {
		t.Fatal(err)
	}
	channels, err = facade.Channels(ctx, viewer)
	if err != nil || len(channels) != 1 || channels[0].Logo != "" || len(channels[0].Images) != 1 || strings.Contains(channels[0].Images[0].Path, "private") {
		t.Fatalf("channel logo URL was exposed: %+v %v", channels, err)
	}
	request.Item = 0
	request.Image = channels[0].Images[0]
	secret, err = service.Issue(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	service.Serve(w, httptest.NewRequest("GET", "/?format=png&w=32", nil), secret)
	if w.Code != 200 || w.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("external SVG transform: %d %q", w.Code, w.Header().Get("Content-Type"))
	}
}
