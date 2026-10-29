package identity_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	_ "github.com/braedonsaunders/ether/internal/auth"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/store"
)

type imageFixture struct{ calls int }

type scopedArtworkFixture struct {
	calls     int
	request   frontend.ArtworkRequest
	image     frontend.Image
	transform frontend.ArtworkTransform
}

func (f *scopedArtworkFixture) List(context.Context, frontend.ArtworkRequest) ([]frontend.Image, bool, error) {
	return nil, false, nil
}

func (f *scopedArtworkFixture) Read(_ context.Context, request frontend.ArtworkRequest, image frontend.Image, transform frontend.ArtworkTransform) ([]byte, string, error) {
	f.calls++
	f.request, f.image, f.transform = request, image, transform
	return []byte("scoped"), "image/png", nil
}

func (i *imageFixture) Get(context.Context, string, int) ([]byte, string, error) {
	i.calls++
	return []byte("image"), "image/jpeg", nil
}
func TestImageGrantRevalidatesSessionMembershipAndArtwork(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,123,0,0)`); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(100000, 0)
	clock := func() time.Time { return now }
	grants, err := identity.NewGrants(db, members{db}, time.Hour, clock)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := grants.Issue(ctx, 1, nil, "jellyfin", core.Device{ID: "tv"})
	if err != nil {
		t.Fatal(err)
	}
	image := frontend.Image{Kind: "person", Path: "opaque-portrait", Tag: "v1", Subject: "tmdb:287"}
	authoritative := false
	allowed := true
	experience := frontendtest.Experience{ItemFunc: func(_ context.Context, req frontend.ItemRequest) (frontend.ItemDetail, error) {
		if !allowed {
			return frontend.ItemDetail{}, core.ErrUnauthorized
		}
		return frontend.ItemDetail{ImagesAuthoritative: authoritative, Images: []frontend.Image{image}}, nil
	}}
	source := &imageFixture{}
	makeService := func(surface frontend.Protocol) *identity.ImageGrantService {
		s, err := identity.NewImageGrantService(db, surface, grants, func() frontend.Experience { return experience }, source, time.Minute, clock)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	service, other := makeService("jellyfin"), makeService("emby")
	request := frontend.ImageGrantRequest{Credential: credential, Device: core.Device{ID: "tv"}, Item: core.MovieKey(1), Image: image}
	if _, err := service.Issue(ctx, request); !errors.Is(err, core.ErrNotAvailable) {
		t.Fatal("non-authoritative issue", err)
	}
	authoritative = true
	wrong := request
	wrong.Image.Path = "/other.jpg"
	if _, err := service.Issue(ctx, wrong); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatal("unlisted image", err)
	}
	wrong = request
	wrong.Image.Subject = "tmdb:819"
	if _, err := service.Issue(ctx, wrong); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatal("unlisted subject", err)
	}
	secret, err := service.Issue(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if secret == credential {
		t.Fatal("account credential exposed")
	}
	call := func(s *identity.ImageGrantService, token string) int {
		w := httptest.NewRecorder()
		s.Serve(w, httptest.NewRequest("GET", "/?w=300", nil), token)
		return w.Code
	}
	if code := call(other, secret); code != 404 {
		t.Fatal("cross-surface", code)
	}
	if code := call(service, secret); code != 200 {
		t.Fatal("valid grant", code)
	}
	allowed = false
	if code := call(service, secret); code != 404 {
		t.Fatal("policy change ignored", code)
	}
	allowed = true
	image.Tag = "v2"
	if code := call(service, secret); code != 404 {
		t.Fatal("artwork change ignored", code)
	}
	image.Tag = "v1"
	image.Subject = "tmdb:819"
	if code := call(service, secret); code != 404 {
		t.Fatal("credit change ignored", code)
	}
	image.Subject = "tmdb:287"
	now = now.Add(time.Minute)
	if code := call(service, secret); code != 404 {
		t.Fatal("expiry ignored", code)
	}
	now = now.Add(-time.Minute)
	secret, err = service.Issue(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(credential))
	if err := grants.RevokeGrant(ctx, 1, hex.EncodeToString(sum[:])); err != nil {
		t.Fatal(err)
	}
	if code := call(service, secret); code != 404 {
		t.Fatal("source revocation ignored", code)
	}
	if _, err := db.ExecContext(ctx, `UPDATE auth_users SET active=0 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if code := call(service, secret); code != 404 {
		t.Fatal("membership ignored", code)
	}
}

func TestImageGrantUsesScopedArtworkAndTransforms(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,123,0,0)`); err != nil {
		t.Fatal(err)
	}
	grants, err := identity.NewGrants(db, members{db}, time.Hour, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	device := core.Device{ID: "tv", Language: "en"}
	credential, err := grants.Issue(ctx, 1, nil, "jellyfin", device)
	if err != nil {
		t.Fatal(err)
	}
	image := frontend.Image{Kind: frontend.ImagePoster, Path: "opaque-reference", Tag: "v1"}
	allowed := true
	experience := frontendtest.Experience{ItemFunc: func(context.Context, frontend.ItemRequest) (frontend.ItemDetail, error) {
		if !allowed {
			return frontend.ItemDetail{}, core.ErrNotFound
		}
		return frontend.ItemDetail{ImagesAuthoritative: true, Images: []frontend.Image{image}}, nil
	}}
	source := &scopedArtworkFixture{}
	s, err := identity.NewArtworkImageGrantService(db, "jellyfin", grants, func() frontend.Experience { return experience }, source, time.Minute, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := s.Issue(ctx, frontend.ImageGrantRequest{Credential: credential, Device: device, Item: core.MovieKey(42), Image: image})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.Serve(w, httptest.NewRequest("GET", "/?w=300&h=200&format=png&blur=1", nil), secret)
	if w.Code != 200 || w.Body.String() != "scoped" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("response: %d %q", w.Code, w.Body.String())
	}
	if source.calls != 1 || source.request.Viewer.ID != 1 || source.request.Device != device || source.request.Item != core.MovieKey(42) || source.image != image || source.transform != (frontend.ArtworkTransform{Width: 300, Height: 200, Format: "png", Blur: true}) {
		t.Fatalf("bad source request: %+v", source)
	}
	w = httptest.NewRecorder()
	s.Serve(w, httptest.NewRequest("GET", "/?h=200&format=png", nil), secret)
	if w.Code != 200 || source.transform.Width != 0 || source.transform.Height != 200 {
		t.Fatalf("height-only request: %+v", source.transform)
	}
	allowed = false
	w = httptest.NewRecorder()
	s.Serve(w, httptest.NewRequest("GET", "/", nil), secret)
	if w.Code != 404 || source.calls != 2 {
		t.Fatal("hidden artwork reached source", w.Code, source.calls)
	}
}
