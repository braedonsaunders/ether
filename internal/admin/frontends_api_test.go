package admin_test

import (
	"context"
	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/frontend"
	"net/http"
	"testing"
)

type frontendsFake struct {
	enabled  bool
	settings frontend.Settings
}

func (f *frontendsFake) List(context.Context) ([]admin.FrontendInfo, error) {
	return []admin.FrontendInfo{{Descriptor: frontend.Descriptor{ID: "plex"}, Enabled: f.enabled}}, nil
}
func (f *frontendsFake) Settings(context.Context, frontend.Protocol) (frontend.Settings, error) {
	return f.settings, nil
}
func (f *frontendsFake) PutSettings(_ context.Context, _ frontend.Protocol, s frontend.Settings) error {
	f.settings = s
	return nil
}
func (f *frontendsFake) Toggle(_ context.Context, _ frontend.Protocol, b bool) error {
	f.enabled = b
	return nil
}
func TestFrontendAPI(t *testing.T) {
	store := newStore(t, func(c *config.Config) { seededConfig(c); c.SetupComplete = true })
	f := &frontendsFake{}
	h := admin.New(admin.Deps{Config: store, IsOwnerToken: storedOwner(store), Frontends: f})
	unauth := serve(h, req("GET", "/ether/api/frontends", ""))
	if unauth.Code != http.StatusUnauthorized {
		t.Fatal(unauth.Code)
	}
	call := func(method, path, body string) *http.Request {
		r := req(method, path, body)
		r.Header.Set("X-Plex-Token", ownerToken)
		return r
	}
	w := serve(h, call("PUT", "/ether/api/frontends/plex/enabled", `{"enabled":true}`))
	if w.Code != 200 || !f.enabled {
		t.Fatal(w.Code, w.Body.String())
	}
	w = serve(h, call("PUT", "/ether/api/frontends/plex/settings", `{"port":8096}`))
	if w.Code != 200 || f.settings["port"] != float64(8096) {
		t.Fatal(w.Code, w.Body.String())
	}
	w = serve(h, call("GET", "/ether/api/frontends/plex/status", ""))
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	w = serve(h, call("GET", "/ether/api/frontends/missing/status", ""))
	if w.Code != 404 {
		t.Fatal(w.Code)
	}
	w = serve(h, call("GET", "/ether/api/frontends", ""))
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	w = serve(h, call("PUT", "/ether/api/frontends/plex/enabled", `{}`))
	if w.Code != 400 {
		t.Fatal(w.Code)
	}
}

func TestFrontendHealth(t *testing.T) {
	store := newStore(t, func(c *config.Config) { seededConfig(c); c.SetupComplete = true })
	f := &healthFrontends{}
	h := admin.New(admin.Deps{Config: store, IsOwnerToken: storedOwner(store), Frontends: f})
	for _, protocol := range []string{"plex", "jellyfin", "emby", "silo", "stremio", "webdav", "m3u", "dlna"} {
		path := "/ether/api/frontends/" + protocol + "/health"
		if w := serve(h, req("GET", path, "")); w.Code != 401 {
			t.Fatal(w.Code)
		}
		for _, state := range []struct {
			enabled, mounted bool
			failure          string
			want             int
		}{{false, false, "", 503}, {true, false, "", 503}, {true, true, "", 200}, {true, true, "bind failed", 503}} {
			f.enabled, f.mounted, f.failure = state.enabled, state.mounted, state.failure
			r := req("GET", path, "")
			r.Header.Set("X-Plex-Token", ownerToken)
			if w := serve(h, r); w.Code != state.want {
				t.Fatal(protocol, state, w.Code, w.Body.String())
			}
		}
	}
	r := req("GET", "/ether/api/frontends/missing/health", "")
	r.Header.Set("X-Plex-Token", ownerToken)
	if w := serve(h, r); w.Code != 404 {
		t.Fatal(w.Code)
	}
}

type healthFrontends struct {
	frontendsFake
	mounted bool
	failure string
}

func (f *healthFrontends) List(context.Context) ([]admin.FrontendInfo, error) {
	var out []admin.FrontendInfo
	for _, id := range []frontend.Protocol{"plex", "jellyfin", "emby", "silo", "stremio", "webdav", "m3u", "dlna"} {
		out = append(out, admin.FrontendInfo{Descriptor: frontend.Descriptor{ID: id}, Enabled: f.enabled, Status: frontend.Status{Mounted: f.mounted, Error: f.failure}})
	}
	return out, nil
}
