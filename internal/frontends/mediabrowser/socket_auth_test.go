package mediabrowser

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
)

func TestSocketDistinguishesMissingAndInvalidCredential(t *testing.T) {
	for _, tc := range []struct {
		name  string
		front frontend.Frontend
	}{{"jellyfin", Jellyfin{MachineID: "test"}}, {"emby", Emby{MachineID: "test"}}} {
		identity := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return nil, errors.New("expired") }}
		inst, err := tc.front.Mount(context.Background(), frontend.Host{Identity: identity})
		if err != nil {
			t.Fatal(err)
		}
		for _, test := range []struct {
			token  string
			status int
		}{{"", http.StatusForbidden}, {"invalid-or-expired", http.StatusUnauthorized}} {
			r := httptest.NewRequest("GET", "/socket", nil)
			if test.token != "" {
				r.Header.Set("X-Emby-Token", test.token)
			}
			w := httptest.NewRecorder()
			inst.Handler().ServeHTTP(w, r)
			if w.Code != test.status {
				t.Fatal(tc.name, test.token, w.Code)
			}
		}
	}
}
