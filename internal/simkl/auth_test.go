package simkl

import (
	"context"
	"database/sql"
	"path/filepath"
	"time"

	"errors"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/store"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDeviceAndRefreshWireFormat(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "POST" || r.Header.Get("User-Agent") == "" {
			t.Error("missing method or agent")
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if r.Form.Get("client_id") != "client" || r.Form.Get("client_secret") != "secret" {
			t.Error("missing client credentials")
		}
		switch r.URL.Path {
		case "/oauth2/device":
			if r.Form.Get("scope") != "media:read media:write" {
				t.Error("wrong scope")
			}
			_, _ = w.Write([]byte(`{"device_code":"private","user_code":"ABCD-EFGH","verification_uri":"https://simkl.com/pin","expires_in":900,"interval":5}`))
		case "/oauth2/token":
			if calls == 2 && r.Form.Get("device_code") != "private" {
				t.Error("wrong device code")
			}
			if calls == 3 && r.Form.Get("refresh_token") != "refresh" {
				t.Error("wrong refresh")
			}
			_, _ = w.Write([]byte(`{"access_token":"access","refresh_token":"refresh","expires_in":3600,"scope":"media:read media:write"}`))
		case "/oauth2/revoke":
			if r.Form.Get("token") != "refresh" {
				t.Error("wrong token")
			}
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	c := AuthClient{BaseURL: srv.URL, ClientID: "client", ClientSecret: "secret"}
	d, err := c.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Poll(context.Background(), d.DeviceCode); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Refresh(context.Background(), "refresh"); err != nil {
		t.Fatal(err)
	}
	if err := c.Revoke(context.Background(), "refresh"); err != nil {
		t.Fatal(err)
	}
}
func TestErrorsAreRedacted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":"secret-value","error_description":"private"}`))
	}))
	defer srv.Close()
	c := AuthClient{BaseURL: srv.URL, ClientID: "client"}
	_, err := c.Poll(context.Background(), "secret-value")
	var api *APIError
	if !errors.As(err, &api) || api.Code != "unknown" || strings.Contains(err.Error(), "secret-value") || strings.Contains(err.Error(), "private") {
		t.Fatalf("unsafe error: %v", err)
	}
}
func TestInsufficientScope(t *testing.T) {
	if err := validateToken(Token{ClientID: "id", AccessToken: "a", RefreshToken: "r", ExpiresIn: 1, Scope: "media:read"}); err == nil {
		t.Fatal("accepted read-only token")
	}
}

type testSealer struct{}

