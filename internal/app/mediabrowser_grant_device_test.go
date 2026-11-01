package app

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/plextv"
)

func TestMediaBrowserRealGrantDeviceTokenOnly(t *testing.T) {
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
	if err := local.Create(ctx, owner.ID, "viewer", "password-1234"); err != nil {
		t.Fatal(err)
	}
	key := core.MovieKey(23)
	title := core.Title{Key: key, Type: core.TypeMovie, IDs: core.IDs{TMDB: 23}, Title: "Fixture Film", Released: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)}
	if err := a.catalog.UpsertTitle(ctx, title, true); err != nil {
		t.Fatal(err)
	}
	for _, surface := range []core.Protocol{"jellyfin", "emby"} {
		t.Run(string(surface), func(t *testing.T) {
			inst, err := a.frontends.registry.Mount(ctx, surface)
			if err != nil {
				t.Fatal(err)
			}
			defer a.frontends.registry.Unmount(ctx, surface)
			call := func(method, path, body, credential, claim string) *httptest.ResponseRecorder {
				r := httptest.NewRequest(method, path, strings.NewReader(body))
				r.RemoteAddr = "127.0.0.1:1234"
				if credential != "" {
					r.Header.Set("X-Emby-Token", credential)
				}
				if claim != "" {
					r.Header.Set("X-Emby-Device-Id", claim)
				}
				w := httptest.NewRecorder()
				inst.Handler().ServeHTTP(w, r)
				return w
			}
			login := call("POST", "/Users/AuthenticateByName", `{"Username":"viewer","Pw":"password-1234"}`, "", "stored-device")
			var signed struct{ AccessToken string }
			if login.Code != 200 || json.Unmarshal(login.Body.Bytes(), &signed) != nil || signed.AccessToken == "" {
				t.Fatal("login", login.Code)
			}
			detail := call("GET", "/Items/"+ids.RatingKeyGUID(key), "", signed.AccessToken, "")
			var body struct {
				Sources []struct {
					ID string `json:"Id"`
				} `json:"MediaSources"`
			}
			if detail.Code != 200 || json.Unmarshal(detail.Body.Bytes(), &body) != nil || len(body.Sources) == 0 || body.Sources[0].ID != ids.RatingKeyGUID(key) {
				t.Fatal("token-only detail", detail.Code, detail.Body.String())
			}
			play := call("POST", "/Items/"+ids.RatingKeyGUID(key)+"/PlaybackInfo", `{"IsPlayback":false}`, signed.AccessToken, "")
			if play.Code != 200 {
				t.Fatal("token-only estimate", play.Code, play.Body.String())
			}
			if bad := call("GET", "/Items/"+ids.RatingKeyGUID(key), "", signed.AccessToken, "different-device"); bad.Code != 401 {
				t.Fatal("mismatched device", bad.Code)
			}
			if logout := call("POST", "/Sessions/Logout", "", signed.AccessToken, ""); logout.Code != 204 {
				t.Fatal("logout", logout.Code)
			}
			if revoked := call("GET", "/Items/"+ids.RatingKeyGUID(key), "", signed.AccessToken, ""); revoked.Code != 401 {
				t.Fatal("revoked token", revoked.Code)
			}
		})
	}
}
