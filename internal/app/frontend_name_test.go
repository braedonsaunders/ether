package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/plextv"
)

func TestEveryFrontendAdvertisesGeneralName(t *testing.T) {
	a := setupTestApp(t, true)
	ctx := context.Background()
	if err := a.users.SetOwner(ctx, plextv.Account{ID: 42, Username: "owner"}, "test-owner"); err != nil {
		t.Fatal(err)
	}
	owner, err := a.users.Owner(ctx)
	if err != nil || owner == nil {
		t.Fatal(err)
	}
	if err := a.cfg.Update(func(c *config.Config) error {
		c.Server.FriendlyName = "Living Room"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.frontends.control.PutSettings(ctx, "dlna", frontend.Settings{"viewerId": float64(owner.ID)}); err != nil {
		t.Fatal(err)
	}
	grants, err := identity.NewGrants(a.db, a.users, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	webdavToken, err := grants.Issue(ctx, owner.ID, nil, "webdav", core.Device{ID: "files"})
	if err != nil {
		t.Fatal(err)
	}
	mounted := map[frontend.Protocol]http.Handler{}
	for _, id := range []frontend.Protocol{"jellyfin", "emby", "silo", "dlna", "webdav"} {
		inst, err := a.frontends.registry.Mount(ctx, id)
		if err != nil {
			t.Fatalf("mount %s: %v", id, err)
		}
		mounted[id] = inst.Handler()
	}
	assertAdvertisedName(t, a, mounted, "Living Room", owner.ID, webdavToken)
	if err := a.cfg.Update(func(c *config.Config) error {
		c.Server.FriendlyName = "Den"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	assertAdvertisedName(t, a, mounted, "Den", owner.ID, webdavToken)
}

func assertAdvertisedName(t *testing.T, a *App, mounted map[frontend.Protocol]http.Handler, name string, viewerID int64, webdavToken string) {
	t.Helper()
	plex := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Plex-Token", "test-owner")
	req.RemoteAddr = "127.0.0.1:12345"
	a.handler.ServeHTTP(plex, req)
	if plex.Code != http.StatusOK || !strings.Contains(plex.Body.String(), `friendlyName="`+name+`"`) {
		t.Fatalf("plex identity: %d %s", plex.Code, plex.Body.String())
	}
	checks := []struct {
		id, method, path, user, pass string
	}{
		{"jellyfin", http.MethodGet, "/jellyfin/System/Info/Public", "", ""},
		{"emby", http.MethodGet, "/emby/System/Info/Public", "", ""},
		{"silo", http.MethodGet, "/silo/api/v2/theme/branding", "", ""},
		{"dlna", http.MethodGet, "/dlna/description.xml", "", ""},
		{"webdav", "PROPFIND", "/webdav/", strconv.FormatInt(viewerID, 10), webdavToken},
	}
	for _, check := range checks {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(check.method, check.path, nil)
		req.RemoteAddr = "127.0.0.1:12345"
		if check.id == "webdav" {
			req.SetBasicAuth(check.user, check.pass)
			req.Header.Set("Depth", "0")
		}
		mounted[frontend.Protocol(check.id)].ServeHTTP(rec, req)
		if rec.Code != http.StatusOK && rec.Code != 207 {
			t.Fatalf("%s %s: %d %s", check.id, name, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), name) {
			t.Fatalf("%s advertised %q, body %s", check.id, name, rec.Body.String())
		}
	}
}
