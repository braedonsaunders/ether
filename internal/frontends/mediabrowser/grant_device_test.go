package mediabrowser

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

func TestGrantDeviceEnablesTokenOnlyAndRejectsMismatch(t *testing.T) {
	viewer := &frontend.Viewer{ID: 1}
	identity := frontendtest.GrantDeviceIdentity{
		Identity: frontendtest.Identity{ResolveFunc: func(_ context.Context, credential string) (*frontend.Viewer, error) {
			if credential == "revoked" {
				return nil, errors.New("revoked")
			}
			return viewer, nil
		}},
		ResolveGrantDeviceFunc: func(_ context.Context, credential, claim string) (*frontend.Viewer, frontend.Device, time.Time, error) {
			if credential != "valid" || (claim != "" && claim != "stored-device") {
				return nil, frontend.Device{}, time.Time{}, errors.New("unauthorized")
			}
			return viewer, frontend.Device{ID: "stored-device", Client: "Client", Product: "Client"}, time.Now().Add(time.Hour), nil
		},
	}
	experience := frontendtest.Experience{ItemFunc: func(_ context.Context, req frontend.ItemRequest) (frontend.ItemDetail, error) {
		if req.Device.ID != "stored-device" {
			t.Fatal("missing verified binding", req.Device)
		}
		return frontend.ItemDetail{Title: frontend.Title{Key: 5, Type: "movie"}, Versions: []frontend.Version{{ID: "private-release"}}}, nil
	}}
	for _, dialect := range []frontend.Frontend{Jellyfin{MachineID: "test"}, Emby{MachineID: "test"}} {
		var planned frontend.PlanRequest
		sessions := frontendtest.Sessions{PlanFunc: func(_ context.Context, req frontend.PlanRequest) (frontend.PlayPlan, error) {
			planned = req
			return frontend.PlayPlan{Estimated: true, DirectPlay: true}, nil
		}}
		lazy := &lazyStub{path: "/sessions/lazy/scoped/file"}
		inst, err := dialect.Mount(context.Background(), frontend.Host{Identity: identity, Experience: experience, Sessions: sessions, Capabilities: frontendtest.Capabilities{}, VersionHandles: missingVersionHandles{}, LazyStreams: lazy, SurfaceURL: func(context.Context, *http.Request) (string, error) { return "https://ether.example", nil }})
		if err != nil {
			t.Fatal(err)
		}
		call := func(credential, claim string) *httptest.ResponseRecorder {
			r := httptest.NewRequest("GET", "/Items/"+ids.RatingKeyGUID(5), nil)
			r.Header.Set("X-Emby-Token", credential)
			if claim != "" {
				r.Header.Set("X-Emby-Device-Id", claim)
			}
			w := httptest.NewRecorder()
			inst.Handler().ServeHTTP(w, r)
			return w
		}
		for _, claim := range []string{"", "stored-device"} {
			w := call("valid", claim)
			if w.Code != 200 || planned.Device.ID != "stored-device" || lazy.seen.Device.ID != "stored-device" {
				t.Fatal(claim, w.Code, planned.Device, lazy.seen.Device)
			}
		}
		if w := call("valid", "other-device"); w.Code != 401 {
			t.Fatal("mismatched claim accepted", w.Code)
		}
		if w := call("revoked", ""); w.Code != 401 {
			t.Fatal("revoked grant accepted", w.Code)
		}
	}
}
