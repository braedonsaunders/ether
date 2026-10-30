package admin_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

// A reverse proxy can use loopback and Host: localhost. Neither grants access.
func TestLoopbackStillRequiresOwner(t *testing.T) {
	store := newStore(t, func(c *config.Config) { seededConfig(c); c.SetupComplete = true })
	h := admin.New(admin.Deps{Config: store, IsOwnerToken: storedOwner(store)})
	for _, tc := range []struct {
		remote, host string
		want         int
	}{
		{"127.0.0.1:5000", "localhost:32400", 401},
		{"[::1]:5000", "[::1]:32400", 401},
		{"127.0.0.1:5000", "127.0.0.1:32400", 401},
		{"127.0.0.1:5000", "evil.example:32400", 401}, // DNS rebinding
		{"10.0.0.5:1", "localhost:32400", 401},        // LAN peer claiming localhost
	} {
		r := req(http.MethodGet, "/ether/api/setup/status", "")
		r.RemoteAddr, r.Host = tc.remote, tc.host
		if rec := serve(h, r); rec.Code != tc.want {
			t.Errorf("%s via %s: got %d want %d", tc.remote, tc.host, rec.Code, tc.want)
		}
	}
	// Bootstrap requires sign-in locally as well.
	r := req(http.MethodGet, "/ether/api/bootstrap", "")
	r.RemoteAddr, r.Host = "127.0.0.1:5000", "localhost:32400"
	var boot struct {
		Authenticated, AuthRequired bool
	}
	if err := json.Unmarshal(serve(h, r).Body.Bytes(), &boot); err != nil {
		t.Fatal(err)
	}
	if boot.Authenticated || !boot.AuthRequired {
		t.Fatalf("bootstrap %+v", boot)
	}
}

type fakeSignIn struct {
	mu      sync.Mutex
	forward string
	tokens  map[string]string // pin id -> token once approved
}

func (f *fakeSignIn) StartPIN(_ context.Context, forward string) (string, string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forward = forward
	return "4242", "ABCD", "https://app.plex.tv/auth#?code=ABCD", nil
}

func (f *fakeSignIn) PINToken(_ context.Context, id string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokens[id], nil
}

func (f *fakeSignIn) approve(id, token string) {
	f.mu.Lock()
	f.tokens[id] = token
	f.mu.Unlock()
}

// fakeSessions issues console sessions for the stored owner token.
type fakeSessions struct {
	mu      sync.Mutex
	store   *config.Store
	live    map[string]bool
	fail    error
	issued  int
	revoked []string
}

func newFakeSessions(store *config.Store) *fakeSessions {
	return &fakeSessions{store: store, live: map[string]bool{}}
}

func (f *fakeSessions) IssueOwnerSession(_ context.Context, credential string) (string, time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return "", time.Time{}, f.fail
	}
	if credential == "" || credential != f.store.Get().Plex.Token {
		return "", time.Time{}, core.ErrUnauthorized
	}
	f.issued++
	tok := fmt.Sprintf("ether-admin-session-%d", f.issued)
	f.live[tok] = true
	return tok, time.Unix(1_900_000_000, 0), nil
}

func (f *fakeSessions) RevokeOwnerSession(_ context.Context, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.live, token)
	f.revoked = append(f.revoked, token)
	return nil
}

// owner accepts the stored owner token or a live session.
func (f *fakeSessions) owner(ctx context.Context, tok string) bool {
	f.mu.Lock()
	live := f.live[tok]
	f.mu.Unlock()
	return live || storedOwner(f.store)(ctx, tok)
}

