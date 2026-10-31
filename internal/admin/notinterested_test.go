package admin_test

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/auth"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/store"
)

type fakeNotInterestedPortal struct {
	viewer   int64
	restored bool
}

func (f *fakeNotInterestedPortal) ListNotInterested(_ context.Context, u *core.User) ([]admin.NotInterestedEntry, error) {
	f.viewer = u.ID
	return []admin.NotInterestedEntry{{Handle: "opaque"}}, nil
}
func (f *fakeNotInterestedPortal) RestoreNotInterested(_ context.Context, u *core.User, handle string) (bool, error) {
	f.viewer = u.ID
	f.restored = handle == "opaque"
	return f.restored, nil
}
func TestNotInterestedPortalAuthorizationAndHiddenEntries(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	users, err := auth.New(ctx, db, auth.Options{})
	if err != nil {
		t.Fatal(err)
	}
	local, _ := identity.NewLocal(db)
	id, err := local.CreateViewer(ctx, "viewer", "password")
	if err != nil {
		t.Fatal(err)
	}
	if err = users.RefreshViewers(ctx); err != nil {
		t.Fatal(err)
	}
	grants, _ := identity.NewGrants(db, users, time.Hour, nil)
	token, err := grants.Issue(ctx, id, nil, "viewer-portal", core.Device{})
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeNotInterestedPortal{}
	h := admin.New(admin.Deps{Config: newStore(t, nil), IdentityGrants: grants, NotInterested: fake})
	r := req("GET", "/ether/api/me/not-interested", "")
	r.Header.Set("Authorization", "Bearer "+token)
	res := serve(h, r)
	var got struct {
		Items []map[string]any `json:"items"`
	}
	if res.Code != 200 || json.Unmarshal(res.Body.Bytes(), &got) != nil || len(got.Items) != 1 || len(got.Items[0]) != 1 || fake.viewer != id {
		t.Fatalf("hidden response %d %s", res.Code, res.Body.String())
	}
	r = req("POST", "/ether/api/me/not-interested/restore", `{"handle":"opaque"}`)
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Del("X-Ether-Request")
	if res = serve(h, r); res.Code != 403 || fake.restored {
		t.Fatalf("csrf %d", res.Code)
	}
	r.Header.Set("X-Ether-Request", "1")
	if res = serve(h, r); res.Code != 200 || !fake.restored {
		t.Fatalf("restore %d %s", res.Code, res.Body.String())
	}
	r = req("GET", "/ether/api/me/not-interested", "")
	if res = serve(h, r); res.Code != 401 {
		t.Fatalf("unauthenticated %d", res.Code)
	}
}

func TestOwnerListsHiddenRecommendationsForAViewer(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	users, err := auth.New(ctx, db, auth.Options{})
	if err != nil {
		t.Fatal(err)
	}
	local, _ := identity.NewLocal(db)
	ownerID, err := local.CreateOwner(ctx, "owner", "password")
	if err != nil {
		t.Fatal(err)
	}
	viewerID, err := local.CreateViewer(ctx, "viewer", "password")
	if err != nil {
		t.Fatal(err)
	}
	if err = users.RefreshViewers(ctx); err != nil {
		t.Fatal(err)
	}
	grants, _ := identity.NewGrants(db, users, time.Hour, nil)
	owner, err := grants.Issue(ctx, ownerID, nil, "owner", core.Device{})
	if err != nil {
		t.Fatal(err)
	}
	portal, err := grants.Issue(ctx, viewerID, nil, "viewer-portal", core.Device{})
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeNotInterestedPortal{}
	h := admin.New(admin.Deps{Config: newStore(t, nil), IdentityGrants: grants, NotInterested: fake, ActiveViewer: users.ActiveUser})
	r := req("GET", "/ether/api/viewers/"+strconv.FormatInt(viewerID, 10)+"/not-interested", "")
	r.Header.Set("Authorization", "Bearer "+portal)
	if res := serve(h, r); res.Code == 200 {
		t.Fatal("portal token reached the owner list")
	}
	r = req("GET", "/ether/api/viewers/"+strconv.FormatInt(viewerID, 10)+"/not-interested", "")
	r.Header.Set("Authorization", "Bearer "+owner)
	res := serve(h, r)
	if res.Code != 200 || fake.viewer != viewerID {
		t.Fatalf("owner list %d %s viewer %d", res.Code, res.Body.String(), fake.viewer)
	}
	r = req("POST", "/ether/api/viewers/"+strconv.FormatInt(viewerID, 10)+"/not-interested/restore", `{"handle":"opaque"}`)
	r.Header.Set("Authorization", "Bearer "+owner)
	if res = serve(h, r); res.Code != 200 || !fake.restored {
		t.Fatalf("owner restore %d %s", res.Code, res.Body.String())
	}
}
