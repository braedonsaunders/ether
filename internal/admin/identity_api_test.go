package admin_test

import (
	"context"
	"fmt"
	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/auth"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/store"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestLocalOwnerAndPortalIsolation(t *testing.T) {
	db := store.OpenTest(t)
	ctx := context.Background()
	users, e := auth.New(ctx, db, auth.Options{})
	if e != nil {
		t.Fatal(e)
	}
	local, _ := identity.NewLocal(db)
	grants, _ := identity.NewGrants(db, users, time.Hour, nil)
	limiter, _ := identity.NewLoginLimiter(db, time.Minute, 100, 20, nil)
	invites, _ := identity.NewInvite(db, time.Hour, time.Hour, nil)
	cfg := newStore(t, nil)
	h := admin.New(admin.Deps{Config: cfg, LocalIdentity: local, IdentityGrants: grants, IdentityLimiter: limiter, Invites: invites, RefreshViewers: users.RefreshViewers, LocalOwnerConfigured: func(ctx context.Context) bool { x, err := users.Owner(ctx); return err == nil && x.PlexID == 0 }})
	setup := serve(h, req("POST", "/ether/api/setup/local", `{"username":"alice","password":"secret"}`))
	if setup.Code != 200 {
		t.Fatalf("setup: %d %s", setup.Code, setup.Body)
	}
	var v struct {
		Token string `json:"token"`
	}
	decode(t, setup, &v)
	ownerGrant := v.Token
	// Setup must close to unauthenticated LAN clients immediately after local creation.
	r0 := req("GET", "/ether/api/config", "")
	if res := serve(h, r0); res.Code != 401 && res.Code != 403 {
		t.Fatalf("setup left open after owner: %d", res.Code)
	}
	if err := cfg.Update(func(c *config.Config) error {
		c.Frontends = map[string]config.Frontend{"jellyfin": {Enabled: true}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r0 = req("POST", "/ether/api/setup/complete", `{"force":true}`)
	r0.Header.Set("Authorization", "Bearer "+ownerGrant)
	if res := serve(h, r0); res.Code != 200 {
		t.Fatalf("Plex-free setup complete: %d %s", res.Code, res.Body)
	}
	if e = cfg.Update(func(c *config.Config) error { c.SetupComplete = true; return nil }); e != nil {
		t.Fatal(e)
	}
	r := req("GET", "/ether/api/config", "")
	r.Header.Set("X-Plex-Token", v.Token)
	if res := serve(h, r); res.Code != 200 {
		t.Fatalf("owner rejected: %d", res.Code)
	}
	r = req("GET", "/ether/api/me", "")
	r.Header.Set("Authorization", "Bearer "+v.Token)
	if res := serve(h, r); res.Code != http.StatusUnauthorized {
		t.Fatalf("owner grant on portal: %d", res.Code)
	}
	login := serve(h, req("POST", "/ether/api/me/login", `{"username":"alice","password":"secret"}`))
	if login.Code != 200 {
		t.Fatalf("login: %d %s", login.Code, login.Body)
	}
	decode(t, login, &v)
	r = req("GET", "/ether/api/config", "")
	r.Header.Set("Authorization", "Bearer "+v.Token)
	if res := serve(h, r); res.Code != http.StatusUnauthorized {
		t.Fatalf("portal grant on owner: %d", res.Code)
	}
	r = req("GET", "/ether/api/me", "")
	r.Header.Set("Authorization", "Bearer "+v.Token)
	if res := serve(h, r); res.Code != 200 {
		t.Fatalf("portal rejected: %d", res.Code)
	}
	r = req("GET", "/ether/api/me/devices", "")
	r.Header.Set("Authorization", "Bearer "+v.Token)
	if res := serve(h, r); res.Code != 200 {
		t.Fatalf("devices: %d", res.Code)
	}
	r = req("GET", "/ether/api/me/identities", "")
	r.Header.Set("Authorization", "Bearer "+v.Token)
	if res := serve(h, r); res.Code != 200 {
		t.Fatalf("identities: %d", res.Code)
	}

	r = req("POST", "/ether/api/me/invite", `{}`)
	r.Header.Set("Authorization", "Bearer "+v.Token)
	if res := serve(h, r); res.Code == 200 {
		t.Fatal("owner invite accepted")
	}
	_, e = db.Exec(`INSERT INTO auth_users(id,plex_id,username,created_at,updated_at) VALUES(22,NULL,'viewer',0,0)`)
	if e != nil {
		t.Fatal(e)
	}
	if e = users.RefreshViewers(ctx); e != nil {
		t.Fatal(e)
	}
	r = req("POST", "/ether/api/users/22/invite", `{}`)
	r.Header.Set("Authorization", "Bearer "+ownerGrant)
	res := serve(h, r)
	if res.Code != 200 {
		t.Fatalf("invite creation: %d %s", res.Code, res.Body)
	}
	var invite struct {
		URL string `json:"url"`
	}
	decode(t, res, &invite)
	secret := strings.TrimPrefix(invite.URL, "/ether/me#invite=")
	if secret == invite.URL {
		t.Fatal("missing fragment")
	}
	r = req("POST", "/ether/api/me/invite/redeem", `{"secret":"`+secret+`"}`)
	res = serve(h, r)
	if res.Code != 200 {
		t.Fatalf("invite redeem: %d %s", res.Code, res.Body)
	}
	decode(t, res, &v)
	r = req("GET", "/ether/api/config", "")
	r.Header.Set("Authorization", "Bearer "+v.Token)
	if res = serve(h, r); res.Code != 401 {
		t.Fatalf("invite reached owner: %d", res.Code)
	}
	r = req("POST", "/ether/api/me/invite/redeem", `{"secret":"`+secret+`"}`)
	if res = serve(h, r); res.Code != 401 {
		t.Fatalf("invite reuse: %d", res.Code)
	}
	if _, err := users.Authenticate(ctx, v.Token); err != core.ErrUnauthorized {
		t.Fatalf("Plex accepted portal: %v", err)
	}
}

func TestPortalPlexLinkCannotAttachAnotherViewer(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	u, err := auth.New(ctx, db, auth.Options{})
	if err != nil {
		t.Fatal(err)
	}
	local, _ := identity.NewLocal(db)
	id, err := local.CreateOwner(ctx, "alice", "secret")
	if err != nil {
		t.Fatal(err)
	}
	if err = u.RefreshViewers(ctx); err != nil {
		t.Fatal(err)
	}
	grants, _ := identity.NewGrants(db, u, time.Hour, nil)
	limiter, _ := identity.NewLoginLimiter(db, time.Minute, 100, 100, nil)
	portal, err := grants.Issue(ctx, id, nil, "viewer-portal", core.Device{})
	if err != nil {
		t.Fatal(err)
	}
	si := &fakeSignIn{tokens: map[string]string{}}
	cfg := newStore(t, func(c *config.Config) { c.SetupComplete = true })
	pollLimiter, _ := identity.NewPollLimiter(db, time.Minute, 120, 60, nil)
	h := admin.New(admin.Deps{Config: cfg, LocalIdentity: local, IdentityGrants: grants, IdentityLimiter: limiter, PollLimiter: pollLimiter, SignIn: si, LinkPlex: func(context.Context, int64, string) error { return core.ErrUnauthorized }})
	r := req("POST", "/ether/api/me/link/plex", `{}`)
	r.Header.Set("Authorization", "Bearer "+portal)
	res := serve(h, r)
	if res.Code != 200 {
		t.Fatalf("start %d %s", res.Code, res.Body)
	}
	var p struct {
		Handle string `json:"handle"`
	}
	decode(t, res, &p)
	si.approve("4242", "other-token")
	r = req("GET", "/ether/api/me/link/plex/"+p.Handle, "")
	r.Header.Set("Authorization", "Bearer "+portal)
	res = serve(h, r)
	if res.Code != 403 {
		t.Fatalf("linked another viewer: %d %s", res.Code, res.Body)
	}
	links, err := local.Links(ctx, id)
	if err != nil || len(links) != 1 {
		t.Fatalf("links %+v: %v", links, err)
	}
}

func TestOwnerManagesLocalMembership(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	users, err := auth.New(ctx, db, auth.Options{})
	if err != nil {
		t.Fatal(err)
	}
	local, _ := identity.NewLocal(db)
	owner, err := local.CreateOwner(ctx, "owner", "secret")
	if err != nil {
		t.Fatal(err)
	}
	if err = users.RefreshViewers(ctx); err != nil {
		t.Fatal(err)
	}
	grants, _ := identity.NewGrants(db, users, time.Hour, nil)
	limiter, _ := identity.NewLoginLimiter(db, time.Minute, 100, 100, nil)
	ownerToken, err := grants.Issue(ctx, owner, nil, "owner", core.Device{})
	if err != nil {
		t.Fatal(err)
	}
	cfg := newStore(t, func(c *config.Config) { c.SetupComplete = true })
	h := admin.New(admin.Deps{Config: cfg, LocalIdentity: local, IdentityGrants: grants, IdentityLimiter: limiter, RefreshViewers: users.RefreshViewers, SetViewerActive: users.SetOwnerEnabled})
	r := req("POST", "/ether/api/users/local", `{"username":"bob","password":"secret"}`)
	r.Header.Set("Authorization", "Bearer "+ownerToken)
	res := serve(h, r)
	if res.Code != 201 {
		t.Fatalf("create %d %s", res.Code, res.Body)
	}
	var created struct {
		ID int64 `json:"id"`
	}
	decode(t, res, &created)
	token, err := grants.Issue(ctx, created.ID, nil, "jellyfin", core.Device{})
	if err != nil {
		t.Fatal(err)
	}
	r = req("PATCH", fmt.Sprintf("/ether/api/users/%d/membership", created.ID), `{"active":false}`)
	r.Header.Set("Authorization", "Bearer "+ownerToken)
	if res = serve(h, r); res.Code != 200 {
		t.Fatalf("deactivate: %d %s", res.Code, res.Body)
	}
	if _, err = grants.Resolve(ctx, "jellyfin", token); err == nil {
		t.Fatal("inactive grant accepted")
	}
	r = req("PATCH", fmt.Sprintf("/ether/api/users/%d/membership", created.ID), `{"active":true}`)
	r.Header.Set("Authorization", "Bearer "+ownerToken)
	if res = serve(h, r); res.Code != 200 {
		t.Fatalf("reactivate: %d %s", res.Code, res.Body)
	}
	if _, err = grants.Resolve(ctx, "jellyfin", token); err != nil {
		t.Fatal(err)
	}
	r = req("PATCH", fmt.Sprintf("/ether/api/users/%d/membership", owner), `{"active":false}`)
	r.Header.Set("Authorization", "Bearer "+ownerToken)
	if res = serve(h, r); res.Code != 403 {
		t.Fatalf("owner deactivated: %d", res.Code)
	}
}

func TestPlexFreeSetupRequiresOwnerEvenForced(t *testing.T) {
	cfg := newStore(t, func(c *config.Config) { c.Frontends = map[string]config.Frontend{"jellyfin": {Enabled: true}} })
	h := admin.New(admin.Deps{Config: cfg})
	rec := serve(h, req("POST", "/ether/api/setup/complete", `{"force":true}`))
	if rec.Code != 409 {
		t.Fatalf("forced setup without owner: %d %s", rec.Code, rec.Body)
	}
	if cfg.Get().SetupComplete {
		t.Fatal("setup completed without owner")
	}
}
func TestGrantNeverFallsBackToPlexOwner(t *testing.T) {
	cfg := newStore(t, func(c *config.Config) { c.SetupComplete = true })
	h := admin.New(admin.Deps{Config: cfg, IsOwnerToken: func(context.Context, string) bool { return true }})
	r := req("GET", "/ether/api/config", "")
	r.Header.Set("Authorization", "Bearer eg1_invalid")
	if res := serve(h, r); res.Code != 401 {
		t.Fatalf("grant accepted by permissive Plex validator: %d", res.Code)
	}
}
