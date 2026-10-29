package silo

import (
	"context"
	"errors"
	"github.com/braedonsaunders/ether/internal/conform/openapi"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
)

func TestPasswordAndRefresh(t *testing.T) {
	v := &frontend.Viewer{ID: 7, Username: "alice"}
	var revoked string
	ident := frontendtest.Identity{SignInFunc: func(ctx context.Context, u, p string, d frontend.Device) (string, error) {
		ip, ok := frontend.ClientIP(ctx)
		if !ok || ip.String() != "192.0.2.1" || u != "alice" || p != "pw" || d.ID != "tv" {
			return "", errors.New("bad")
		}
		return "access", nil
	}, ResolveFunc: func(_ context.Context, token string) (*frontend.Viewer, error) {
		if token != "access" {
			return nil, errors.New("bad")
		}
		return v, nil
	}, IssueRefreshFunc: func(_ context.Context, token string, d frontend.Device) (string, error) {
		if token != "access" || d.ID != "tv" {
			return "", errors.New("bad")
		}
		return "refresh", nil
	}, RefreshFunc: func(_ context.Context, token string, d frontend.Device) (string, string, error) {
		if token != "refresh" || d.ID != "tv" {
			return "", "", errors.New("bad")
		}
		return "next", "next-refresh", nil
	}, RevokeFunc: func(_ context.Context, token string) error { revoked = token; return nil }}
	inst, err := (Frontend{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: ident, ClientIP: func(*http.Request) (netip.Addr, bool) { return netip.MustParseAddr("192.0.2.1"), true }})
	if err != nil {
		t.Fatal(err)
	}
	call := func(path, body, auth string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", path, strings.NewReader(body))
		r.Header.Set("Authorization", auth)
		r.Header.Set("X-Silo-Device-Id", "tv")
		w := httptest.NewRecorder()
		inst.Handler().ServeHTTP(w, r)
		return w
	}
	spec, e := openapi.CachedSpec("../../conform/specs/manifest.json", "silo", "")
	var doc *openapi.Document
	if e == nil {
		doc, e = openapi.Parse(spec)
	}
	if e != nil && !os.IsNotExist(e) {
		t.Fatal(e)
	}
	w := call("/api/v2/auth/login", `{"username":"alice","password":"pw"}`, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"refresh_token":"refresh"`) {
		t.Fatalf("login: %d %s", w.Code, w.Body.String())
	}
	if doc != nil {
		if err := doc.Validate("POST", "/api/v2/auth/login", w.Code, w.Header().Get("Content-Type"), w.Body.Bytes()); err != nil {
			t.Errorf("login spec: %v", err)
		}
	}
	w = call("/api/v2/auth/refresh", `{"refresh_token":"refresh"}`, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"access_token":"next"`) {
		t.Fatalf("refresh: %d %s", w.Code, w.Body.String())
	}
	if doc != nil {
		if err := doc.Validate("POST", "/api/v2/auth/refresh", w.Code, w.Header().Get("Content-Type"), w.Body.Bytes()); err != nil {
			t.Errorf("refresh spec: %v", err)
		}
	}
	w = call("/api/v2/auth/logout", "", "Bearer access")
	if w.Code != 204 || revoked != "access" {
		t.Fatalf("logout: %d", w.Code)
	}
}

func TestCurrentClientCapabilityDiscovery(t *testing.T) {
	viewer := &frontend.Viewer{ID: 7}
	identity := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
	inst, err := (Frontend{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: identity})
	if err != nil {
		t.Fatal(err)
	}
	var doc *openapi.Document
	if data, e := openapi.CachedSpec("../../conform/specs/manifest.json", "silo", ""); e == nil {
		doc, e = openapi.Parse(data)
		if e != nil {
			t.Fatal(e)
		}
	} else if !os.IsNotExist(e) {
		t.Fatal(e)
	}
	for _, tc := range []struct {
		path, expected string
		authenticated  bool
	}{
		{"/api/v2/auth/providers", `"password_login":true`, false},
		{"/api/v2/auth/oauth/capabilities", `"state":"unsupported"`, false},
		{"/api/v2/auth/external-sign-in/capabilities", `"state":"unsupported"`, false},
		{"/api/v2/playback/sessions/control/capabilities", `"available":false`, true},
	} {
		r := httptest.NewRequest("GET", tc.path, nil)
		if tc.authenticated {
			r.Header.Set("Authorization", "Bearer access")
		}
		w := httptest.NewRecorder()
		inst.Handler().ServeHTTP(w, r)
		if w.Code != 200 || !strings.Contains(w.Body.String(), tc.expected) {
			t.Errorf("%s: %d %s", tc.path, w.Code, w.Body.String())
		}
		if doc != nil {
			if e := doc.Validate("GET", tc.path, w.Code, w.Header().Get("Content-Type"), w.Body.Bytes()); e != nil {
				t.Errorf("%s spec: %v", tc.path, e)
			}
		}
	}
	r := httptest.NewRequest("GET", "/api/v2/playback/sessions/control/capabilities", nil)
	w := httptest.NewRecorder()
	inst.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("control capability without auth: %d", w.Code)
	}
}

func TestDevicePairing(t *testing.T) {
	approved := false
	ident := frontendtest.Identity{
		InitiateFunc: func(_ context.Context, d frontend.Device) (string, string, error) {
			if d.ID != "tv" {
				t.Fatal("wrong device")
			}
			return "ABC234", "secret", nil
		},
		DetailsFunc: func(_ context.Context, s string) (frontend.QuickConnectDetails, error) {
			if s != "secret" {
				return frontend.QuickConnectDetails{}, errors.New("bad")
			}
			return frontend.QuickConnectDetails{Approved: approved, Device: frontend.Device{ID: "tv"}}, nil
		},
		PollFunc: func(context.Context, string) (string, error) { return "access", nil },
		ResolveFunc: func(_ context.Context, s string) (*frontend.Viewer, error) {
			if s != "access" {
				return nil, errors.New("bad")
			}
			return &frontend.Viewer{ID: 7, Username: "alice"}, nil
		},
		IssueRefreshFunc: func(_ context.Context, s string, d frontend.Device) (string, error) {
			if s != "access" || d.ID != "tv" {
				t.Fatal("wrong grant scope")
			}
			return "refresh", nil
		},
	}
	inst, err := (Frontend{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: ident, ViewerPortalURL: func(context.Context, *http.Request) (string, error) { return "https://ether.example/ether/me", nil }})
	if err != nil {
		t.Fatal(err)
	}
	call := func(path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", path, strings.NewReader(body))
		r.Header.Set("X-Silo-Device-Id", "tv")
		w := httptest.NewRecorder()
		inst.Handler().ServeHTTP(w, r)
		return w
	}
	w := call("/api/v2/auth/device/start", `{"device_name":"TV","device_platform":"tvos"}`)
	if w.Code != 201 || !strings.Contains(w.Body.String(), "https://ether.example/ether/me?code=ABC234") {
		t.Fatalf("start %d %s", w.Code, w.Body.String())
	}
	w = call("/api/v2/auth/device/poll", `{"device_code":"secret"}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"pending"`) {
		t.Fatalf("pending %d %s", w.Code, w.Body.String())
	}
	approved = true
	w = call("/api/v2/auth/device/poll", `{"device_code":"secret"}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"refresh_token":"refresh"`) {
		t.Fatalf("approved %d %s", w.Code, w.Body.String())
	}
}
