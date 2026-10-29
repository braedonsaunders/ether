package app

import (
	"context"
	"encoding/json"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/plextv"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMediaBrowserRealExperience(t *testing.T) {
	a := setupTestApp(t, true)
	ctx := context.Background()
	if err := a.users.SetOwner(ctx, plextv.Account{ID: 42, Username: "owner"}, "owner-token"); err != nil {
		t.Fatal(err)
	}
	owner, err := a.users.Owner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	local, err := identity.NewLocal(a.db)
	if err != nil {
		t.Fatal(err)
	}
	if err = local.Create(ctx, owner.ID, "viewer", "password"); err != nil {
		t.Fatal(err)
	}
	key := core.MovieKey(23)
	title := core.Title{Key: key, Type: core.TypeMovie, IDs: core.IDs{TMDB: 23}, Title: "Fixture Film", Released: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)}
	if err = a.catalog.UpsertTitle(ctx, title, true); err != nil {
		t.Fatal(err)
	}
	for _, surface := range []string{"jellyfin", "emby"} {
		t.Run(surface, func(t *testing.T) {
			inst, err := a.frontends.registry.Mount(ctx, core.Protocol(surface))
			if err != nil {
				t.Fatal(err)
			}
			defer a.frontends.registry.Unmount(ctx, core.Protocol(surface))
			call := func(method, path, body, token string) *httptest.ResponseRecorder {
				w := httptest.NewRecorder()
				r := httptest.NewRequest(method, path, strings.NewReader(body))
				r.RemoteAddr = "192.0.2.20:1234"
				r.Header.Set("Authorization", `MediaBrowser Client="TV", DeviceId="tv", Token="`+token+`"`)
				inst.Handler().ServeHTTP(w, r)
				return w
			}
			login := call("POST", "/Users/AuthenticateByName", `{"Username":"viewer","Pw":"password"}`, "")
			if login.Code != 200 {
				t.Fatalf("login %d %s", login.Code, login.Body)
			}
			var signed struct{ AccessToken string }
			if err = json.Unmarshal(login.Body.Bytes(), &signed); err != nil {
				t.Fatal(err)
			}
			views := call("GET", "/UserViews", "", signed.AccessToken)
			if views.Code != 200 || !strings.Contains(views.Body.String(), "Movies") {
				t.Fatalf("views %d %s", views.Code, views.Body)
			}
			view, _ := ids.GUID(ids.View, 1)
			list := call("GET", "/Items?ParentId="+view, "", signed.AccessToken)
			if list.Code != 200 || !strings.Contains(list.Body.String(), "Fixture Film") {
				t.Fatalf("list %d %s", list.Code, list.Body)
			}
			watched := call("POST", "/UserPlayedItems/"+ids.RatingKeyGUID(key), "", signed.AccessToken)
			if watched.Code != 200 || !strings.Contains(watched.Body.String(), `"Played":true`) {
				t.Fatalf("watched %d %s", watched.Code, watched.Body)
			}
		})
	}
}
