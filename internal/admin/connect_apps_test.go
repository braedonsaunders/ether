package admin_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/auth"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/store"
)

type connectFrontends struct{}

func (connectFrontends) List(context.Context) ([]admin.FrontendInfo, error) {
	return []admin.FrontendInfo{{Descriptor: frontend.Descriptor{ID: "stremio"}, Enabled: true}, {Descriptor: frontend.Descriptor{ID: "webdav"}, Enabled: false}}, nil
}
func (connectFrontends) Settings(context.Context, frontend.Protocol) (frontend.Settings, error) {
	return nil, nil
}
func (connectFrontends) PutSettings(context.Context, frontend.Protocol, frontend.Settings) error {
	return nil
}
func (connectFrontends) Toggle(context.Context, frontend.Protocol, bool) error { return nil }

func TestConnectAppsGrantScopeAndAuthorization(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	users, err := auth.New(ctx, db, auth.Options{})
	if err != nil {
		t.Fatal(err)
	}
	local, _ := identity.NewLocal(db)
	ownerID, _ := local.CreateOwner(ctx, "owner", "password")
	viewerID, _ := local.CreateViewer(ctx, "viewer", "password")
	if err := users.RefreshViewers(ctx); err != nil {
		t.Fatal(err)
	}
	grants, _ := identity.NewGrants(db, users, time.Hour, nil)
	portal, _ := grants.Issue(ctx, viewerID, nil, "viewer-portal", core.Device{})
	owner, _ := grants.Issue(ctx, ownerID, nil, "owner", core.Device{})
	h := admin.New(admin.Deps{Config: newStore(t, nil), IdentityGrants: grants, Frontends: connectFrontends{}, ActiveViewer: users.ActiveUser,
		ViewerPortalURL: func(context.Context, *http.Request) (string, error) {
			return "https://ether.example/prefix/ether/me", nil
		},
	})
	do := func(method, path, token string) *http.Response {
		r := req(method, path, "")
		r.Header.Set("Authorization", "Bearer "+token)
		return serve(h, r).Result()
	}
	res := do("GET", "/ether/api/me/apps", portal)
	var listing struct {
		Apps []string `json:"apps"`
	}
	_ = json.NewDecoder(res.Body).Decode(&listing)
	res.Body.Close()
	if res.StatusCode != 200 || len(listing.Apps) != 1 || listing.Apps[0] != "stremio" {
		t.Fatalf("listing: %d %+v", res.StatusCode, listing)
	}
	res = do("POST", "/ether/api/me/apps/stremio", portal)
	var created struct {
		URL string `json:"url"`
		QR  string `json:"qr"`
	}
	_ = json.NewDecoder(res.Body).Decode(&created)
	res.Body.Close()
	if res.StatusCode != 200 || res.Header.Get("Cache-Control") != "no-store" || !strings.HasPrefix(created.URL, "https://ether.example/prefix/stremio/") || !strings.HasPrefix(created.QR, "data:image/png;base64,") {
		t.Fatalf("create: %d %q", res.StatusCode, created.URL)
	}
	secret := strings.TrimSuffix(strings.TrimPrefix(created.URL, "https://ether.example/prefix/stremio/"), "/manifest.json")
	viewer, err := grants.Resolve(ctx, "stremio", secret)
	if err != nil || viewer.ID != viewerID {
		t.Fatalf("grant: %+v %v", viewer, err)
	}
	if _, err := grants.Resolve(ctx, "webdav", secret); err == nil {
		t.Fatal("cross-surface grant accepted")
	}
	devices, err := grants.List(ctx, viewerID)
	if err != nil {
		t.Fatal(err)
	}
	var hash string
	for _, device := range devices {
		if device.Surface == "stremio" {
			hash = device.Hash
		}
	}
	if hash == "" {
		t.Fatal("grant absent from devices")
	}
	if err := grants.RevokeGrant(ctx, viewerID, hash); err != nil {
		t.Fatal(err)
	}
	if _, err := grants.Resolve(ctx, "stremio", secret); err == nil {
		t.Fatal("revoked grant accepted")
	}
	for _, tc := range []struct {
		path, token string
		want        int
	}{
		{"/ether/api/me/apps/webdav", portal, 404},
		{"/ether/api/viewers/2/apps/stremio", portal, 403},
		{"/ether/api/viewers/999/apps/stremio", owner, 404},
	} {
		res = do("POST", tc.path, tc.token)
		res.Body.Close()
		if res.StatusCode != tc.want {
			t.Fatalf("%s: %d", tc.path, res.StatusCode)
		}
	}
	res = do("POST", "/ether/api/viewers/2/apps/stremio", owner)
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("owner create: %d", res.StatusCode)
	}
}
