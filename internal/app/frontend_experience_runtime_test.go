package app

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/plex"
	"github.com/braedonsaunders/ether/internal/plextv"
	"github.com/braedonsaunders/ether/internal/tmdb"
)

type artworkProbe struct {
	calls    int
	complete bool
	err      error
}

func TestArtworkGrantStopsWhenViewerLosesTitleAccess(t *testing.T) {
	a := setupTestApp(t, true)
	ctx := context.Background()
	if err := a.users.SyncFriends(ctx, []plextv.Friend{{PlexID: 333, Username: "viewer", AllLibs: true}}); err != nil {
		t.Fatal(err)
	}
	users, err := a.users.Users(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var viewer *core.User
	for i := range users {
		if users[i].PlexID == 333 {
			viewer = &users[i]
			break
		}
	}
	if viewer == nil {
		t.Fatal("viewer missing")
	}
	viewer.MaxContentRating = "PG-13"
	if err := a.users.UpdateUser(ctx, *viewer); err != nil {
		t.Fatal(err)
	}
	title := core.Title{Key: core.MovieKey(999), Type: core.TypeMovie, IDs: core.IDs{TMDB: 999}, Title: "Restricted artwork", PosterPath: "/test.jpg", ContentRating: "PG-13", Released: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)}
	if err := a.catalog.UpsertTitle(ctx, title, true); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.ExecContext(ctx, `UPDATE catalog_titles SET detail=1,detail_at=? WHERE key=?`, time.Now().Unix(), int64(title.Key)); err != nil {
		t.Fatal(err)
	}
	source := plex.FrontendArtwork{Server: a.plexServer, Variants: func(*core.Title, string) (*tmdb.Images, bool) { return &tmdb.Images{}, true }}
	exp, err := a.newSurfaceExperience("jellyfin", source)
	if err != nil {
		t.Fatal(err)
	}
	grants, err := identity.NewGrants(a.db, a.users, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := grants.Issue(ctx, viewer.ID, nil, "jellyfin", core.Device{ID: "tv"})
	if err != nil {
		t.Fatal(err)
	}
	service, err := identity.NewArtworkImageGrantService(a.db, "jellyfin", grants, func() frontend.Experience { return exp }, source, time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	detail, err := exp.Item(ctx, frontend.ItemRequest{Viewer: viewer, Device: core.Device{ID: "tv"}, Key: title.Key})
	if err != nil || !detail.ImagesAuthoritative || len(detail.Images) == 0 {
		t.Fatalf("visible artwork: %+v %v", detail, err)
	}
	secret, err := service.Issue(ctx, frontend.ImageGrantRequest{Credential: credential, Device: core.Device{ID: "tv"}, Item: title.Key, Image: detail.Images[0]})
	if err != nil {
		t.Fatal(err)
	}
	viewer.MaxContentRating = "G"
	if err := a.users.UpdateUser(ctx, *viewer); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	service.Serve(w, httptest.NewRequest("GET", "/", nil), secret)
	if w.Code != 404 {
		t.Fatalf("hidden title artwork remained accessible: %d", w.Code)
	}
	if _, err := service.Issue(ctx, frontend.ImageGrantRequest{Credential: credential, Device: core.Device{ID: "tv"}, Item: title.Key, Image: detail.Images[0]}); err == nil {
		t.Fatal("issued artwork for a hidden title")
	}
}

func (p *artworkProbe) List(_ context.Context, req frontend.ArtworkRequest) ([]frontend.Image, bool, error) {
	p.calls++
	return []frontend.Image{{Kind: frontend.ImagePoster, Path: "opaque"}}, p.complete, p.err
}
func (*artworkProbe) Read(context.Context, frontend.ArtworkRequest, frontend.Image, frontend.ArtworkTransform) ([]byte, string, error) {
	return nil, "", core.ErrNotAvailable
}

func TestSurfaceExperienceArtworkFollowsItemAuthorization(t *testing.T) {
	a := setupTestApp(t, true)
	ctx := context.Background()
	if err := a.users.SetOwner(ctx, plextv.Account{ID: 42, Username: "owner"}, "owner-token"); err != nil {
		t.Fatal(err)
	}
	owner, err := a.users.Owner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	title := core.Title{Key: core.MovieKey(42), Type: core.TypeMovie, IDs: core.IDs{TMDB: 42}, Title: "Artwork test", Released: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)}
	if err := a.catalog.UpsertTitle(ctx, title, true); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.ExecContext(ctx, `UPDATE catalog_titles SET detail=1,detail_at=? WHERE key=?`, time.Now().Unix(), int64(title.Key)); err != nil {
		t.Fatal(err)
	}
	probe := &artworkProbe{}
	availability := a.availability
	a.availability = nil
	t.Cleanup(func() { a.availability = availability })
	experience, err := a.newSurfaceExperience("jellyfin", probe)
	if err != nil {
		t.Fatal(err)
	}
	request := frontend.ItemRequest{Viewer: owner, Key: title.Key}
	detail, err := experience.Item(ctx, request)
	if err != nil || detail.ImagesAuthoritative || len(detail.Images) != 1 || detail.Images[0].Path != "opaque" {
		t.Fatalf("incomplete: %+v %v", detail, err)
	}
	selector, ok := experience.(frontend.ItemArtwork)
	if !ok {
		t.Fatal("lightweight artwork policy missing")
	}
	images, complete, err := selector.ItemArtwork(ctx, owner, core.Device{}, title.Key)
	if err != nil || complete || len(images) != 1 {
		t.Fatalf("selector: %+v %v %v", images, complete, err)
	}
	probe.complete = true
	detail, err = experience.Item(ctx, request)
	if err != nil || !detail.ImagesAuthoritative {
		t.Fatalf("complete: %+v %v", detail, err)
	}
	probe.err = errors.New("source unavailable")
	detail, err = experience.Item(ctx, request)
	if err != nil || detail.ImagesAuthoritative || len(detail.Images) != 0 {
		t.Fatalf("source failure: %+v %v", detail, err)
	}
	before := probe.calls
	request.Viewer = &core.Viewer{ID: owner.ID + 10000, IsOwner: true}
	if _, err := experience.Item(ctx, request); err == nil || probe.calls != before {
		t.Fatal("unauthorized viewer reached artwork")
	}
	if _, _, err := selector.ItemArtwork(ctx, request.Viewer, core.Device{}, title.Key); err == nil || probe.calls != before {
		t.Fatal("unauthorized viewer reached lightweight artwork")
	}
}

type captureExperienceFrontend struct{ host *frontend.Host }

func (f captureExperienceFrontend) Describe() frontend.Descriptor {
	return frontend.Descriptor{ID: "experience-probe"}
}
func (f captureExperienceFrontend) Mount(_ context.Context, h frontend.Host) (frontend.Instance, error) {
	*f.host = h
	return sharedInstance{id: "experience-probe", prefix: "/probe"}, nil
}

func TestRuntimeSuppliesSurfaceExperienceAndRejectsUnknownViewer(t *testing.T) {
	a := setupTestApp(t, true)
	ctx := context.Background()
	if err := a.users.SetOwner(ctx, plextv.Account{ID: 42, Username: "owner"}, "owner-token"); err != nil {
		t.Fatal(err)
	}
	owner, err := a.users.Owner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var host frontend.Host
	if err := a.frontends.registry.Register(captureExperienceFrontend{host: &host}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.frontends.registry.Mount(ctx, "experience-probe"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.frontends.registry.Unmount(context.Background(), "experience-probe") })
	if host.Experience == nil {
		t.Fatal("surface has no experience")
	}
	views, err := host.Experience.Views(ctx, owner)
	if err != nil || len(views) != 2 {
		t.Fatalf("owner views = %v, %v", views, err)
	}
	unknown := &core.User{ID: owner.ID + 10000}
	if _, err := host.Experience.Views(ctx, unknown); err == nil {
		t.Fatal("unknown viewer obtained views")
	}
	if err := host.Experience.SetPlayed(ctx, unknown, core.MovieKey(1)); err == nil {
		t.Fatal("unknown viewer changed watch state")
	}
}
