package vela

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/store"
)

type castMembers struct{}

func (castMembers) ActiveUser(_ context.Context, id int64) (*core.User, error) {
	return &core.User{ID: id}, nil
}

type castExperience struct {
	frontendtest.Experience
	image   frontend.Image
	allowed bool
	batches int
}

func (e *castExperience) AuthorizePersonImages(_ context.Context, viewer *core.Viewer, device core.Device, targets []frontend.PersonImageTarget) ([]bool, error) {
	e.batches++
	allowed := make([]bool, len(targets))
	for n, target := range targets {
		allowed[n] = e.allowed && viewer.ID == 1 && device.ID == "tv" && target.Item == core.MovieKey(42) && frontend.SameImage(target.Image, e.image)
	}
	return allowed, nil
}
func (e *castExperience) AuthorizePersonImage(ctx context.Context, viewer *core.Viewer, device core.Device, item core.RatingKey, image frontend.Image, admission func(context.Context) error) error {
	if !e.allowed || viewer.ID != 1 || device.ID != "tv" || item != core.MovieKey(42) || !frontend.SameImage(image, e.image) {
		return core.ErrUnauthorized
	}
	if admission != nil {
		return admission(ctx)
	}
	return nil
}

type castArtwork struct{ frontendtest.Artwork }

func (castArtwork) Read(_ context.Context, req frontend.ArtworkRequest, image frontend.Image, _ frontend.ArtworkTransform) ([]byte, string, error) {
	if req.Viewer.ID != 1 || req.Item != core.MovieKey(42) || image.Kind != frontend.ImagePerson {
		return nil, "", core.ErrUnauthorized
	}
	return []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="185" height="278"/>`), "image/svg+xml", nil
}

func TestVelaCastArtworkScopedGrants(t *testing.T) {
	testVelaCreditArtworkScopedGrants(t, "cast")
}

func TestVelaCrewArtworkScopedGrants(t *testing.T) {
	testVelaCreditArtworkScopedGrants(t, "crew")
}

func testVelaCreditArtworkScopedGrants(t *testing.T, field string) {
	t.Helper()
	ctx := context.Background()
	db := store.OpenTest(t)
	if _, err := db.Exec(`INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,123,0,0),(2,456,0,0)`); err != nil {
		t.Fatal(err)
	}
	grants, err := identity.NewGrants(db, castMembers{}, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	device := core.Device{ID: "tv"}
	credential, err := grants.Issue(ctx, 1, nil, "vela", device)
	if err != nil {
		t.Fatal(err)
	}
	otherCredential, err := grants.Issue(ctx, 2, nil, "vela", device)
	if err != nil {
		t.Fatal(err)
	}
	image := frontend.Image{Kind: frontend.ImagePerson, Path: "opaque-person", Tag: "v1", Subject: "tmdb:10", Width: 185, Height: 278}
	exp := &castExperience{image: image, allowed: true}
	exp.ItemFunc = func(context.Context, frontend.ItemRequest) (frontend.ItemDetail, error) {
		title := core.Title{Key: core.MovieKey(42), Cast: []core.Person{{TMDB: 10, Name: "Actor", Role: "Character"}, {TMDB: 20, Name: "No portrait"}}}
		if field == "crew" {
			title.Cast = nil
			title.Crew = []core.Person{{TMDB: 10, Name: "Director", Role: "Director", ThumbPath: "/known.jpg"}, {TMDB: 20, Name: "No portrait"}, {Name: "Name only"}, {Role: "Unnamed"}}
			title.Directors = []core.Person{title.Crew[0]}
			title.Writers = []core.Person{{TMDB: 10, Name: "Director", Role: "Writer", ThumbPath: "/known.jpg"}}
		}
		return frontend.ItemDetail{Title: title, Images: []frontend.Image{image}, ImagesAuthoritative: true}, nil
	}
	service, err := identity.NewArtworkImageGrantService(db, "vela", grants, func() frontend.Experience { return exp }, castArtwork{}, time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	i := &instance{host: frontend.Host{Experience: exp, Identity: identityFake{}, ImageGrants: service}}
	r := httptest.NewRequest(http.MethodGet, "/vela/v0/titles/"+ids.RatingKeyGUID(core.MovieKey(42)), nil)
	r.SetPathValue("id", ids.RatingKeyGUID(core.MovieKey(42)))
	r.Header.Set("Authorization", "Bearer "+credential)
	r.Header.Set("X-Vela-Device-Id", "tv")
	w := httptest.NewRecorder()
	i.detail(w, r)
	type credit struct {
		Name, Role string
		Artwork    *struct {
			Kind  string
			Sizes []struct {
				Width, Height int
				URL           string
			}
		}
	}
	// The title and other detail fields have different shapes.
	var envelope map[string]json.RawMessage
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &envelope) != nil {
		t.Fatalf("detail: %d %s", w.Code, w.Body.String())
	}
	var credits []credit
	if err := json.Unmarshal(envelope[field], &credits); err != nil {
		t.Fatal(err)
	}
	wantCount := 2
	if field == "crew" {
		wantCount = 4
	}
	if len(credits) != wantCount || credits[0].Artwork == nil || credits[1].Artwork != nil || exp.batches != 1 {
		t.Fatalf("%s=%+v batches=%d", field, credits, exp.batches)
	}
	if field == "crew" && (credits[0].Name != "Director" || credits[0].Role != "Director" || credits[2].Artwork != nil || credits[3].Role != "Writer" || credits[3].Artwork == nil) {
		t.Fatalf("crew deduplication/portraits=%+v", credits)
	}
	art := credits[0].Artwork
	if art.Kind != "person" || len(art.Sizes) != 1 || art.Sizes[0].Width != 185 || art.Sizes[0].Height != 278 {
		t.Fatalf("artwork=%+v", art)
	}
	u, err := url.Parse(art.Sizes[0].URL)
	if err != nil {
		t.Fatal(err)
	}
	secret := u.Path[len("/vela/v0/images/"):]
	serve := func() int {
		r := httptest.NewRequest(http.MethodGet, u.String(), nil)
		r.SetPathValue("secret", secret)
		w := httptest.NewRecorder()
		i.image(w, r)
		return w.Code
	}
	if code := serve(); code != 200 {
		t.Fatalf("portrait status=%d", code)
	}
	if err := service.Revoke(ctx, otherCredential, secret); err == nil {
		t.Fatal("another viewer revoked portrait")
	}
	if code := serve(); code != 200 {
		t.Fatalf("other viewer affected portrait: %d", code)
	}
	exp.allowed = false
	if code := serve(); code != 404 {
		t.Fatalf("lost credit/policy served portrait: %d", code)
	}
	exp.allowed = true
	if err := service.Revoke(ctx, credential, secret); err != nil {
		t.Fatal(err)
	}
	if code := serve(); code != 404 {
		t.Fatalf("revoked portrait status=%d", code)
	}
}
