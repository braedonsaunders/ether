package mediabrowser

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

func TestEmbyBusyPlaybackAndStreamAreRetryable(t *testing.T) {
	viewer := &frontend.Viewer{ID: 1}
	host := frontend.Host{
		Identity:     frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }},
		Capabilities: frontendtest.Capabilities{},
		Sessions: frontendtest.Sessions{
			PlanFunc: func(context.Context, frontend.PlanRequest) (frontend.PlayPlan, error) {
				return frontend.PlayPlan{ID: "plan", Decision: frontend.Decision{Kind: "directplay"}}, nil
			},
			OpenFunc: func(context.Context, frontend.OpenRequest) (frontend.Session, error) {
				return frontend.Session{ID: "private-session"}, nil
			},
			StartStreamFunc: func(context.Context, frontend.SessionAccess, string, frontend.StreamRequest) (string, error) {
				return "", &frontend.SessionBusyError{RetryAfter: 1500 * time.Millisecond}
			},
			SessionsFunc: func(context.Context, frontend.SessionAccess) ([]frontend.Session, error) {
				return []frontend.Session{{ID: "private-session", Item: 5, Protocol: "emby", Device: frontend.Device{ID: "tv"}}}, nil
			},
		},
	}
	inst, err := (Emby{MachineID: "test"}).Mount(context.Background(), host)
	if err != nil {
		t.Fatal(err)
	}
	id := ids.RatingKeyGUID(5)
	for _, tc := range []struct{ method, path, body string }{{"POST", "/Items/" + id + "/PlaybackInfo", `{"IsPlayback":true}`}, {"GET", "/Videos/" + id + "/stream?PlaySessionId=private-session", ""}} {
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		r.Header.Set("X-Emby-Token", "account-secret")
		r.Header.Set("X-Emby-Device-Id", "tv")
		w := httptest.NewRecorder()
		inst.Handler().ServeHTTP(w, r)
		if w.Code != 503 || w.Header().Get("Retry-After") != "2" || strings.Contains(w.Body.String(), "account-secret") || strings.Contains(w.Body.String(), "private-session") {
			t.Fatal(w.Code, w.Header(), w.Body.String())
		}
	}
}
