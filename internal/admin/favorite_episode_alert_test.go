package admin_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/auth"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/store"
)

type fakeAlertPrefs struct{ enabled map[int64]bool }

func (f *fakeAlertPrefs) Enabled(_ context.Context, u *core.User) (bool, error) {
	return f.enabled[u.ID], nil
}
func (f *fakeAlertPrefs) SetEnabled(_ context.Context, u *core.User, v bool) error {
	f.enabled[u.ID] = v
	return nil
}
func TestFavoriteEpisodeAlertAuthorization(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	users, err := auth.New(ctx, db, auth.Options{})
	if err != nil {
		t.Fatal(err)
	}
	local, _ := identity.NewLocal(db)
	ownerID, err := local.CreateOwner(ctx, "owner", "password-1234")
	if err != nil {
		t.Fatal(err)
	}
	otherID, err := local.CreateViewer(ctx, "viewer", "password-1234")
	if err != nil {
		t.Fatal(err)
	}
	if err = users.RefreshViewers(ctx); err != nil {
		t.Fatal(err)
	}
	grants, _ := identity.NewGrants(db, users, time.Hour, nil)
	owner, _ := grants.Issue(ctx, ownerID, nil, "owner", core.Device{})
	portal, _ := grants.Issue(ctx, otherID, nil, "viewer-portal", core.Device{})
	mirror := &fakeAlertPrefs{enabled: map[int64]bool{}}
	h := admin.New(admin.Deps{Config: newStore(t, nil), IdentityGrants: grants, ViewerAlerts: mirror, ActiveViewer: users.ActiveUser})
	do := func(method, path, token, body string) int {
		r := req(method, path, body)
		r.Header.Set("Authorization", "Bearer "+token)
		return serve(h, r).Code
	}
	if code := do("GET", "/ether/api/me/favorite-episode-alert", owner, ""); code != http.StatusUnauthorized {
		t.Fatal("owner token on portal", code)
	}
	if code := do("POST", "/ether/api/me/favorite-episode-alert", portal, `{"enabled":false}`); code != 200 || mirror.enabled[otherID] {
		t.Fatal(code, mirror.enabled)
	}
	if code := do("POST", "/ether/api/viewers/1/favorite-episode-alert", portal, `{"enabled":false}`); code == 200 {
		t.Fatal("portal token reached owner")
	}
	if code := do("PUT", "/ether/api/viewers/2/favorite-episode-alert", owner, `{"enabled":true}`); code == 200 || mirror.enabled[otherID] {
		t.Fatal(code, mirror.enabled)
	}
	if code := do("POST", "/ether/api/viewers/999/favorite-episode-alert", owner, `{"enabled":true}`); code != 404 {
		t.Fatal(code)
	}
	r := req("POST", "/ether/api/me/favorite-episode-alert", `{"enabled":false}`)
	r.Header.Set("Authorization", "Bearer "+portal)
	r.Header.Del("X-Ether-Request")
	if res := serve(h, r); res.Code != http.StatusForbidden {
		t.Fatal("portal accepted missing CSRF header", res.Code)
	}
	r = req("GET", "/ether/api/viewers/2/favorite-episode-alert", "")
	if res := serve(h, r); res.Code == http.StatusOK {
		t.Fatal("tokenless setup access reached owner-only mirror")
	}
	if _, err := db.ExecContext(ctx, `UPDATE auth_users SET active=0 WHERE id=?`, otherID); err != nil {
		t.Fatal(err)
	}
	if err := users.RefreshViewers(ctx); err != nil {
		t.Fatal(err)
	}
	if code := do("GET", "/ether/api/me/favorite-episode-alert", portal, ""); code != http.StatusUnauthorized {
		t.Fatal("inactive portal viewer", code)
	}
	if code := do("GET", "/ether/api/viewers/2/favorite-episode-alert", owner, ""); code != http.StatusNotFound {
		t.Fatal("inactive target", code)
	}
}
