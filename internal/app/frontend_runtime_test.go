package app

import (
	"bytes"
	"context"
	"errors"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/plextv"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/frontend"
)

func TestPlexRegistryTogglePreservesHandler(t *testing.T) {
	a := setupTestApp(t, true)
	call := func(handler http.Handler, path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("GET", path, nil)
		req.RemoteAddr = "127.0.0.1:12345"
		handler.ServeHTTP(w, req)
		return w
	}
	direct := call(a.plexServer, "/identity")
	mounted := call(a.Handler(), "/identity")
	if direct.Code != mounted.Code || !bytes.Equal(direct.Body.Bytes(), mounted.Body.Bytes()) {
		t.Fatalf("registry changed Plex response: direct=%d %q mounted=%d %q", direct.Code, direct.Body, mounted.Code, mounted.Body)
	}
	if err := a.frontends.control.Toggle(context.Background(), "plex", false); err != nil {
		t.Fatal(err)
	}
	if got := call(a.Handler(), "/identity"); got.Code != http.StatusNotFound {
		t.Fatal(got.Code)
	}
	if got := call(a.Handler(), "/ether/api/health"); got.Code != http.StatusOK {
		t.Fatal("owner listener unavailable", got.Code)
	}
	if err := a.frontends.control.Toggle(context.Background(), "plex", true); err != nil {
		t.Fatal(err)
	}
	restored := call(a.Handler(), "/identity")
	if restored.Code != direct.Code || !bytes.Equal(restored.Body.Bytes(), direct.Body.Bytes()) {
		t.Fatal("Plex response changed after remount")
	}
}
func TestPlexFrontendStatusUsesMainListener(t *testing.T) {
	a := setupTestApp(t, true)
	a.frontends.mu.Lock()
	a.frontends.mainAddr = &net.TCPAddr{IP: net.ParseIP("192.0.2.8"), Port: 32450}
	a.frontends.mu.Unlock()
	list, err := a.frontends.control.List(context.Background())
	if err != nil || len(list) != 1 {
		t.Fatal(list, err)
	}
	item := list[0]
	if item.Descriptor.ID != frontend.Protocol("plex") || !item.Enabled || len(item.Status.Addresses) != 1 || item.Status.Addresses[0] != "192.0.2.8:32450" || !item.Status.LiveStreamsKnown || !item.Status.Clients24hKnown {
		t.Fatal(item)
	}
}

func TestPlexFrontendSettingsUseExistingServerConfig(t *testing.T) {
	a := setupTestApp(t, true)
	ctx := context.Background()
	if err := a.frontends.control.PutSettings(ctx, "plex", frontend.Settings{"port": float64(32401), "serverName": "Test Ether"}); err != nil {
		t.Fatal(err)
	}
	cfg := a.cfg.Get()
	if cfg.Server.Port != 32401 || cfg.Server.FriendlyName != "Test Ether" || cfg.FrontendPorts["plex"] != 0 {
		t.Fatal(cfg.Server, cfg.FrontendPorts)
	}
	settings, err := a.frontends.control.Settings(ctx, "plex")
	if err != nil || settings["port"] != float64(32401) || settings["serverName"] != "Test Ether" {
		t.Fatal(settings, err)
	}
}

func TestPlexFrontendOwnerAPI(t *testing.T) {
	a := setupTestApp(t, true)
	if err := a.users.SetOwner(context.Background(), plextv.Account{ID: 42, Username: "owner"}, "test-owner"); err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("X-Plex-Token", "test-owner")
		if method != "GET" {
			req.Header.Set("X-Ether-Request", "1")
			req.Header.Set("Content-Type", "application/json")
		}
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, req)
		return w
	}
	list := call("GET", "/ether/api/frontends", "")
	if list.Code != 200 || !strings.Contains(list.Body.String(), `"id":"plex"`) {
		t.Fatal(list.Code, list.Body)
	}
	toggled := call("PUT", "/ether/api/frontends/plex/enabled", `{"enabled":false}`)
	if toggled.Code != 200 {
		t.Fatal(toggled.Code, toggled.Body)
	}
	status := call("GET", "/ether/api/frontends/plex/status", "")
	if status.Code != 200 {
		t.Fatal(status.Code, status.Body)
	}
	if _, ok := a.frontends.registry.Instance("plex"); ok {
		t.Fatal("frontend still mounted")
	}
}

type fakePlexDiscovery struct {
	started, stopped int
	err              error
}

func (f *fakePlexDiscovery) Start(context.Context) error { f.started++; return f.err }
func (f *fakePlexDiscovery) Stop()                       { f.stopped++ }
func TestPlexDiscoveryTracksRuntimeToggle(t *testing.T) {
	a := setupTestApp(t, true)
	first, second := &fakePlexDiscovery{}, &fakePlexDiscovery{}
	a.frontends.discovery = first
	a.frontends.newDiscovery = func() plexDiscovery { return second }
	a.frontends.start(context.Background(), &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 32400})
	if first.started != 1 {
		t.Fatal(first)
	}
	if err := a.frontends.control.Toggle(context.Background(), "plex", false); err != nil || first.stopped != 1 {
		t.Fatal(err, first)
	}
	if err := a.frontends.control.Toggle(context.Background(), "plex", true); err != nil || second.started != 1 {
		t.Fatal(err, second)
	}
	a.frontends.stop()
	if second.stopped != 1 {
		t.Fatal(second)
	}
}
func TestPlexDiscoveryFailureDoesNotUnmount(t *testing.T) {
	a := setupTestApp(t, true)
	failed := &fakePlexDiscovery{err: errors.New("discovery failed")}
	a.frontends.discovery = failed
	a.frontends.start(context.Background(), &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 32400})
	list, err := a.frontends.control.List(context.Background())
	if err != nil || len(list) != 1 || len(list[0].Status.Warnings) != 1 {
		t.Fatal(list, err)
	}
	if _, ok := a.frontends.registry.Instance("plex"); !ok {
		t.Fatal("discovery failure unmounted Plex")
	}
	a.frontends.stop()
}

func TestPlexFreeSetupKeepsOwnerConsole(t *testing.T) {
	dir := t.TempDir()
	cfg, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Update(func(c *config.Config) error {
		c.Frontends = map[string]config.Frontend{"jellyfin": {Enabled: true}}
		c.Transcode.FFmpegPath = "/not-installed/ffmpeg"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	a, err := New(context.Background(), Options{DataDir: dir, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	call := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		r.RemoteAddr = "127.0.0.1:12345"
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	if got := call("/identity"); got.Code != http.StatusNotFound {
		t.Fatal(got.Code)
	}
	if got := call("/ether/api/bootstrap"); got.Code != http.StatusOK {
		t.Fatal(got.Code, got.Body)
	}
}
