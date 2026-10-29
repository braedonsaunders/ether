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
