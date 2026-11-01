package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/braedonsaunders/ether/internal/config"
)

func TestTrayFollowsChangedServerPort(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ETHER_DATA_DIR", dir)
	t.Setenv("ETHER_URL", "")
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"server":{"port":32400}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	a := newApp(slog.New(slog.NewTextHandler(io.Discard, nil)))
	var opened string
	a.openURL = func(u string) error { opened = u; return nil }
	if err := os.WriteFile(path, []byte(`{"server":{"port":32450}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	a.openEther()
	if opened != "http://127.0.0.1:32450/ether/" {
		t.Fatalf("opened %s after port change", opened)
	}
	opened = ""
	if code := a.openUI(); code != 0 || opened != "http://127.0.0.1:32450/ether/" {
		t.Fatalf("shortcut code=%d URL=%s", code, opened)
	}
	if got := a.updatePageURL(); got != "http://127.0.0.1:32450/ether/maintenance/updates" {
		t.Fatalf("update URL = %s", got)
	}
}

func TestLocalBaseURLWithoutPrivateConfig(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private-config-not-accessible")
	published := func(got string) (string, error) {
		if got != dir {
			t.Fatalf("data directory = %s", got)
		}
		return "http://127.0.0.1:32450", nil
	}
	if got := localBaseURL(dir, "", published); got != "http://127.0.0.1:32450" {
		t.Fatalf("protected configuration fallback = %s", got)
	}
	if got := localBaseURL(dir, "http://localhost:32500/", func(string) (string, error) {
		t.Fatal("explicit URL must bypass discovery")
		return "", nil
	}); got != "http://localhost:32500" {
		t.Fatal(got)
	}
	if got := localBaseURL(dir, "", func(string) (string, error) { return "", errors.New("not published") }); got != defaultBaseURL {
		t.Fatal(got)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("discovery created private directory: %v", err)
	}
}

func TestTrayStatusFollowsPublishedEndpoint(t *testing.T) {
	newServer := func(id string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/identity" {
				fmt.Fprintf(w, `<MediaContainer machineIdentifier="%s" version="1.43.5"/>`, id)
			} else {
				http.NotFound(w, r)
			}
		}))
	}
	oldServer, movedServer := newServer("old-port"), newServer("new-port")
	defer oldServer.Close()
	defer movedServer.Close()
	base := oldServer.URL
	a := &app{baseURL: oldServer.URL, status: NewStatusClient(oldServer.URL, oldServer.Client()), resolveBaseURL: func() string { return base }}
	if st, err := a.pollStatus(context.Background()); err != nil || st.MachineID != "old-port" {
		t.Fatalf("before: %+v %v", st, err)
	}
	base = movedServer.URL
	if st, err := a.pollStatus(context.Background()); err != nil || st.MachineID != "new-port" {
		t.Fatalf("after: %+v %v", st, err)
	}
	if a.status.baseURL != oldServer.URL {
		t.Fatal("poll mutated the shared HTTP client")
	}
}

func TestOpenServerConfigNeverCreates(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Ether")
	if _, err := openServerConfig(dir); err == nil {
		t.Fatal("missing config opened without error")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("tray created the server data dir: %v", err)
	}
}

// Reading an existing, unsealed config must not rewrite it or mint a
// credential key in the server's data directory.
func TestOpenServerConfigNeverWrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	doc := []byte(`{"server":{"port":32500},"updates":{"channel":"beta"}}`)
	if err := os.WriteFile(path, doc, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := openServerConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.Server.Port != 32500 || c.Updates.Channel != "beta" {
		t.Fatalf("read %+v %+v", c.Server, c.Updates)
	}
	if got, _ := os.ReadFile(path); string(got) != string(doc) {
		t.Fatalf("config.json rewritten:\n%s", got)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("tray created files in the data dir: %v", entries)
	}
}

func TestServerBaseURLFollowsBindAddress(t *testing.T) {
	for _, tc := range []struct {
		srv  config.Server
		want string
	}{
		{config.Server{}, "http://127.0.0.1:32400"},
		{config.Server{Port: 32401, BindAddress: "0.0.0.0"}, "http://127.0.0.1:32401"},
		{config.Server{Port: 32400, BindAddress: "::"}, "http://127.0.0.1:32400"},
		{config.Server{Port: 32400, BindAddress: "192.168.1.20"}, "http://192.168.1.20:32400"},
		{config.Server{Port: 8080, BindAddress: "fd00::5"}, "http://[fd00::5]:8080"},
	} {
		if got := serverBaseURL(tc.srv); got != tc.want {
			t.Errorf("%+v: %s, want %s", tc.srv, got, tc.want)
		}
	}
}
