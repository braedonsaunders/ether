package mediabrowser

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
)

func TestQueryCapabilitiesNativeClient(t *testing.T) {
	for _, tc := range []struct {
		name  string
		front frontend.Frontend
	}{{"jellyfin", Jellyfin{MachineID: "test"}}, {"emby", Emby{MachineID: "test"}}} {
		viewer := &frontend.Viewer{ID: 1}
		var seen frontend.SessionCapabilities
		var access frontend.SessionAccess
		calls := 0
		identity := frontendtest.Identity{ResolveFunc: func(_ context.Context, token string) (*frontend.Viewer, error) {
			if token != "valid" {
				return nil, errors.New("unauthorized")
			}
			return viewer, nil
		}}
		sessions := frontendtest.Sessions{UpdateCapabilitiesFunc: func(_ context.Context, a frontend.SessionAccess, c frontend.SessionCapabilities) error {
			access, seen, calls = a, c, calls+1
			return nil
		}}
		inst, err := tc.front.Mount(context.Background(), frontend.Host{Identity: identity, Sessions: sessions})
		if err != nil {
			t.Fatal(err)
		}
		for _, test := range []struct {
			token, query string
			status       int
		}{{"valid", "playableMediaTypes=Video&supportedCommands=DisplayContent&supportedCommands=Play&supportsMediaControl=true", 204}, {"invalid", "", 401}, {"valid", "supportsMediaControl=bad", 400}} {
			r := httptest.NewRequest("POST", "/Sessions/Capabilities?"+test.query, nil)
			r.Header.Set("Authorization", `MediaBrowser Token="`+test.token+`", DeviceId="device"`)
			w := httptest.NewRecorder()
			inst.Handler().ServeHTTP(w, r)
			if w.Code != test.status {
				t.Fatal(tc.name, w.Code, w.Body.String())
			}
		}
		if calls != 1 || access.Viewer != viewer || access.Device.ID != "device" || access.Protocol != frontend.Protocol(tc.name) || len(seen.PlayableMediaTypes) != 1 || seen.PlayableMediaTypes[0] != "Video" || len(seen.SupportedCommands) != 2 || seen.SupportsMediaControl == nil || !*seen.SupportsMediaControl {
			t.Fatal(tc.name, access, seen, calls)
		}
	}
}