func (testSealer) SealField(p string, parts ...string) (string, error) {
	return "ether-sealed:v2:" + strings.Join(parts, "|") + ":" + p, nil
}
func (testSealer) OpenField(p string, parts ...string) (string, bool, error) {
	prefix := "ether-sealed:v2:" + strings.Join(parts, "|") + ":"
	if !strings.HasPrefix(p, prefix) {
		return "", false, errors.New("wrong context")
	}
	return strings.TrimPrefix(p, prefix), false, nil
}
func TestTokenStorageIsolation(t *testing.T) {
	db, err := store.OpenPath(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := TokenStore{DB: db, Sealer: testSealer{}}
	token := Token{ClientID: "id", AccessToken: "access-secret", RefreshToken: "refresh-secret", ExpiresIn: 3600, Scope: "media:read media:write"}
	if err := s.Save(context.Background(), 1, token, time.Now()); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := db.QueryRow(`SELECT access_token FROM simkl_tokens WHERE user_id=1`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw == token.AccessToken || !config.IsSealed(raw) {
		t.Fatal("token not sealed")
	}
	if _, err := s.Load(context.Background(), 2); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("other viewer: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO simkl_tokens(user_id,client_id,access_token,refresh_token,expires_at) SELECT 2,client_id,access_token,refresh_token,expires_at FROM simkl_tokens WHERE user_id=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(context.Background(), 2); err == nil {
		t.Fatal("copied token opened for another viewer")
	}
}

func TestDevicePollingAndDisconnect(t *testing.T) {
	db, err := store.OpenPath(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	polls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth2/device":
			_, _ = w.Write([]byte(`{"device_code":"device","user_code":"CODE","verification_uri":"https://simkl.com/pin","expires_in":900,"interval":5}`))
		case "/oauth2/token":
			polls++
			if polls == 1 {
				w.WriteHeader(400)
				_, _ = w.Write([]byte(`{"error":"slow_down"}`))
				return
			}
			_, _ = w.Write([]byte(`{"access_token":"access","refresh_token":"refresh","expires_in":3600,"scope":"media:read media:write"}`))
		case "/oauth2/revoke":
		default:
			t.Errorf("unexpected %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	m := DeviceManager{Auth: AuthClient{BaseURL: srv.URL, ClientID: "id"}, Tokens: TokenStore{DB: db, Sealer: testSealer{}}}
	ctx := context.Background()
	if _, err := m.Start(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Poll(ctx, 1); !errors.Is(err, ErrPollEarly) {
		t.Fatalf("early poll: %v", err)
	}
	if _, err := db.Exec(`UPDATE simkl_devices SET next_poll=0 WHERE user_id=1`); err != nil {
		t.Fatal(err)
	}
	if status, err := m.Poll(ctx, 1); err != nil || status != "pending" {
		t.Fatalf("slow down: %s %v", status, err)
	}
	var interval int
	if err := db.QueryRow(`SELECT interval_seconds FROM simkl_devices WHERE user_id=1`).Scan(&interval); err != nil {
		t.Fatal(err)
	}
	if interval != 10 {
		t.Fatal("slow down did not extend interval")
	}
	if _, err := db.Exec(`UPDATE simkl_devices SET next_poll=0 WHERE user_id=1`); err != nil {
		t.Fatal(err)
	}
	if status, err := m.Poll(ctx, 1); err != nil || status != "authorized" {
		t.Fatalf("poll: %s %v", status, err)
	}
	if _, err := m.Tokens.Load(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if err := m.Disconnect(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Tokens.Load(ctx, 1); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("still connected: %v", err)
	}
	if _, err := m.Start(ctx, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE simkl_devices SET expires_at=1 WHERE user_id=2`); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Poll(ctx, 2); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired: %v", err)
	}
}

func TestDisconnectCancelsInFlightPoll(t *testing.T) {
	db, err := store.OpenPath(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	started := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth2/device" {
			_, _ = w.Write([]byte(`{"device_code":"device","user_code":"CODE","verification_uri":"https://simkl.com/pin","expires_in":900,"interval":5}`))
			return
		}
		close(started)
		select {
		case <-r.Context().Done():
		case <-release:
			_, _ = w.Write([]byte(`{"access_token":"a","refresh_token":"r","expires_in":300,"scope":"media:read media:write"}`))
		}
	}))
	defer srv.Close()
	m := DeviceManager{Auth: AuthClient{BaseURL: srv.URL, ClientID: "id"}, Tokens: TokenStore{DB: db, Sealer: testSealer{}}}
	ctx := context.Background()
	if _, err := m.Start(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE simkl_devices SET next_poll=0 WHERE user_id=1`); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := m.Poll(ctx, 1); done <- err }()
	<-started
	if err := m.Disconnect(ctx, 1); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case err := <-done:
		if !errors.Is(err, ErrNoPending) {
			t.Fatalf("poll: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("poll was not canceled")
	}
	if _, err := m.Tokens.Load(ctx, 1); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unexpected grant: %v", err)
	}
}

func TestAuthNeverForwardsCredentialsOnRedirect(t *testing.T) {
	forwarded := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forwarded = true }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	c := AuthClient{BaseURL: source.URL, ClientID: "client", ClientSecret: "secret"}
	if _, err := c.Start(context.Background()); err == nil {
		t.Fatal("redirect accepted")
	}
	if forwarded {
		t.Fatal("credentials sent to redirect target")
	}
}

func TestDeviceFlowCannotMoveBetweenViewers(t *testing.T) {
	db, err := store.OpenPath(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	polls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth2/device" {
			_, _ = w.Write([]byte(`{"device_code":"viewer-one-code","user_code":"CODE","verification_uri":"https://simkl.com/pin","expires_in":900,"interval":5}`))
			return
		}
		polls++
		_, _ = w.Write([]byte(`{"access_token":"a","refresh_token":"r","expires_in":300,"scope":"media:read media:write"}`))
	}))
	defer srv.Close()
	m := DeviceManager{Auth: AuthClient{BaseURL: srv.URL, ClientID: "id"}, Tokens: TokenStore{DB: db, Sealer: testSealer{}}}
	ctx := context.Background()
	if _, err := m.Start(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE simkl_devices SET next_poll=0 WHERE user_id=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Poll(ctx, 2); !errors.Is(err, ErrNoPending) || polls != 0 {
		t.Fatalf("cross-viewer poll: %v calls %d", err, polls)
	}
	if err := m.Disconnect(ctx, 2); err != nil {
		t.Fatal(err)
	}
	if status, err := m.Poll(ctx, 1); err != nil || status != "authorized" {
		t.Fatalf("original flow: %s %v", status, err)
	}
	if _, err := m.Tokens.Load(ctx, 2); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cross-viewer token: %v", err)
	}
}

func TestDeviceFlowCanPollOnAnotherNode(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenPath(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth2/device" {
			_, _ = w.Write([]byte(`{"device_code":"secret-device","user_code":"CODE","verification_uri":"https://simkl.com/pin","expires_in":900,"interval":5}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"a","refresh_token":"r","expires_in":300,"scope":"media:read media:write"}`))
	}))
	defer srv.Close()
	auth := AuthClient{BaseURL: srv.URL, ClientID: "id"}
	tokens := TokenStore{DB: db, Sealer: testSealer{}}
	first := DeviceManager{Auth: auth, Tokens: tokens}
	second := DeviceManager{Auth: auth, Tokens: tokens}
	if _, err := first.Start(ctx, 1); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := db.QueryRow(`SELECT device_code FROM simkl_devices WHERE user_id=1`).Scan(&raw); err != nil || !config.IsSealed(raw) {
		t.Fatalf("unsealed flow: %v", err)
	}
	if _, err := db.Exec(`UPDATE simkl_devices SET next_poll=0 WHERE user_id=1`); err != nil {
		t.Fatal(err)
	}
	if status, err := second.Poll(ctx, 1); err != nil || status != "authorized" {
		t.Fatalf("cross-node poll: %s %v", status, err)
	}
	if _, err := tokens.Load(ctx, 1); err != nil {
		t.Fatal(err)
	}
}
