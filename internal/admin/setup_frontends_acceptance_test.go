package admin_test

// First-install acceptance for one setup wizard: app selection, a local
// owner with Plex disabled, the real setup gates, and a rerun that keeps
// stored config. Routes are the admin handler. State is a temporary config
// directory plus the migrated SQLite database from store.OpenTest.
//
// app.frontendControl is unexported and also mounts protocol listeners, so
// this file cannot call it without starting those listeners. configFrontends
// persists through the same config.Store fields that control writes
// (frontends.enabled, nested frontends.settings, and node-local
// frontendPorts) and refuses to mount a listener. Live plex.tv claim,
// Usenet, indexer, and TMDB probes are upstream checks. They are not
// invoked. offlinePlexAuth fails closed if claim or PIN sign-in is called.
// SetupStatus itself only inspects stored config, which is what these
// tests satisfy through the config section routes.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/auth"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/tmdb"
)

const (
	setupOwnerUser = "household-owner"
	setupOwnerPass = "local-owner-pass"
	fixtureUsenet  = "fixture-usenet-password"
	fixtureIndexer = "fixture-indexer-key"
	fixtureTMDB    = "fixture-tmdb-key"
	fixturePlex    = "fixture-plex-user-token"
)

func TestSetupFrontendsAcceptanceFreshInstall(t *testing.T) {
	if tmdb.BundledKey != "" {
		t.Fatal("catalog gate is not observable: this binary has a bundled TMDB key")
	}
	ctx := context.Background()
	dir := t.TempDir()
	cfg := openConfig(t, dir)
	db := store.OpenTest(t)
	users := newUsers(t, ctx, db)
	local := newLocal(t, db)
	grants := newGrants(t, db, users)
	limiter := newLimiter(t, db)
	frontends := newConfigFrontends(t, cfg)
	h := newSetupHandler(cfg, users, local, grants, limiter, frontends)

	boot := mustBootstrap(t, h, "")
	// Tokenless LAN setup is admitted until a local owner exists, so
	// authenticated is true while authRequired is still false.
	if boot.SetupComplete || boot.AuthRequired || !boot.Authenticated || boot.OwnerConfigured || len(boot.SignInMethods) != 1 || boot.SignInMethods[0] != "local" {
		t.Fatalf("fresh bootstrap: %+v", boot)
	}
	open := mustStatus(t, h, "")
	if open.SetupComplete || open.RequiredComplete || step(t, open, "plex").Title != "Sign in to plex.tv and claim" || step(t, open, "plex").Complete {
		t.Fatalf("fresh plex gate: complete=%v required=%v plex=%+v", open.SetupComplete, open.RequiredComplete, step(t, open, "plex"))
	}
	if cfg.Get().Plex.Claimed || cfg.Get().Plex.Token != "" {
		t.Fatal("fresh install stored a Plex claim")
	}

	putJSON(t, h, http.MethodPut, "/ether/api/frontends/plex/enabled", `{"enabled":false}`, "", http.StatusOK)
	putJSON(t, h, http.MethodPut, "/ether/api/frontends/jellyfin/enabled", `{"enabled":true}`, "", http.StatusOK)
	putJSON(t, h, http.MethodPut, "/ether/api/frontends/emby/enabled", `{"enabled":true}`, "", http.StatusOK)
	putJSON(t, h, http.MethodPut, "/ether/api/frontends/jellyfin/settings", `{"port":8096,"serverName":"Living Room","discovery":true}`, "", http.StatusOK)
	putJSON(t, h, http.MethodPut, "/ether/api/frontends/emby/settings", `{"port":8097,"serverName":"Downstairs","quickConnect":true}`, "", http.StatusOK)
	putJSON(t, h, http.MethodPut, "/ether/api/frontends/missing/enabled", `{"enabled":true}`, "", http.StatusBadRequest)

	cfg = openConfig(t, dir)
	assertSelectedFrontends(t, cfg.Get(), "Living Room", "Downstairs")
	h = newSetupHandler(cfg, users, local, grants, limiter, newConfigFrontends(t, cfg))
	reloadedSettings := putJSON(t, h, http.MethodGet, "/ether/api/frontends/jellyfin/settings", "", "", http.StatusOK)
	var jellyfin map[string]any
	decode(t, reloadedSettings, &jellyfin)
	if jellyfin["serverName"] != "Living Room" || jellyfin["discovery"] != true || jellyfin["port"] != float64(8096) {
		t.Fatal("reloaded Jellyfin settings route did not return nested settings")
	}
	again := mustStatus(t, h, "")
	plexStep := step(t, again, "plex")
	if again.SetupComplete || plexStep.Title != "Create local owner" || plexStep.Complete || !plexStep.Required {
		t.Fatalf("disabled Plex still imposed a claim: title=%s complete=%v required=%v", plexStep.Title, plexStep.Complete, plexStep.Required)
	}

	env := getConfig(t, h, "")
	env.SetupComplete = true
	env.Server.FriendlyName = "Ether House"
	raw, err := json.Marshal(map[string]any{"config": env})
	if err != nil {
		t.Fatal(err)
	}
	putJSON(t, h, http.MethodPut, "/ether/api/config", string(raw), "", http.StatusOK)
	if cfg.Get().SetupComplete || cfg.Get().Server.FriendlyName != "Ether House" {
		t.Fatal("config write marked setup complete or dropped the server name")
	}
	assertSelectedFrontends(t, cfg.Get(), "Living Room", "Downstairs")

	if err := cfg.Update(func(c *config.Config) error {
		c.Cache.Dir = ""
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	gated := mustStatus(t, h, "")
	for _, id := range []string{"plex", "sources", "indexers", "cache", "catalog"} {
		if step(t, gated, id).Complete {
			t.Fatalf("step %s complete before it was satisfied", id)
		}
	}
	putJSON(t, h, http.MethodPut, "/ether/api/config/cache", `{"dir":""}`, "", http.StatusBadRequest)
	if cfg.Get().Cache.Dir != "" {
		t.Fatal("rejected cache write stored a directory")
	}
	cacheDir := t.TempDir()
	cacheBody, err := json.Marshal(map[string]any{"dir": cacheDir, "budgetBytes": 52428800})
	if err != nil {
		t.Fatal(err)
	}
	putJSON(t, h, http.MethodPut, "/ether/api/config/cache", string(cacheBody), "", http.StatusOK)
	if !step(t, mustStatus(t, h, ""), "cache").Complete || cfg.Get().Cache.Dir != cacheDir {
		t.Fatal("cache directory saved through the admin API did not satisfy the cache gate")
	}
	mustConflict(t, h, `{}`, "", "plex_required")
	mustConflict(t, h, `{"force":true}`, "", "plex_required")
	if cfg.Get().SetupComplete || cfg.Get().Plex.Claimed {
		t.Fatal("refused completion still closed setup or claimed Plex")
	}

	created := tokenFrom(t, putJSON(t, h, http.MethodPost, "/ether/api/setup/local", `{"username":"`+setupOwnerUser+`","password":"`+setupOwnerPass+`"}`, "", http.StatusOK))
	assertAdminGrant(t, ctx, users, db, h, created)
	if !localOwnerReady(users, local)(ctx) {
		t.Fatal("local owner was not visible after setup")
	}
	for _, path := range []string{"/ether/api/config", "/ether/api/setup/status", "/ether/api/frontends", "/ether/api/frontends/jellyfin/settings"} {
		putJSON(t, h, http.MethodGet, path, "", "", http.StatusUnauthorized)
	}
	remote := req(http.MethodGet, "/ether/api/setup/status", "")
	remote.RemoteAddr = "203.0.113.9:1"
	remote.Host = "setup.example"
	if rec := serve(h, remote); rec.Code != http.StatusUnauthorized {
		t.Fatalf("remote tokenless setup: %d %s", rec.Code, apiCode(rec))
	}
	putJSON(t, h, http.MethodPost, "/ether/api/setup/local", `{"username":"other","password":"`+setupOwnerPass+`"}`, "", http.StatusForbidden)

	signedIn := tokenFrom(t, putJSON(t, h, http.MethodPost, "/ether/api/auth/local", `{"username":"`+setupOwnerUser+`","password":"`+setupOwnerPass+`"}`, "", http.StatusOK))
	if signedIn == created {
		t.Fatal("local sign-in reused the setup grant")
	}
	assertAdminGrant(t, ctx, users, db, h, signedIn)
	ownerBoot := mustBootstrap(t, h, signedIn)
	if !ownerBoot.AuthRequired || !ownerBoot.Authenticated || ownerBoot.SetupComplete || len(ownerBoot.SignInMethods) != 1 || ownerBoot.SignInMethods[0] != "local" {
		t.Fatalf("owner bootstrap: %+v", ownerBoot)
	}
	owned := mustStatus(t, h, signedIn)
	if owned.SetupComplete || owned.RequiredComplete || !step(t, owned, "plex").Complete || step(t, owned, "plex").Detail != "created" {
		t.Fatalf("owner did not replace the Plex claim: complete=%v required=%v plex=%+v", owned.SetupComplete, owned.RequiredComplete, step(t, owned, "plex"))
	}
	mustConflict(t, h, `{}`, signedIn, "setup_incomplete")
	if cfg.Get().SetupComplete {
		t.Fatal("incomplete setup was stored complete")
	}

	portal := tokenFrom(t, putJSON(t, h, http.MethodPost, "/ether/api/me/login", `{"username":"`+setupOwnerUser+`","password":"`+setupOwnerPass+`"}`, "", http.StatusOK))
	putJSON(t, h, http.MethodGet, "/ether/api/config", "", portal, http.StatusUnauthorized)
	putJSON(t, h, http.MethodGet, "/ether/api/me", "", signedIn, http.StatusUnauthorized)
	if _, err := users.Authenticate(ctx, portal); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatal("portal grant authenticated as Plex/playback")
	}

	putJSON(t, h, http.MethodPut, "/ether/api/config/usenet", `{"providers":[{"id":"p1","name":"Main","host":"news.example.invalid","port":563,"tls":true,"username":"u1","password":"`+fixtureUsenet+`","connections":8,"enabled":true}]}`, signedIn, http.StatusOK)
	afterSources := mustStatus(t, h, signedIn)
	if !step(t, afterSources, "sources").Complete || afterSources.SetupComplete || step(t, afterSources, "indexers").Complete {
		t.Fatal("provider gate did not advance on its own")
	}
	putJSON(t, h, http.MethodPut, "/ether/api/config/indexers", `[{"id":"i1","name":"Idx","url":"https://indexer.example.invalid","apiKey":"`+fixtureIndexer+`","enabled":true}]`, signedIn, http.StatusOK)
	afterIndexers := mustStatus(t, h, signedIn)
	if !step(t, afterIndexers, "indexers").Complete || afterIndexers.SetupComplete || step(t, afterIndexers, "catalog").Complete {
		t.Fatal("indexer gate did not advance on its own")
	}
	putJSON(t, h, http.MethodPut, "/ether/api/config/catalog", `{"tmdbKey":"`+fixtureTMDB+`"}`, signedIn, http.StatusOK)
	afterCatalog := mustStatus(t, h, signedIn)
	if !step(t, afterCatalog, "catalog").Complete || !step(t, afterCatalog, "cache").Complete || afterCatalog.SetupComplete || !afterCatalog.RequiredComplete {
		t.Fatal("satisfied provider, indexer, catalog, and cache checks still did not leave setup unfinished")
	}
	ready := afterCatalog
	if !ready.RequiredComplete || ready.SetupComplete {
		t.Fatalf("satisfied gates: required=%v setupComplete=%v", ready.RequiredComplete, ready.SetupComplete)
	}
	for _, id := range []string{"plex", "sources", "indexers", "cache", "catalog"} {
		if !step(t, ready, id).Complete {
			t.Fatalf("step %s still incomplete after it was stored", id)
		}
	}
	finished := mustStatusBody(t, putJSON(t, h, http.MethodPost, "/ether/api/setup/complete", `{}`, signedIn, http.StatusOK))
	if !finished.SetupComplete || !cfg.Get().SetupComplete || cfg.Get().Plex.Claimed || cfg.Get().Plex.Token != "" {
		t.Fatal("finished setup without a stored completion or stored a Plex claim")
	}
	putJSON(t, h, http.MethodGet, "/ether/api/config", "", "", http.StatusUnauthorized)

	reopened := mustStatusBody(t, putJSON(t, h, http.MethodPost, "/ether/api/setup/reopen", `{}`, signedIn, http.StatusOK))
	if !reopened.SetupComplete || !cfg.Get().SetupComplete {
		t.Fatal("rerun cleared setup completion")
	}
	putJSON(t, h, http.MethodPut, "/ether/api/config/server", `{"friendlyName":"Ether House Again"}`, signedIn, http.StatusOK)
	putJSON(t, h, http.MethodPut, "/ether/api/frontends/jellyfin/settings", `{"port":8096,"serverName":"Renamed","discovery":true}`, signedIn, http.StatusOK)
	putJSON(t, h, http.MethodPost, "/ether/api/setup/reopen", `{}`, signedIn, http.StatusOK)
	putJSON(t, h, http.MethodGet, "/ether/api/setup/status", "", "", http.StatusUnauthorized)
	kept := openConfig(t, dir).Get()
	if !kept.SetupComplete || kept.Server.FriendlyName != "Ether House Again" || kept.Plex.Claimed || kept.Plex.Token != "" {
		t.Fatal("reloaded config lost setup state or gained a Plex claim")
	}
	assertSelectedFrontends(t, kept, "Renamed", "Downstairs")
	if kept.Usenet.Providers[0].Password != fixtureUsenet || kept.Indexers[0].APIKey != fixtureIndexer || kept.Catalog.TMDBKey != fixtureTMDB || kept.Cache.Dir != cacheDir {
		t.Fatal("rerun did not preserve provider, indexer, catalog, or cache settings")
	}
	if body := putJSON(t, h, http.MethodGet, "/ether/api/config", "", signedIn, http.StatusOK).Body.String(); strings.Contains(body, fixtureUsenet) || strings.Contains(body, fixtureIndexer) || strings.Contains(body, fixtureTMDB) {
		t.Fatal("admin config response exposed stored credentials")
	}
	againSignedIn := tokenFrom(t, putJSON(t, h, http.MethodPost, "/ether/api/auth/local", `{"username":"`+setupOwnerUser+`","password":"`+setupOwnerPass+`"}`, "", http.StatusOK))
	assertAdminGrant(t, ctx, users, db, h, againSignedIn)
	putJSON(t, h, http.MethodGet, "/ether/api/setup/status", "", againSignedIn, http.StatusOK)
	putJSON(t, h, http.MethodGet, "/ether/api/config", "", portal, http.StatusUnauthorized)
}

func TestSetupFrontendsAcceptanceEnabledPlexRequiresClaim(t *testing.T) {
	if tmdb.BundledKey != "" {
		t.Fatal("catalog gate is not observable: this binary has a bundled TMDB key")
	}
	ctx := context.Background()
	dir := t.TempDir()
	cfg := openConfig(t, dir)
	db := store.OpenTest(t)
	users := newUsers(t, ctx, db)
	local := newLocal(t, db)
	grants := newGrants(t, db, users)
	limiter := newLimiter(t, db)
	h := newSetupHandler(cfg, users, local, grants, limiter, newConfigFrontends(t, cfg))

	putJSON(t, h, http.MethodPut, "/ether/api/frontends/plex/enabled", `{"enabled":true}`, "", http.StatusOK)
	putJSON(t, h, http.MethodPut, "/ether/api/frontends/jellyfin/enabled", `{"enabled":true}`, "", http.StatusOK)
	cacheDir := t.TempDir()
	cacheBody, err := json.Marshal(map[string]any{"dir": cacheDir, "budgetBytes": 52428800})
	if err != nil {
		t.Fatal(err)
	}
	putJSON(t, h, http.MethodPut, "/ether/api/config/usenet", `{"providers":[{"id":"p1","host":"news.example.invalid","port":563,"connections":4,"enabled":true}]}`, "", http.StatusOK)
	putJSON(t, h, http.MethodPut, "/ether/api/config/indexers", `[{"id":"i1","url":"https://indexer.example.invalid","apiKey":"`+fixtureIndexer+`","enabled":true}]`, "", http.StatusOK)
	putJSON(t, h, http.MethodPut, "/ether/api/config/catalog", `{"tmdbKey":"`+fixtureTMDB+`"}`, "", http.StatusOK)
	putJSON(t, h, http.MethodPut, "/ether/api/config/cache", string(cacheBody), "", http.StatusOK)
	ready := mustStatus(t, h, "")
	plexStep := step(t, ready, "plex")
	if ready.SetupComplete || plexStep.Title != "Sign in to plex.tv and claim" || plexStep.Complete || !step(t, ready, "sources").Complete {
		t.Fatalf("enabled Plex was not the remaining gate: %+v", plexStep)
	}
	mustConflict(t, h, `{}`, "", "plex_required")
	mustConflict(t, h, `{"force":true}`, "", "plex_required")

	if err := cfg.Update(func(c *config.Config) error {
		c.Plex.Token = fixturePlex
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	signedIn := mustStatus(t, h, "")
	if step(t, signedIn, "plex").Detail != "signed in, not claimed" || step(t, signedIn, "plex").Complete || cfg.Get().Plex.Claimed {
		t.Fatalf("token without claim counted as claimed: %+v", step(t, signedIn, "plex"))
	}
	mustConflict(t, h, `{"force":true}`, "", "plex_required")
	putJSON(t, h, http.MethodPost, "/ether/api/plex/claim", `{}`, "", http.StatusBadGateway)
	if cfg.Get().SetupComplete || cfg.Get().Plex.Claimed || cfg.Get().Plex.Token != fixturePlex {
		t.Fatal("offline claim changed Plex state or closed setup")
	}
	if body := putJSON(t, h, http.MethodGet, "/ether/api/config", "", "", http.StatusOK).Body.String(); strings.Contains(body, fixturePlex) || strings.Contains(body, fixtureIndexer) || strings.Contains(body, fixtureTMDB) {
		t.Fatal("admin config response exposed stored credentials")
	}
}

func TestSetupFrontendsAcceptanceAdminSessionIsNotPlayback(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	users := newUsers(t, ctx, db)
	if err := users.BootstrapOwner(ctx, config.Plex{OwnerPlexID: 42, OwnerUsername: "owner", Token: "server-token"}); err != nil {
		t.Fatal(err)
	}
	session, _, err := users.IssueOwnerSession(ctx, "server-token")
	if err != nil {
		t.Fatal(err)
	}
	if !auth.IsOwnerSession(session) || strings.Contains(session, "server-token") {
		t.Fatal("console session was not an auth_sessions token independent of the Plex token")
	}
	var stored string
	if err := db.QueryRow(`SELECT token_hash FROM auth_sessions`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != auth.HashToken(session) || strings.Contains(stored, session) {
		t.Fatal("auth_sessions stored the console token in the clear")
	}
	if _, err := users.Authenticate(ctx, session); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatal("auth_sessions token authenticated as Plex/playback")
	}
	if u, err := users.Authenticate(ctx, "server-token"); err != nil || !u.IsOwner {
		t.Fatal("fixture Plex owner token was not a playback credential")
	}
	cfg := openConfig(t, t.TempDir())
	h := admin.New(admin.Deps{
		Config:       cfg,
		IsOwnerToken: productionOwnerToken(users),
		Sessions:     users,
		Logger:       quietLog(),
	})
	putJSON(t, h, http.MethodGet, "/ether/api/config", "", session, http.StatusOK)
	if _, err := users.Authenticate(ctx, session); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatal("admin use of the console session made it a playback credential")
	}
}

type setupView struct {
	SetupComplete    bool              `json:"setupComplete"`
	RequiredComplete bool              `json:"requiredComplete"`
	Steps            []admin.SetupStep `json:"steps"`
}

type bootstrapView struct {
	SetupComplete   bool     `json:"setupComplete"`
	AuthRequired    bool     `json:"authRequired"`
	Authenticated   bool     `json:"authenticated"`
	OwnerConfigured bool     `json:"ownerConfigured"`
	SignInMethods   []string `json:"signInMethods"`
}

func openConfig(t *testing.T, dir string) *config.Store {
	t.Helper()
	cfg, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func newUsers(t *testing.T, ctx context.Context, db *sql.DB) *auth.Service {
	t.Helper()
	users, err := auth.New(ctx, db, auth.Options{Logger: quietLog()})
	if err != nil {
		t.Fatal(err)
	}
	return users
}

func newLocal(t *testing.T, db *sql.DB) *identity.Local {
	t.Helper()
	local, err := identity.NewLocal(db)
	if err != nil {
		t.Fatal(err)
	}
	return local
}

func newGrants(t *testing.T, db *sql.DB, users *auth.Service) *identity.Grants {
	t.Helper()
	grants, err := identity.NewGrants(db, users, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	return grants
}

func newLimiter(t *testing.T, db *sql.DB) *identity.LoginLimiter {
	t.Helper()
	limiter, err := identity.NewLoginLimiter(db, time.Minute, 100, 20, nil)
	if err != nil {
		t.Fatal(err)
	}
	return limiter
}

func newSetupHandler(cfg *config.Store, users *auth.Service, local *identity.Local, grants *identity.Grants, limiter *identity.LoginLimiter, frontends admin.FrontendAdmin) http.Handler {
	return admin.New(admin.Deps{
		Config:               cfg,
		LocalIdentity:        local,
		IdentityGrants:       grants,
		IdentityLimiter:      limiter,
		RefreshViewers:       users.RefreshViewers,
		OwnerConfigured:      func(ctx context.Context) bool { _, err := users.Owner(ctx); return err == nil },
		LocalOwnerConfigured: localOwnerReady(users, local),
		IsOwnerToken:         productionOwnerToken(users),
		Sessions:             users,
		AuthenticatePlex:     users.Authenticate,
		PlexAuth:             offlinePlexAuth{},
		Frontends:            frontends,
		Logger:               quietLog(),
	})
}

// productionOwnerToken matches internal/app/admin.go: an auth_sessions
// console token is checked with OwnerSession, and every other credential
// goes through Authenticate. Authenticate rejects both console sessions and
// eg1_ grants before any plex.tv validation.
func productionOwnerToken(users *auth.Service) func(context.Context, string) bool {
	return func(ctx context.Context, token string) bool {
		if auth.IsOwnerSession(token) {
			u, err := users.OwnerSession(ctx, token)
			return err == nil && u.IsOwner
		}
		u, err := users.Authenticate(ctx, token)
		return err == nil && u.IsOwner
	}
}

// localOwnerReady matches the app wiring: a local identity link on the
// owner closes tokenless setup. A Plex id of zero is not itself enough.
func localOwnerReady(users *auth.Service, local *identity.Local) func(context.Context) bool {
	return func(ctx context.Context) bool {
		owner, err := users.Owner(ctx)
		if err != nil {
			return false
		}
		links, err := local.Links(ctx, owner.ID)
		if err != nil {
			return true
		}
		for _, link := range links {
			if link.Provider == "local" {
				return true
			}
		}
		return false
	}
}

func assertAdminGrant(t *testing.T, ctx context.Context, users *auth.Service, db *sql.DB, h http.Handler, token string) {
	t.Helper()
	if !core.IsGrantCredential(token) || auth.IsOwnerSession(token) {
		t.Fatal("local admin credential was not an identity grant")
	}
	if _, err := users.Authenticate(ctx, token); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatal("admin grant authenticated as Plex/playback")
	}
	putJSON(t, h, http.MethodPost, "/ether/api/auth/session", `{}`, token, http.StatusUnauthorized)
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM auth_sessions`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("local owner sign-in wrote an auth_sessions row")
	}
}

func assertSelectedFrontends(t *testing.T, cfg config.Config, jellyfinName, embyName string) {
	t.Helper()
	if cfg.Frontends["plex"].Enabled || !cfg.Frontends["jellyfin"].Enabled || !cfg.Frontends["emby"].Enabled {
		t.Fatal("reloaded app selection was not Jellyfin and Emby with Plex disabled")
	}
	if cfg.Frontends["jellyfin"].Settings["serverName"] != jellyfinName || cfg.Frontends["jellyfin"].Settings["discovery"] != true {
		t.Fatal("reloaded Jellyfin settings were not nested under the frontend")
	}
	if cfg.FrontendPorts["jellyfin"] != 8096 {
		t.Fatal("reloaded Jellyfin port was not stored as a node-local port")
	}
	if cfg.Frontends["emby"].Settings["serverName"] != embyName || cfg.Frontends["emby"].Settings["quickConnect"] != true || cfg.FrontendPorts["emby"] != 8097 {
		t.Fatal("reloaded Emby settings were not preserved")
	}
}

func mustBootstrap(t *testing.T, h http.Handler, token string) bootstrapView {
	t.Helper()
	rec := putJSON(t, h, http.MethodGet, "/ether/api/bootstrap", "", token, http.StatusOK)
	var boot bootstrapView
	decode(t, rec, &boot)
	return boot
}

func mustStatus(t *testing.T, h http.Handler, token string) setupView {
	t.Helper()
	return mustStatusBody(t, putJSON(t, h, http.MethodGet, "/ether/api/setup/status", "", token, http.StatusOK))
}

func mustStatusBody(t *testing.T, rec *httptest.ResponseRecorder) setupView {
	t.Helper()
	var st setupView
	decode(t, rec, &st)
	return st
}

func step(t *testing.T, st setupView, id string) admin.SetupStep {
	t.Helper()
	for _, s := range st.Steps {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("missing setup step %s", id)
	return admin.SetupStep{}
}

func mustConflict(t *testing.T, h http.Handler, body, token, code string) {
	t.Helper()
	rec := putJSON(t, h, http.MethodPost, "/ether/api/setup/complete", body, token, http.StatusConflict)
	if apiCode(rec) != code {
		t.Fatalf("setup complete code %s", apiCode(rec))
	}
}

func getConfig(t *testing.T, h http.Handler, token string) config.Config {
	t.Helper()
	rec := putJSON(t, h, http.MethodGet, "/ether/api/config", "", token, http.StatusOK)
	var env struct {
		Config config.Config `json:"config"`
	}
	decode(t, rec, &env)
	return env.Config
}

func putJSON(t *testing.T, h http.Handler, method, path, body, token string, want int) *httptest.ResponseRecorder {
	t.Helper()
	r := req(method, path, body)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	rec := serve(h, r)
	if rec.Code != want {
		t.Fatalf("%s %s: %d %s", method, path, rec.Code, apiCode(rec))
	}
	return rec
}

func tokenFrom(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Token string `json:"token"`
	}
	decode(t, rec, &body)
	if body.Token == "" {
		t.Fatal("response did not include a token")
	}
	return body.Token
}

func apiCode(rec *httptest.ResponseRecorder) string {
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return body.Error.Code
}

type offlinePlexAuth struct{}

func (offlinePlexAuth) StartPIN(context.Context) (string, string, string, error) {
	return "", "", "", errors.New("plex.tv is not contacted in this slice")
}
func (offlinePlexAuth) PollPIN(context.Context, string) (bool, error) {
	return false, errors.New("plex.tv is not contacted in this slice")
}
func (offlinePlexAuth) Claim(context.Context) error {
	return errors.New("plex.tv claim is not available offline")
}
func (offlinePlexAuth) Status() any { return map[string]string{"mode": "offline"} }

// configFrontends is the config-store side of app.frontendControl. Listener
// reconcile stays in the app package because it binds ports.
type configFrontends struct {
	registry *frontend.Registry
	config   *config.Store
}

func newConfigFrontends(t *testing.T, cfg *config.Store) *configFrontends {
	t.Helper()
	reg := frontend.NewRegistry(func(frontend.Protocol) frontend.Host { return frontend.Host{} })
	for _, item := range []struct {
		id   frontend.Protocol
		name string
	}{
		{"plex", "Plex"},
		{"jellyfin", "Jellyfin"},
		{"emby", "Emby"},
	} {
		port, _ := frontend.DefaultPort(item.id)
		if err := reg.Register(describedFrontend{id: item.id, name: item.name, port: port}); err != nil {
			t.Fatal(err)
		}
	}
	return &configFrontends{registry: reg, config: cfg}
}

type describedFrontend struct {
	id   frontend.Protocol
	name string
	port int
}

func (f describedFrontend) Describe() frontend.Descriptor {
	return frontend.Descriptor{ID: f.id, Name: f.name, DefaultPort: f.port}
}
func (describedFrontend) Mount(context.Context, frontend.Host) (frontend.Instance, error) {
	return nil, errors.New("protocol listeners are outside this acceptance slice")
}

func (f *configFrontends) descriptor(id frontend.Protocol) (frontend.Descriptor, bool) {
	for _, d := range f.registry.Descriptors() {
		if d.ID == id {
			return d, true
		}
	}
	return frontend.Descriptor{}, false
}

func (f *configFrontends) List(context.Context) ([]admin.FrontendInfo, error) {
	cfg := f.config.Get()
	out := make([]admin.FrontendInfo, 0)
	for _, d := range f.registry.Descriptors() {
		enabled := cfg.Frontends[string(d.ID)].Enabled
		if cfg.Frontends == nil && d.ID == "plex" {
			enabled = true
		}
		out = append(out, admin.FrontendInfo{Descriptor: d, Enabled: enabled})
	}
	return out, nil
}

func (f *configFrontends) Settings(_ context.Context, id frontend.Protocol) (frontend.Settings, error) {
	if _, ok := f.descriptor(id); !ok {
		return nil, frontend.ErrNotRegistered
	}
	cfg := f.config.Get()
	if id == "plex" {
		return frontend.Settings{"port": float64(cfg.Server.Port), "serverName": cfg.Server.FriendlyName}, nil
	}
	settings := frontend.Settings{}
	for k, v := range cfg.Frontends[string(id)].Settings {
		settings[k] = v
	}
	if port := cfg.FrontendPorts[string(id)]; port > 0 {
		settings["port"] = float64(port)
	} else if port, ok := frontend.DefaultPort(id); ok {
		settings["port"] = float64(port)
	}
	return settings, nil
}

func (f *configFrontends) PutSettings(_ context.Context, id frontend.Protocol, s frontend.Settings) error {
	if _, ok := f.descriptor(id); !ok {
		return frontend.ErrNotRegistered
	}
	supported, err := f.registry.ValidateSettings(id, s)
	if err != nil {
		return err
	}
	if err := acceptFrontendSettings(s, supported); err != nil {
		return err
	}
	if id == "plex" {
		for key := range s {
			if key != "port" && key != "serverName" {
				return fmt.Errorf("unsupported Plex setting %s", key)
			}
		}
		return f.config.Update(func(c *config.Config) error {
			if port, ok := s["port"].(float64); ok {
				c.Server.Port = int(port)
			}
			if name, ok := s["serverName"].(string); ok {
				c.Server.FriendlyName = name
			}
			return nil
		})
	}
	return f.config.Update(func(c *config.Config) error {
		if c.Frontends == nil {
			c.Frontends = map[string]config.Frontend{}
			c.Frontends["plex"] = config.Frontend{Enabled: true}
		}
		v := c.Frontends[string(id)]
		v.Settings = make(map[string]any, len(s))
		for k, val := range s {
			if k != "port" {
				v.Settings[k] = val
			}
		}
		if c.FrontendPorts == nil {
			c.FrontendPorts = make(map[string]int)
		}
		if port, ok := s["port"].(float64); ok {
			c.FrontendPorts[string(id)] = int(port)
		} else {
			delete(c.FrontendPorts, string(id))
		}
		c.Frontends[string(id)] = v
		return nil
	})
}

func (f *configFrontends) Toggle(_ context.Context, id frontend.Protocol, enabled bool) error {
	if _, ok := f.descriptor(id); !ok {
		return frontend.ErrNotRegistered
	}
	return f.config.Update(func(c *config.Config) error {
		if c.Frontends == nil {
			c.Frontends = map[string]config.Frontend{}
			if id != "plex" {
				c.Frontends["plex"] = config.Frontend{Enabled: true}
			}
		}
		v := c.Frontends[string(id)]
		v.Enabled = enabled
		c.Frontends[string(id)] = v
		return nil
	})
}

// acceptFrontendSettings mirrors app.validateFrontendSettings. That function
// is unexported, and this slice does not import the app package.
func acceptFrontendSettings(s frontend.Settings, protocolValidator bool) error {
	for k, v := range s {
		switch k {
		case "port":
			n, ok := v.(float64)
			if !ok || n < 1 || n > 65535 || math.Trunc(n) != n {
				return errors.New("invalid port")
			}
		case "serverName":
			if _, ok := v.(string); !ok {
				return errors.New("invalid server name")
			}
		case "discovery", "quickConnect", "publicViewerPicker":
			if _, ok := v.(bool); !ok {
				return fmt.Errorf("invalid %s", k)
			}
		default:
			if !protocolValidator {
				return fmt.Errorf("unknown setting %s", k)
			}
		}
	}
	return nil
}

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
