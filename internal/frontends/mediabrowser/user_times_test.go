package mediabrowser

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/conform/openapi"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
)

func TestUserTimesUseIssuedAndRequestCredentials(t *testing.T) {
	for _, protocol := range []frontend.Protocol{"emby", "jellyfin"} {
		t.Run(string(protocol), func(t *testing.T) {
			viewer := &frontend.Viewer{ID: 7}
			login := time.Date(2025, 1, 2, 3, 4, 5, 123456700, time.UTC)
			activity := login.Add(time.Second)
			unavailable := false
			calls := 0
			identity := frontendtest.Identity{
				SignInFunc: func(context.Context, string, string, frontend.Device) (string, error) { return "issued-grant", nil },
				ResolveFunc: func(_ context.Context, token string) (*frontend.Viewer, error) {
					if token != "issued-grant" {
						return nil, errors.New("invalid")
					}
					return viewer, nil
				},
				ViewerTimesFunc: func(_ context.Context, credential string, id int64) (frontend.ViewerTimes, error) {
					calls++
					if credential != "issued-grant" || id != viewer.ID || unavailable {
						return frontend.ViewerTimes{}, errors.New("denied")
					}
					return frontend.ViewerTimes{LastLogin: login, LastActivity: activity}, nil
				},
			}
			inst, err := mount(context.Background(), frontend.Host{Identity: identity}, "test", protocol)
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest("POST", "/Users/AuthenticateByName", strings.NewReader(`{"Username":"user","Pw":"password"}`))
			w := httptest.NewRecorder()
			inst.Handler().ServeHTTP(w, request)
			var auth struct{ User map[string]any }
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &auth) != nil || auth.User["LastLoginDate"] != wireTime(login) || auth.User["LastActivityDate"] != wireTime(activity) {
				t.Fatal(w.Code, w.Body.String())
			}
			spec, err := openapi.CachedSpec("../../conform/specs/manifest.json", string(protocol), "")
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if err == nil {
				doc, err := openapi.Parse(spec)
				if err != nil {
					t.Fatal(err)
				}
				if err := doc.Validate("POST", "/Users/AuthenticateByName", w.Code, w.Header().Get("Content-Type"), w.Body.Bytes()); err != nil {
					t.Fatal(err)
				}
			}
			request = httptest.NewRequest("GET", "/Users/Me", nil)
			request.Header.Set("X-Emby-Token", "issued-grant")
			w = httptest.NewRecorder()
			inst.Handler().ServeHTTP(w, request)
			var me map[string]any
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &me) != nil || me["LastLoginDate"] != wireTime(login) || calls != 2 {
				t.Fatal(w.Code, w.Body.String(), calls)
			}
			unavailable = true
			w = httptest.NewRecorder()
			inst.Handler().ServeHTTP(w, request)
			me = nil
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &me) != nil {
				t.Fatal(w.Code, w.Body.String())
			}
			if _, ok := me["LastLoginDate"]; ok {
				t.Fatal("unavailable times were emitted", me)
			}
			if _, ok := me["LastActivityDate"]; ok {
				t.Fatal("unavailable times were emitted", me)
			}
			identity.ViewerTimesFunc = func(context.Context, string, int64) (frontend.ViewerTimes, error) { return frontend.ViewerTimes{}, nil }
			empty := (&instance{protocol: protocol, host: frontend.Host{Identity: identity}}).userDTOWithTimes(request, viewer, "issued-grant")
			if _, ok := empty["LastLoginDate"]; ok {
				t.Fatal("zero login was emitted", empty)
			}
			if _, ok := empty["LastActivityDate"]; ok {
				t.Fatal("zero activity was emitted", empty)
			}
		})
	}
}
