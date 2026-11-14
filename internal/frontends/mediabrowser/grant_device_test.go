package mediabrowser

import (
	"context"
	"crypto/sha1"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

// Replay v0.20.0 AuthenticationRepository + SessionRepository: the login
// and first authenticated request deliberately use different SHA-1 IDs.
func TestAndroidTVSignIn(t *testing.T) {
	viewer := &frontend.Viewer{ID: 1, Username: "alice"}
	guid, _ := ids.GUID(ids.User, viewer.ID)
	uuid := guid[:8] + "-" + guid[8:12] + "-" + guid[12:16] + "-" + guid[16:20] + "-" + guid[20:]
	hash := func(user string) string { return fmt.Sprintf("%x", sha1.Sum([]byte("shield-installation+"+user))) }
	var stored frontend.Device
	ident := frontendtest.GrantDeviceIdentity{
		Identity: frontendtest.Identity{
			SignInFunc: func(_ context.Context, user, pw string, d frontend.Device) (string, error) {
				if user != "alice" || pw != "secret" || d.ID != hash("alice") || !frontend.JellyfinAndroidTVClient(d.Client) || d.DeviceName != "SHIELD Android TV" {
					return "", errors.New("bad login")
				}
				stored = d
				return "valid", nil
			},
			ResolveFunc: func(_ context.Context, token string) (*frontend.Viewer, error) {
				if token != "valid" {
					return nil, errors.New("bad token")
				}
				return viewer, nil
			},
		},
		ResolveGrantDeviceFunc: func(_ context.Context, token, claim string) (*frontend.Viewer, frontend.Device, time.Time, error) {
			if token != "valid" || (claim != "" && claim != stored.ID) {
				return nil, frontend.Device{}, time.Time{}, errors.New("device mismatch")
			}
			return viewer, stored, time.Now().Add(time.Hour), nil
		},
	}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: ident})
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body, deviceID, client, token string) int {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		header := fmt.Sprintf(`MediaBrowser Client="%s", Version="0.20.0", DeviceId="%s", Device="SHIELD+Android+TV"`, client, deviceID)
		if token != "" {
			header += `, Token="` + token + `"`
		}
		r.Header.Set("Authorization", header)
		w := httptest.NewRecorder()
		inst.Handler().ServeHTTP(w, r)
		return w.Code
	}
	for _, client := range []string{"Android+TV", "Jellyfin+Android+TV", "Jellyfin+for+Android+TV"} {
		if got := call("POST", "/Users/AuthenticateByName", `{"Username":"alice","Pw":"secret"}`, hash("alice"), client, ""); got != 200 {
			t.Fatal("login", got)
		}
		if got := call("GET", "/Users/Me", "", hash(uuid), client, "valid"); got != 200 {
			t.Fatal("session", got)
		}
		if got := call("GET", "/Users/Me", "", hash(uuid), "Other", "valid"); got != 401 {
			t.Fatal("foreign client", got)
		}
		if got := call("GET", "/Users/Me", "", hash(uuid), client, "revoked"); got != 401 {
			t.Fatal("bad token", got)
		}
	}
}

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
			if w.Code != 200 || planned.Device.ID != "stored-device" || lazy.seen.Device.ID != "stored-device" || !strings.Contains(w.Body.String(), "https://ether.example/sessions/lazy/scoped/file") || strings.Contains(w.Body.String(), "valid") {
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
