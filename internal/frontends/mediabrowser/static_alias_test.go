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
