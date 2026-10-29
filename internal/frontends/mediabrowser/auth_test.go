package mediabrowser

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/conform/openapi"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
)

func TestAuthentication(t *testing.T) {
	v := &frontend.Viewer{ID: 9, Username: "alice"}
	var revoked string
	ident := frontendtest.Identity{ResolveFunc: func(_ context.Context, token string) (*frontend.Viewer, error) {
		if token == "valid" {
			return v, nil
		}
		return nil, errors.New("invalid")
	}, SignInFunc: func(ctx context.Context, user, pw string, d frontend.Device) (string, error) {
		ip, ok := frontend.ClientIP(ctx)
		if !ok || ip.String() != "192.0.2.4" || user != "alice" || pw != "secret" || d.ID != "device" {
			return "", errors.New("invalid")
		}
		return "valid", nil
	}, RevokeFunc: func(_ context.Context, token string) error { revoked = token; return nil }}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: ident, ClientIP: func(*http.Request) (netip.Addr, bool) { return netip.MustParseAddr("192.0.2.4"), true }})
	if err != nil {
		t.Fatal(err)
	}
	spec, e := openapi.CachedSpec("../../conform/specs/manifest.json", "jellyfin", "")
	var doc *openapi.Document
	if e == nil {
		doc, e = openapi.Parse(spec)
	}
	if e != nil && !os.IsNotExist(e) {
		t.Fatal(e)
	}
	cases := []struct {
		method, path, body, auth string
		status                   int
	}{
		{"POST", "/Users/AuthenticateByName", `{"Username":"alice","Pw":"secret"}`, `MediaBrowser Client="TV", DeviceId="device"`, 200},
		{"GET", "/Users/Me", "", `MediaBrowser Token="valid"`, 200},
		{"GET", "/Users/Me", "", "", 401},
		{"GET", "/socket", "", "", 403},
		{"POST", "/Sessions/Logout", "", `MediaBrowser Token="valid"`, 204},
	}
	for _, c := range cases {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
		r.Header.Set("Authorization", c.auth)
		inst.Handler().ServeHTTP(w, r)
		if w.Code != c.status {
			t.Errorf("%s: %d %s", c.path, w.Code, w.Body.String())
			continue
		}
		if doc != nil && c.status == 200 {
			if err := doc.Validate(c.method, c.path, w.Code, w.Header().Get("Content-Type"), w.Body.Bytes()); err != nil {
				t.Errorf("%s: %v", c.path, err)
			}
		}
	}
	if revoked != "valid" {
		t.Fatal("grant not revoked")
	}
}
