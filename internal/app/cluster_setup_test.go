package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/clusterconfig"
	"github.com/braedonsaunders/ether/internal/config"
)

func setupTestApp(t *testing.T, complete bool) *App {
	t.Helper()
	return setupTestAppWith(t, complete, Options{})
}

func setupTestAppWith(t *testing.T, complete bool, opts Options) *App {
	t.Helper()
	dir := t.TempDir()
	cfg, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Update(func(c *config.Config) error {
		c.SetupComplete = complete
		c.Plex.Token = "test-owner"
		c.Transcode.FFmpegPath = "/not-installed/ffmpeg"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s := &clusterSetup{state: "idle", stop: make(chan struct{}, 1), check: func(context.Context, clusterconfig.SetupRequest) error { return nil }}
	opts.DataDir = dir
	opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	opts.clusterSetup = s
	a, err := New(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	return a
}

func TestSetupRequiresOwnerAndCSRF(t *testing.T) {
	a := setupTestApp(t, true)
	for _, test := range []struct {
		token, csrf string
		code        int
	}{{"", "1", 401}, {"test-owner", "", 403}} {
		req := httptest.NewRequest("POST", "http://localhost/ether/api/cluster/setup/apply", strings.NewReader(`{"mode":"create","databaseUrl":"postgres://db/ether"}`))
		req.RemoteAddr = "127.0.0.1:1000"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Plex-Token", test.token)
		req.Header.Set("X-Ether-Request", test.csrf)
		w := httptest.NewRecorder()
		a.handler.ServeHTTP(w, req)
		if w.Code != test.code {
			t.Fatalf("got %d: %s", w.Code, w.Body.String())
		}
	}
	if len(a.opts.clusterSetup.stop) != 0 {
		t.Fatal("unauthorized restart")
	}
}

func TestSetupQueuesOnlyAfterBackupWithoutSwitchingLiveConfig(t *testing.T) {
	a := setupTestApp(t, true)
	s := a.opts.clusterSetup
	body := `{"mode":"create","databaseUrl":"postgres://db/ether"}`
	for _, path := range []string{"/setup/check", "/setup/apply"} {
		req := httptest.NewRequest("POST", path, strings.NewReader(body))
		w := httptest.NewRecorder()
		a.clusterSetupHandler().ServeHTTP(w, req)
		want := 200
		if path == "/setup/apply" {
			want = 202
		}
		if w.Code != want {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		if a.cfg.Get().Database.IsPostgres() {
			t.Fatal("changed database while writers still run")
		}
	}
	if len(s.stop) != 1 || s.request == nil || s.state != "restarting" {
		t.Fatal("setup not queued")
	}
	snaps, err := a.backups.List()
	if err != nil || len(snaps) == 0 {
		t.Fatalf("no automatic backup: %v", err)
	}
	req := httptest.NewRequest("POST", "/setup/apply", strings.NewReader(body))
	w := httptest.NewRecorder()
	a.clusterSetupHandler().ServeHTTP(w, req)
	if w.Code != 409 {
		t.Fatal("duplicate setup not rejected")
	}
}

func TestStoppedSetupRestoresSettingsAndKeyOnFailure(t *testing.T) {
	a := setupTestApp(t, true)
	dir := a.opts.DataDir
	a.Close()
	original := map[string][]byte{}
	for _, name := range []string{"config.json", config.CredentialKeyFile} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		original[name] = b
	}
	s := a.opts.clusterSetup
	s.request = &clusterconfig.SetupRequest{Mode: "join", Token: "private"}
	s.execute = func(context.Context, string, clusterconfig.SetupRequest) error {
		for name := range original {
			if err := os.WriteFile(filepath.Join(dir, name), []byte("changed"), 0600); err != nil {
				return err
			}
		}
		return errors.New("postgres://owner:private@db/ether")
	}
	if err := s.apply(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	for name, want := range original {
		got, _ := os.ReadFile(filepath.Join(dir, name))
		if !bytes.Equal(got, want) {
			t.Fatalf("did not restore %s", name)
		}
	}
	if s.state != "failed" || strings.Contains(s.message, "private") || s.request != nil {
		t.Fatal("unsafe or missing failure status")
	}
	cfg, err := config.Open(dir)
	if err != nil || cfg.Get().Plex.Token != "test-owner" {
		t.Fatalf("restored server cannot load credentials: %v", err)
	}
}

func TestFreshJoinAndCreateEligibility(t *testing.T) {
	a := setupTestApp(t, false)
	tok := clusterconfig.JoinToken{DatabaseURL: "postgres://db/ether", ID: "id", Secret: "secret", Key: make([]byte, 32)}
	for _, mode := range []string{"create", "join"} {
		body, _ := json.Marshal(clusterconfig.SetupRequest{Mode: mode, DatabaseURL: "postgres://db/ether", Token: tok.String()})
		w := httptest.NewRecorder()
		a.clusterSetupHandler().ServeHTTP(w, httptest.NewRequest("POST", "/setup/check", bytes.NewReader(body)))
		want := 403
		if mode == "join" {
			want = 200
		}
		if w.Code != want {
			t.Fatalf("%s: %d %s", mode, w.Code, w.Body.String())
		}
	}
	t.Setenv(config.DatabaseURLEnv, "postgres://deployment/db")
	w := httptest.NewRecorder()
	a.clusterSetupHandler().ServeHTTP(w, httptest.NewRequest("GET", "/setup", nil))
	if !strings.Contains(w.Body.String(), `"available":false`) {
		t.Fatal("ignored database environment override")
	}
}
