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
	"time"

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

func TestQuickConnectStatusDoesNotRedeem(t *testing.T) {
	approved := false
	redeemed := 0
	ident := frontendtest.Identity{
		ResolveFunc: func(_ context.Context, token string) (*frontend.Viewer, error) {
			if token == "grant" {
				return &frontend.Viewer{ID: 1, Username: "viewer"}, nil
			}
			return nil, errors.New("bad grant")
		},
		InitiateFunc: func(context.Context, frontend.Device) (string, string, error) { return "123456", "secret", nil },
		DetailsFunc: func(_ context.Context, secret string) (frontend.QuickConnectDetails, error) {
			if secret != "secret" {
				return frontend.QuickConnectDetails{}, errors.New("invalid")
			}
			return frontend.QuickConnectDetails{Approved: approved, Secret: secret, Code: "123456", CreatedAt: time.Now()}, nil
		},
		ApproveFunc: func(_ context.Context, v *frontend.Viewer, code string) error {
			if v.ID != 1 || code != "123456" {
				return errors.New("invalid")
			}
			approved = true
			return nil
		},
		PollFunc: func(_ context.Context, secret string) (string, error) {
			if secret != "secret" || !approved || redeemed != 0 {
				return "", errors.New("invalid")
			}
			redeemed++
			return "grant", nil
		},
	}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: ident})
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body, token string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("X-Emby-Token", token)
		inst.Handler().ServeHTTP(w, r)
		return w
	}
	if w := call("POST", "/QuickConnect/Initiate", "", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"Code":"123456"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := call("GET", "/QuickConnect/Connect?secret=secret", "", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"Authenticated":false`) {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := call("POST", "/QuickConnect/Authorize?code=123456", "", "grant"); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	for j := 0; j < 2; j++ {
		if w := call("GET", "/QuickConnect/Connect?secret=secret", "", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"Authenticated":true`) {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if redeemed != 0 {
		t.Fatal("status consumed grant")
	}
	if w := call("POST", "/Users/AuthenticateWithQuickConnect", `{"Secret":"secret"}`, ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if redeemed != 1 {
		t.Fatal(redeemed)
	}
}

func TestQuickConnectOwnerSetting(t *testing.T) {
	enabled := false
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: frontendtest.Identity{}, Config: func() frontend.Settings { return frontend.Settings{"quickConnect": enabled} }})
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		inst.Handler().ServeHTTP(w, httptest.NewRequest(method, path, nil))
		return w
	}
	if w := call("GET", "/QuickConnect/Enabled"); w.Body.String() != "false\n" {
		t.Fatal(w.Body.String())
	}
	if w := call("POST", "/QuickConnect/Initiate"); w.Code != 401 {
		t.Fatal(w.Code)
	}
	enabled = true
	if w := call("GET", "/QuickConnect/Enabled"); w.Body.String() != "true\n" {
		t.Fatal(w.Body.String())
	}
}
