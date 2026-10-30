package app

import (
	"context"
	"errors"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/plex"
	"github.com/braedonsaunders/ether/internal/plextv"
	"github.com/braedonsaunders/ether/internal/tmdb"
)

func TestAppHomeDesignerLayoutUsesRealEngineAndStableSidecars(t *testing.T) {
	a := setupTestApp(t, true)
	ctx := context.Background()
	_, err := a.personalize.SetHomeLayout(ctx, core.HomeLayout{Global: []core.LayoutPage{{Section: "home", Rows: []core.RowPolicy{{Row: "home.continue", Status: "always", Slot: 1}, {Row: "movie.trending", Status: "always"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	rows := []core.NeutralHomeRow{{ID: "movie.trending", Title: "first", ItemCount: 1}, {ID: "home.continue", Title: "Continue", ItemCount: 1}, {ID: "movie.trending", Title: "last", ItemCount: 1}}
	user := &core.User{ID: 1}
	situation := core.HomeSituation{Now: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	indices, err := experience.ArrangeNeutralHome(ctx, a.personalize, user, situation, "home", rows)
	if err != nil || !reflect.DeepEqual(indices, []int{1, 2, 2}) {
		t.Fatalf("configured layout indices %v: %v", indices, err)
	}
	legacy, err := a.personalize.FinalizeHome(ctx, user, core.HubContext{Now: situation.Now}, "home", []core.Hub{{ID: "movie.trending", ItemCount: 1}, {ID: "home.continue", ItemCount: 1}, {ID: "movie.trending", ItemCount: 1}})
	if err != nil || len(legacy) != len(indices) {
		t.Fatalf("legacy layout %+v: %v", legacy, err)
	}
	for i, index := range indices {
		if legacy[i].ID != rows[index].ID {
			t.Fatalf("legacy=%+v indices=%v", legacy, indices)
		}
	}
}

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
	title := core.Title{Key: core.MovieKey(999), Type: core.TypeMovie, IDs: core.IDs{TMDB: 999}, Title: "Restricted artwork", PosterPath: "/test.jpg", ContentRating: "PG-13", Released: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), Cast: []core.Person{{TMDB: 456, Name: "Credit", ThumbPath: "/person.jpg"}}}
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
	relatedProvider, ok := exp.(frontend.RelatedArtworkProvider)
	if !ok {
		t.Fatal("related artwork unavailable")
	}
	related, err := relatedProvider.RelatedArtwork(ctx, viewer, core.Device{ID: "tv"}, title.Key)
	if err != nil || !related.PeopleAuthoritative || len(related.People) != 1 || related.People[0].Subject != "tmdb:456" {
		t.Fatalf("authorized credit portrait: %+v %v", related, err)
	}
	portrait, err := service.Issue(ctx, frontend.ImageGrantRequest{Credential: credential, Device: core.Device{ID: "tv"}, Item: title.Key, Image: related.People[0]})
	if err != nil {
		t.Fatal(err)
	}
	batch, err := service.IssueBatch(ctx, frontend.ImageBatchRequest{Credential: credential, Device: core.Device{ID: "tv"}, Items: []core.RatingKey{title.Key}})
	if err != nil || len(batch) != 1 || len(batch[0].Images) != 1 {
		t.Fatalf("visible batch: %+v %v", batch, err)
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
	w = httptest.NewRecorder()
	service.Serve(w, httptest.NewRequest("GET", "/", nil), portrait)
	if w.Code != 404 {
		t.Fatalf("credit portrait grant survived title revocation: %d", w.Code)
	}
	if _, err := service.Issue(ctx, frontend.ImageGrantRequest{Credential: credential, Device: core.Device{ID: "tv"}, Item: title.Key, Image: detail.Images[0]}); err == nil {
		t.Fatal("issued artwork for a hidden title")
	}
	batch, err = service.IssueBatch(ctx, frontend.ImageBatchRequest{Credential: credential, Device: core.Device{ID: "tv"}, Items: []core.RatingKey{title.Key}})
	if err != nil || len(batch) != 0 {
		t.Fatalf("hidden title exposed in batch: %+v %v", batch, err)
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
	if _, ok := experience.(frontend.RelatedArtworkProvider); !ok {
		t.Fatal("surface experience lacks related artwork")
	}
	request := frontend.ItemRequest{Viewer: owner, Key: title.Key}
	detail, err := experience.Item(ctx, request)
	if err != nil || detail.ImagesAuthoritative || len(detail.Images) != 1 || detail.Images[0].Path != "opaque" {
		t.Fatalf("incomplete: %+v %v", detail, err)
	}
	if a.playback == nil || len(detail.Tracks) == 0 || !detail.TracksEstimated {
		t.Fatalf("synthetic playback tracks must carry estimate provenance: %+v", detail)
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

func TestSurfaceHomeRowsEnforceRestrictedCandidatePolicy(t *testing.T) {
	ctx := context.Background()
	a := setupTestApp(t, true)
	if err := a.users.SyncFriends(ctx, []plextv.Friend{{PlexID: 77221, Username: "home-viewer", AllLibs: true}}); err != nil {
		t.Fatal(err)
	}
	users, err := a.users.Users(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var viewer *core.User
	for i := range users {
		if users[i].PlexID == 77221 {
			viewer = &users[i]
			break
		}
	}
	if viewer == nil {
		t.Fatal("viewer missing")
	}
	viewer.MaxContentRating = "PG"
	if err = a.users.UpdateUser(ctx, *viewer); err != nil {
		t.Fatal(err)
	}
	released := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	visible := core.Title{Key: core.MovieKey(77221), Type: core.TypeMovie, Title: "Visible", ContentRating: "PG", Released: released}
	denied := core.Title{Key: core.MovieKey(77222), Type: core.TypeMovie, Title: "Denied", Adult: true, Released: released}
	parent := core.Title{Key: core.ShowKey(77223), Type: core.TypeShow, Title: "Denied parent", Adult: true, Released: released}
	child := core.Title{Key: core.EpisodeKey(77223, 1, 1), Type: core.TypeEpisode, ShowKey: parent.Key, Title: "Hidden child", ContentRating: "PG", Released: released}
	for _, title := range []core.Title{visible, denied, parent, child} {
		if err = a.catalog.UpsertTitle(ctx, title, title.Type == core.TypeMovie || title.Type == core.TypeShow); err != nil {
			t.Fatal(err)
		}
		if _, err = a.db.ExecContext(ctx, `UPDATE catalog_titles SET detail=1,detail_at=? WHERE key=?`, time.Now().Unix(), int64(title.Key)); err != nil {
			t.Fatal(err)
		}
	}
	for _, title := range []core.Title{visible, denied, child} {
		if err = a.state.Progress(ctx, viewer.ID, title.Key, 10*time.Minute, time.Hour, "paused"); err != nil {
			t.Fatal(err)
		}
	}
	oldAvailability := a.availability
	a.availability = nil
	t.Cleanup(func() { a.availability = oldAvailability })
	service, err := a.newSurfaceExperience("jellyfin")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := service.Rows(ctx, frontend.RowRequest{Viewer: viewer, Device: core.Device{ID: "home-test"}, Placement: frontend.PlacementHome, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range rows {
		for _, entry := range row.Entries {
			if entry.Title == nil {
				continue
			}
			switch entry.Title.Key {
			case denied.Key, child.Key, parent.Key:
				t.Fatalf("restricted title escaped Home: %+v", entry.Title)
			case visible.Key:
				if row.Purpose == frontend.RowContinue {
					found = true
				}
			}
		}
	}
	if !found {
		t.Fatalf("authorized resume missing from Home: %+v", rows)
	}
}
