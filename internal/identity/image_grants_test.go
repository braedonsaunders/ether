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
	image := frontend.Image{Kind: "poster", Path: "/poster.jpg", Tag: "v1"}
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
