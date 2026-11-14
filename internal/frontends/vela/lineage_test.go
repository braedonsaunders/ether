package vela

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/store"
)

type lineageViewer struct{}

func (lineageViewer) ActiveUser(_ context.Context, id int64) (*core.User, error) {
	if id != 1 {
		return nil, core.ErrUnauthorized
	}
	return &core.User{ID: 1, Username: "viewer"}, nil
}

type lineageIdentity struct {
	frontend.SurfaceIdentity
	grants *identity.Grants
	quick  *identity.QuickConnect
}

func (s lineageIdentity) ResolveDevice(ctx context.Context, a string, d frontend.Device) (*frontend.Viewer, time.Time, error) {
	return s.grants.ResolveDevice(ctx, "vela", a, d)
}
func (s lineageIdentity) IssueRefresh(ctx context.Context, a string, d frontend.Device) (string, error) {
	return s.grants.IssueRefresh(ctx, "vela", a, d)
}
func (s lineageIdentity) GrantScope(ctx context.Context, a string, d frontend.Device) (string, error) {
	return s.grants.GrantScope(ctx, "vela", a, d)
}
func (s lineageIdentity) GrantScopeCurrent(ctx context.Context, a string, d frontend.Device) (*frontend.Viewer, error) {
	return s.grants.GrantScopeCurrent(ctx, "vela", a, d)
}
func (s lineageIdentity) QuickConnectPollDevice(ctx context.Context, a string, d frontend.Device) (string, error) {
	return s.quick.PollSurfaceDevice(ctx, "vela", a, d)
}
func (s lineageIdentity) Refresh(ctx context.Context, a string, d frontend.Device) (string, string, error) {
	return s.grants.Refresh(ctx, "vela", a, d)
}
func (s lineageIdentity) Revoke(ctx context.Context, a string) error {
	return s.grants.RevokeGrant(ctx, 1, hash(a))
}

// Exercise the lineage invariant across real pairing, durable token-result reuse,
// a frontend remount, access rotation, session lookup and authenticated replay.
func checkPairingLineage(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db := store.OpenTest(t)
	if _, err := db.Exec(`INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,1,0,0)`); err != nil {
		t.Fatal(err)
	}
	grants, err := identity.NewGrants(db, lineageViewer{}, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	quick, err := identity.NewQuickConnect(db, time.Minute, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	auth := lineageIdentity{grants: grants, quick: quick}
	seal, err := encryption(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	newInstance := func() *instance {
		return &instance{ctx: ctx, cancel: cancel, db: db, seal: seal, host: frontend.Host{Identity: auth}, mux: http.NewServeMux(), replays: map[string]*replay{}, sessions: map[string]*playSession{}}
	}
	i := newInstance()
	i.routes()
	call := func(i *instance, path, token string, b object) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(b)
		r := httptest.NewRequest("POST", "/vela/v0"+path, strings.NewReader(string(raw)))
		r.Header.Set("X-Vela-Device-Id", "device")
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		i.serve(w, r)
		return w
	}
	d := device{ID: "device", AppVersion: "0.1.0"}
	code, secret, err := quick.Initiate(ctx, "vela", d.neutral())
	if err != nil {
		t.Fatal(err)
	}
	if err = quick.ApproveSurface(ctx, "vela", code, 1); err != nil {
		t.Fatal(err)
	}
	type tokenPair struct{ AccessToken, RefreshToken string }
	poll := func(i *instance, key string) tokenPair {
		w := call(i, "/auth/device/poll", "", object{"idempotencyKey": key, "deviceCode": secret, "device": d})
		var reply struct {
			Status string
			Tokens tokenPair
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &reply) != nil || reply.Status != "approved" {
			t.Fatalf("poll: %d %s", w.Code, w.Body.String())
		}
		return reply.Tokens
	}
	first := poll(i, "poll-a")
	// A new polling key and a remounted adapter must recover the whole pair,
	// even if the first HTTP response never reached the client.
	i = newInstance()
	i.routes()
	second := poll(i, "poll-b")
	if first != second || first.RefreshToken == "" {
		t.Fatal("approved replay changed token family")
	}
	var count int
	if err = db.QueryRow(`SELECT COUNT(*) FROM identity_refresh_families WHERE surface='vela'`).Scan(&count); err != nil || count != 1 {
		t.Fatal("pairing minted multiple families", count, err)
	}
	scope, err := auth.GrantScope(ctx, second.AccessToken, d.neutral())
	if err != nil {
		t.Fatal(err)
	}
	i.sessions["play"] = &playSession{scope: scope, viewer: 1, device: d.ID, plan: object{"sessionId": "play"}}
	calls := 0
	i.route("POST", "/lineage", func(w http.ResponseWriter, r *http.Request) { calls++; send(w, 200, object{"accepted": true}) })
	before := call(i, "/lineage", second.AccessToken, object{"idempotencyKey": "same-operation"})
	if before.Code != 200 {
		t.Fatal(before.Body.String())
	}
	refreshed := call(i, "/auth/refresh", "", object{"idempotencyKey": "refresh", "refreshToken": second.RefreshToken, "device": d})
	var next tokenPair
	if refreshed.Code != 200 || json.Unmarshal(refreshed.Body.Bytes(), &next) != nil {
		t.Fatal("refresh failed", refreshed.Body.String())
	}
	after := call(i, "/lineage", next.AccessToken, object{"idempotencyKey": "same-operation"})
	if after.Code != 200 || calls != 1 || after.Body.String() != before.Body.String() {
		t.Fatal("rotation lost mutation replay", after.Body.String())
	}
	// A reused rotated access grant must recover its existing refresh result,
	// rather than creating another family through a later sign-in response.
	response := httptest.NewRecorder()
	i.tokens(response, httptest.NewRequest("POST", "/", nil), next.AccessToken, "", d.neutral())
	var reused tokenPair
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &reused) != nil || reused != next {
		t.Fatal("rotated grant did not reuse complete token result", response.Body.String())
	}
	if err = db.QueryRow(`SELECT COUNT(*) FROM identity_refresh_families WHERE surface='vela'`).Scan(&count); err != nil || count != 1 {
		t.Fatal("rotated access minted another family", count, err)
	}
	nextScope, err := auth.GrantScope(ctx, next.AccessToken, d.neutral())
	if err != nil || nextScope != scope {
		t.Fatal("rotation changed login scope", err)
	}
	if _, err = auth.GrantScopeCurrent(ctx, scope, d.neutral()); err != nil {
		t.Fatal("rotation ended session lineage", err)
	}
	r := httptest.NewRequest("GET", "/vela/v0/sessions/play", nil)
	r.SetPathValue("id", "play")
	r.Header.Set("Authorization", "Bearer "+next.AccessToken)
	r.Header.Set("X-Vela-Device-Id", d.ID)
	w := httptest.NewRecorder()
	_, session := i.session(w, r)
	if session == nil {
		t.Fatal("rotated session inaccessible", w.Body.String())
	}
	if err = auth.Revoke(ctx, next.AccessToken); err != nil {
		t.Fatal(err)
	}
	if _, err = auth.GrantScopeCurrent(ctx, scope, d.neutral()); !errors.Is(err, frontend.ErrUnauthorized) {
		t.Fatal("logout did not revoke lineage", err)
	}
}
