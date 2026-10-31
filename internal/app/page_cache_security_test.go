package app

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/plextv"
	"github.com/braedonsaunders/ether/internal/safety"
)

// A cached browse page is a viewer's body. The next request after a rating
// cap, a section limit, a safety policy, or a revoked token must not reuse it.
func TestPageCacheDropsBodyWhenRestrictionOrTokenChanges(t *testing.T) {
	a := setupTestApp(t, true)
	ctx := context.Background()
	if err := a.users.SetOwner(ctx, plextv.Account{ID: 42, Username: "owner"}, "owner-token"); err != nil {
		t.Fatal(err)
	}
	local, err := identity.NewLocal(a.db)
	if err != nil {
		t.Fatal(err)
	}
	id, err := local.CreateViewer(ctx, "child", "password")
	if err != nil {
		t.Fatal(err)
	}
	if err = a.users.RefreshViewers(ctx); err != nil {
		t.Fatal(err)
	}
	released := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	titles := []core.Title{
		{Key: core.MovieKey(11), Type: core.TypeMovie, IDs: core.IDs{TMDB: 11}, Title: "Kids Film", ContentRating: "G", Released: released},
		{Key: core.MovieKey(12), Type: core.TypeMovie, IDs: core.IDs{TMDB: 12}, Title: "Adult Film", ContentRating: "R", Released: released},
		{Key: core.ShowKey(13), Type: core.TypeShow, IDs: core.IDs{TMDB: 13}, Title: "Kids Show", ContentRating: "TV-G", Released: released},
	}
	for _, title := range titles {
		if err = a.catalog.UpsertTitle(ctx, title, true); err != nil {
			t.Fatal(err)
		}
	}
	inst, err := a.frontends.registry.Mount(ctx, core.Protocol("jellyfin"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.frontends.registry.Unmount(ctx, core.Protocol("jellyfin"))
	call := func(method, path, body, token string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.RemoteAddr = "192.0.2.40:1234"
		r.Header.Set("Authorization", `MediaBrowser Client="Scorecard", Device="tv", DeviceId="child-tv", Version="1", Token="`+token+`"`)
		inst.Handler().ServeHTTP(w, r)
		return w
	}
	login := call("POST", "/Users/AuthenticateByName", `{"Username":"child","Pw":"password"}`, "")
	if login.Code != 200 {
		t.Fatalf("login %d %s", login.Code, login.Body)
	}
	var signed struct{ AccessToken string }
	if err = json.Unmarshal(login.Body.Bytes(), &signed); err != nil || signed.AccessToken == "" {
		t.Fatal(err, login.Body)
	}
	const list = "/Items?Recursive=true&IncludeItemTypes=Movie,Series&Limit=50"
	open := call("GET", list, "", signed.AccessToken)
	if open.Code != 200 || !strings.Contains(open.Body.String(), "Kids Film") || !strings.Contains(open.Body.String(), "Adult Film") || !strings.Contains(open.Body.String(), "Kids Show") {
		t.Fatalf("open list %d %s", open.Code, open.Body)
	}
	child, err := a.users.ActiveUser(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	child.MaxContentRating = "G"
	if err = a.users.UpdateUser(ctx, *child); err != nil {
		t.Fatal(err)
	}
	capped := call("GET", list, "", signed.AccessToken)
	if capped.Code != 200 || strings.Contains(capped.Body.String(), "Adult Film") || !strings.Contains(capped.Body.String(), "Kids Film") || !strings.Contains(capped.Body.String(), "Kids Show") {
		t.Fatalf("rating cap %d %s", capped.Code, capped.Body)
	}
	child, err = a.users.ActiveUser(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	child.AllowedSections = []int{1}
	if err = a.users.UpdateUser(ctx, *child); err != nil {
		t.Fatal(err)
	}
	sectioned := call("GET", list, "", signed.AccessToken)
	if sectioned.Code != 200 || strings.Contains(sectioned.Body.String(), "Kids Show") || strings.Contains(sectioned.Body.String(), "Adult Film") || !strings.Contains(sectioned.Body.String(), "Kids Film") {
		t.Fatalf("section limit %d %s", sectioned.Code, sectioned.Body)
	}
	child, err = a.users.ActiveUser(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	child.MaxContentRating = ""
	child.AllowedSections = nil
	if err = a.users.UpdateUser(ctx, *child); err != nil {
		t.Fatal(err)
	}
	restored := call("GET", list, "", signed.AccessToken)
	if restored.Code != 200 || !strings.Contains(restored.Body.String(), "Adult Film") {
		t.Fatalf("restored %d %s", restored.Code, restored.Body)
	}
	if err = a.safety.Put(ctx, id, safety.Policy{MaxContentRating: "G"}); err != nil {
		t.Fatal(err)
	}
	policy := call("GET", list, "", signed.AccessToken)
	if policy.Code != 200 || strings.Contains(policy.Body.String(), "Adult Film") || !strings.Contains(policy.Body.String(), "Kids Film") {
		t.Fatalf("safety cap %d %s", policy.Code, policy.Body)
	}
	logout := call("POST", "/Sessions/Logout", "", signed.AccessToken)
	if logout.Code != 200 && logout.Code != 204 {
		t.Fatalf("logout %d %s", logout.Code, logout.Body)
	}
	revoked := call("GET", list, "", signed.AccessToken)
	if revoked.Code != 401 || strings.Contains(revoked.Body.String(), "Kids Film") || strings.Contains(revoked.Body.String(), "Adult Film") {
		t.Fatalf("revoked %d %s", revoked.Code, revoked.Body)
	}
}
