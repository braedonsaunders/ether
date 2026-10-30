package mediabrowser

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

type authenticatedLazyStub struct {
	lazyStub
	plans []frontend.PlanRequest
}

func (s *authenticatedLazyStub) ServeAuthenticated(w http.ResponseWriter, r *http.Request, credential string, req frontend.PlanRequest) {
	if credential != "account-secret" || r.Header.Get("Range") != "bytes=0-5" {
		http.Error(w, "denied", 401)
		return
	}
	s.plans = append(s.plans, req)
	w.WriteHeader(http.StatusPartialContent)
	_, _ = w.Write([]byte("abcdef"))
}

func TestStaticAliasWithoutSessionUsesAuthenticatedLazyBroker(t *testing.T) {
	viewer := &frontend.Viewer{ID: 1}
	handle := "4e0f0a56-014b-4883-89b7-e9465f03462e"
	identity := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
	for _, tc := range []struct {
		name  string
		front frontend.Frontend
	}{{"jellyfin", Jellyfin{MachineID: "test"}}, {"emby", Emby{MachineID: "test"}}} {
		lazy := &authenticatedLazyStub{}
		inst, err := tc.front.Mount(context.Background(), frontend.Host{Identity: identity, Sessions: frontendtest.Sessions{}, Capabilities: frontendtest.Capabilities{}, VersionHandles: &testVersionHandles{item: 5, handle: handle}, LazyStreams: lazy})
		if err != nil {
			t.Fatal(err)
		}
		for _, source := range []string{ids.RatingKeyGUID(5), handle} {
			r := httptest.NewRequest("GET", "/Videos/"+ids.RatingKeyGUID(5)+"/stream?static=true&MediaSourceId="+source, nil)
			r.Header.Set("Authorization", `MediaBrowser Token="account-secret", DeviceId="device"`)
			r.Header.Set("Range", "bytes=0-5")
			w := httptest.NewRecorder()
			inst.Handler().ServeHTTP(w, r)
			if w.Code != 206 || w.Body.String() != "abcdef" {
				t.Fatal(tc.name, w.Code, w.Body.String())
			}
			plan := lazy.plans[len(lazy.plans)-1]
			if plan.Viewer != viewer || plan.Item != 5 || plan.Device.ID != "device" || plan.Protocol != frontend.Protocol(tc.name) || (source == handle && (!plan.Intent.ExactVersion || plan.Version != handle)) || (source != handle && (plan.Intent.ExactVersion || plan.Version != "")) {
				t.Fatal(plan)
			}
		}
	}
}

func TestStaticAliasBindsDetailSourceRevision(t *testing.T) {
	const handle = "82b0c3fc-7616-46da-8528-fc26ec5cb4ab"
	viewer := &frontend.Viewer{ID: 1}
	identity := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
	for _, tc := range []struct {
		name  string
		front frontend.Frontend
	}{{"jellyfin", Jellyfin{MachineID: "test"}}, {"emby", Emby{MachineID: "test"}}} {
		lazy := &authenticatedLazyStub{}
		planner := &detailPlannerStub{embeddedSessions: frontendtest.Sessions{}}
		inst, err := tc.front.Mount(context.Background(), frontend.Host{Identity: identity, Sessions: planner, Capabilities: frontendtest.Capabilities{}, LazyStreams: lazy})
		if err != nil {
			t.Fatal(err)
		}
		for _, device := range []string{"device", "other"} {
			r := httptest.NewRequest("GET", "/Videos/"+ids.RatingKeyGUID(5)+"/stream?static=true&MediaSourceId="+handle, nil)
			r.Header.Set("Authorization", `MediaBrowser Token="account-secret", DeviceId="`+device+`"`)
			r.Header.Set("Range", "bytes=0-5")
			w := httptest.NewRecorder()
			inst.Handler().ServeHTTP(w, r)
			if device == "other" {
				if w.Code != http.StatusNotFound || len(lazy.plans) != 1 {
					t.Fatal(tc.name, w.Code)
				}
				continue
			}
			if w.Code != http.StatusPartialContent || len(lazy.plans) != 1 {
				t.Fatal(tc.name, w.Code, w.Body.String())
			}
			plan := lazy.plans[0]
			if plan.DetailSource != handle || plan.ExpectedSourceETag != "source-revision" || plan.Item != 5 || plan.Version != "" || plan.Intent.ExactVersion {
				t.Fatal(tc.name, plan)
			}
		}
	}
}

func TestStaticAliasChecksDetailSessionBinding(t *testing.T) {
	const handle = "82b0c3fc-7616-46da-8528-fc26ec5cb4ab"
	viewer := &frontend.Viewer{ID: 1}
	identity := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
	for _, tc := range []struct {
		name  string
		front frontend.Frontend
	}{{"jellyfin", Jellyfin{MachineID: "test"}}, {"emby", Emby{MachineID: "test"}}} {
		calls := 0
		sessions := frontendtest.Sessions{SessionsFunc: func(context.Context, frontend.SessionAccess) ([]frontend.Session, error) {
			return []frontend.Session{{ID: "session", Item: 5, Protocol: frontend.Protocol(tc.name), Device: frontend.Device{ID: "device"}}}, nil
		}, StartStreamFunc: func(context.Context, frontend.SessionAccess, string, frontend.StreamRequest) (string, error) {
			calls++
			return "/sessions/media/session/grant/file", nil
		}}
		inst, err := tc.front.Mount(context.Background(), frontend.Host{Identity: identity, Sessions: &detailPlannerStub{embeddedSessions: sessions}, Capabilities: frontendtest.Capabilities{}})
		if err != nil {
			t.Fatal(err)
		}
		for _, test := range []struct {
			session string
			status  int
		}{{"session", http.StatusTemporaryRedirect}, {"other", http.StatusNotFound}} {
			r := httptest.NewRequest("GET", "/Videos/"+ids.RatingKeyGUID(5)+"/stream?static=true&MediaSourceId="+handle+"&PlaySessionId="+test.session, nil)
			r.Header.Set("Authorization", `MediaBrowser Token="account-secret", DeviceId="device"`)
			w := httptest.NewRecorder()
			inst.Handler().ServeHTTP(w, r)
			if w.Code != test.status {
				t.Fatal(tc.name, test.session, w.Code, w.Body.String())
			}
		}
		if calls != 1 {
			t.Fatal(tc.name, calls)
		}
	}
}
