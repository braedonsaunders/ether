package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/plextv"
)

type quickPlexPIN struct{}

func (quickPlexPIN) StartPIN(context.Context, string) (string, string, string, error) {
	return "pin", "CODE", "https://app.plex.tv/auth", nil
}
func (quickPlexPIN) PINToken(context.Context, string) (string, error) {
	return "plex-viewer-token", nil
}
func TestJellyfinQuickConnectPlexPortal(t *testing.T) {
	a := setupTestApp(t, true)
	ctx := context.Background()
	if err := a.users.SetOwner(ctx, plextv.Account{ID: 42, Username: "owner"}, "plex-viewer-token"); err != nil {
		t.Fatal(err)
	}
	owner, err := a.users.Owner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	grants, err := identity.NewGrants(a.db, a.users, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	quick, err := identity.NewQuickConnect(a.db, time.Minute, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	limiter, err := identity.NewLoginLimiter(a.db, time.Minute, 100, 20, nil)
	if err != nil {
		t.Fatal(err)
	}
	pollLimiter, err := identity.NewPollLimiter(a.db, time.Minute, 100, 20, nil)
	if err != nil {
		t.Fatal(err)
	}
	portal := admin.New(admin.Deps{PollLimiter: pollLimiter, Config: a.cfg, IdentityGrants: grants, QuickConnect: quick, IdentityLimiter: limiter, SignIn: quickPlexPIN{}, AuthenticatePlex: func(_ context.Context, token string) (*core.User, error) {
		if token != "plex-viewer-token" {
			return nil, core.ErrUnauthorized
		}
		return owner, nil
	}})
	inst, err := a.frontends.registry.Mount(ctx, "jellyfin")
	if err != nil {
		t.Fatal(err)
	}
	defer a.frontends.registry.Unmount(ctx, "jellyfin")
	request := func(h http.Handler, method, path, body, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.RemoteAddr = "192.0.2.20:1234"
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Ether-Request", "1")
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	start := request(portal, "POST", "/ether/api/me/pin", "{}", "")
	if start.Code != 200 {
		t.Fatalf("start: %d %s", start.Code, start.Body)
	}
	var pin struct{ Handle string }
	if err = json.Unmarshal(start.Body.Bytes(), &pin); err != nil {
		t.Fatal(err)
	}
	poll := request(portal, "GET", "/ether/api/me/pin/"+pin.Handle, "", "")
	if poll.Code != 200 {
		t.Fatalf("poll: %d %s", poll.Code, poll.Body)
	}
	var login struct{ Token string }
	if err = json.Unmarshal(poll.Body.Bytes(), &login); err != nil || login.Token == "" {
		t.Fatal(login, err)
	}
	init := request(inst.Handler(), "POST", "/QuickConnect/Initiate", "", "")
	if init.Code != 200 {
		t.Fatalf("init: %d %s", init.Code, init.Body)
	}
	var result struct{ Code, Secret string }
	if err = json.Unmarshal(init.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	approved := request(portal, "POST", "/ether/api/me/quickconnect", `{"code":"`+result.Code+`"}`, login.Token)
	if approved.Code != 200 {
		t.Fatalf("approve: %d %s", approved.Code, approved.Body)
	}
	redeem := request(inst.Handler(), "POST", "/Users/AuthenticateWithQuickConnect", `{"Secret":"`+result.Secret+`"}`, "")
	if redeem.Code != 200 {
		t.Fatalf("redeem: %d %s", redeem.Code, redeem.Body)
	}
	var auth struct{ AccessToken string }
	if err = json.Unmarshal(redeem.Body.Bytes(), &auth); err != nil {
		t.Fatal(err)
	}
	viewer, err := grants.Resolve(ctx, "jellyfin", auth.AccessToken)
	if err != nil || viewer.ID != owner.ID {
		t.Fatal(viewer, err)
	}
	if _, err = grants.Resolve(ctx, "emby", auth.AccessToken); err == nil {
		t.Fatal("grant escaped Jellyfin surface")
	}
}