func TestSignInWithPlex(t *testing.T) {
	store := newStore(t, func(c *config.Config) { seededConfig(c); c.SetupComplete = true })
	si := &fakeSignIn{tokens: map[string]string{}}
	sessions := newFakeSessions(store)
	h := admin.New(admin.Deps{Config: store, IsOwnerToken: sessions.owner, SignIn: si, Sessions: sessions})

	start := func() string {
		r := req(http.MethodPost, "/ether/api/auth/pin", "{}")
		r.RemoteAddr, r.Host = "10.0.0.5:1", "ether.lan:32400"
		rec := serve(h, r)
		if rec.Code != 200 {
			t.Fatalf("start: %d %s", rec.Code, rec.Body)
		}
		var out struct{ Handle, Code, AuthURL string }
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		if out.Handle == "" || out.Handle == "4242" || out.Code != "ABCD" {
			t.Fatalf("start response %+v: handle must be opaque, not the plex.tv PIN id", out)
		}
		return out.Handle
	}
	poll := func(handle string) (int, map[string]any) {
		r := req(http.MethodGet, "/ether/api/auth/pin/"+handle, "")
		r.RemoteAddr = "10.0.0.5:1"
		rec := serve(h, r)
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return rec.Code, body
	}

	handle := start()
	if si.forward != "http://ether.lan:32400/ether/" {
		t.Errorf("forward URL %q", si.forward)
	}
	if code, body := poll(handle); code != 200 || body["done"] != false {
		t.Fatalf("pending poll: %d %v", code, body)
	}
	if code, _ := poll("4242"); code != 404 {
		t.Fatalf("polling by raw PIN id must fail, got %d", code)
	}
	si.approve("4242", ownerToken)

	// A failure to issue the session keeps the approved sign-in retryable.
	sessions.fail = errors.New("database is locked")
	if code, body := poll(handle); code != http.StatusServiceUnavailable || body["token"] != nil {
		t.Fatalf("failed session: %d %v", code, body)
	}
	sessions.fail = nil

	code, body := poll(handle)
	session, _ := body["token"].(string)
	if code != 200 || body["done"] != true || session == "" {
		t.Fatalf("approved poll: %d %v", code, body)
	}
	// The browser gets a console session, never the plex.tv account token.
	if session == ownerToken || strings.Contains(fmt.Sprint(body), ownerToken) {
		t.Fatalf("plex.tv token returned to the browser: %v", body)
	}
	if code, _ := poll(handle); code != 404 {
		t.Fatalf("handle must be single-use, got %d", code)
	}
	r := req(http.MethodGet, "/ether/api/config", "")
	r.Header.Set("X-Ether-Admin-Token", session)
	if rec := serve(h, r); rec.Code != http.StatusOK {
		t.Fatalf("session rejected: %d", rec.Code)
	}

	// A Plex account that doesn't own the server gets no token.
	handle = start()
	si.approve("4242", "someone-else")
	if code, body := poll(handle); code != 403 || body["token"] != nil {
		t.Fatalf("non-owner: %d %v", code, body)
	}

	// Starting a sign-in is a mutation: it needs the CSRF header.
	r = req(http.MethodPost, "/ether/api/auth/pin", "{}")
	r.Header.Del("X-Ether-Request")
	if rec := serve(h, r); rec.Code != http.StatusForbidden {
		t.Fatalf("start without CSRF header: %d", rec.Code)
	}
}

func TestConsoleSessionExchangeAndSignOut(t *testing.T) {
	store := newStore(t, func(c *config.Config) { seededConfig(c); c.SetupComplete = true })
	sessions := newFakeSessions(store)
	h := admin.New(admin.Deps{Config: store, IsOwnerToken: sessions.owner, Sessions: sessions})
	exchange := func(cred string, csrf bool) (int, map[string]any) {
		r := req(http.MethodPost, "/ether/api/auth/session", "{}")
		if cred != "" {
			r.Header.Set("X-Plex-Token", cred)
		}
		if !csrf {
			r.Header.Del("X-Ether-Request")
		}
		rec := serve(h, r)
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return rec.Code, body
	}
	if code, _ := exchange(ownerToken, false); code != http.StatusForbidden {
		t.Fatalf("exchange without CSRF header: %d", code)
	}
	if code, body := exchange("", true); code != http.StatusUnauthorized || body["token"] != nil {
		t.Fatalf("no credential: %d %v", code, body)
	}
	if code, body := exchange("someone-else", true); code != http.StatusUnauthorized || body["token"] != nil {
		t.Fatalf("non-owner: %d %v", code, body)
	}
	code, body := exchange(ownerToken, true)
	session, _ := body["token"].(string)
	if code != 200 || session == "" || session == ownerToken || strings.Contains(fmt.Sprint(body), ownerToken) {
		t.Fatalf("exchange: %d %v", code, body)
	}

	// Plain owner tokens keep working for scripts.
	r := req(http.MethodGet, "/ether/api/setup/status", "")
	r.Header.Set("X-Plex-Token", ownerToken)
	if rec := serve(h, r); rec.Code != 200 {
		t.Fatalf("owner token: %d", rec.Code)
	}

	out := req(http.MethodDelete, "/ether/api/auth/session", "")
	out.Header.Set("X-Ether-Admin-Token", session)
	out.Header.Del("X-Ether-Request")
	if rec := serve(h, out); rec.Code != http.StatusForbidden {
		t.Fatalf("sign-out without CSRF header: %d", rec.Code)
	}
	out.Header.Set("X-Ether-Request", "1")
	if rec := serve(h, out); rec.Code != 200 {
		t.Fatalf("sign-out: %d %s", rec.Code, rec.Body)
	}
	if len(sessions.revoked) != 1 || sessions.revoked[0] != session {
		t.Fatalf("revoked %v", sessions.revoked)
	}
	r = req(http.MethodGet, "/ether/api/setup/status", "")
	r.Header.Set("X-Ether-Admin-Token", session)
	if rec := serve(h, r); rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked session accepted: %d", rec.Code)
	}
	// Signing out again is harmless.
	if rec := serve(h, out); rec.Code != 200 {
		t.Fatalf("second sign-out: %d", rec.Code)
	}
}
