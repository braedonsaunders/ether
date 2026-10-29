package mediabrowser

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

type testVersionHandles struct {
	item      frontend.RatingKey
	handle    string
	denyMatch bool
}

func (h *testVersionHandles) Issue(_ context.Context, _ frontend.VersionScope, item frontend.RatingKey, raw string) (string, error) {
	if item != h.item || raw != "private-release" {
		return "", errors.New("not offered")
	}
	return h.handle, nil
}
func (h *testVersionHandles) Resolve(_ context.Context, _ frontend.VersionScope, id string) (frontend.VersionTarget, error) {
	if id != h.handle {
		return frontend.VersionTarget{}, errors.New("unknown handle")
	}
	return frontend.VersionTarget{Item: h.item}, nil
}
func (h *testVersionHandles) MatchSession(_ context.Context, _ frontend.VersionScope, id, session string) error {
	if h.denyMatch || id != h.handle || session != "session" {
		return errors.New("wrong source")
	}
	return nil
}

func TestScopedVersionDetailAndPlayback(t *testing.T) {
	viewer := &frontend.Viewer{ID: 1}
	handle := "4e0f0a56-014b-4883-89b7-e9465f03462e"
	versions := &testVersionHandles{item: 5, handle: handle}
	identity := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
	experience := frontendtest.Experience{ItemFunc: func(_ context.Context, r frontend.ItemRequest) (frontend.ItemDetail, error) {
		if r.Key != 5 {
			t.Fatal(r.Key)
		}
		return frontend.ItemDetail{Title: frontend.Title{Key: 5, Type: "movie"}, Versions: []frontend.Version{{ID: "private-release", Name: "HD"}}}, nil
	}}
	var plan frontend.PlanRequest
	broker := frontendtest.Sessions{
		PlanFunc: func(_ context.Context, p frontend.PlanRequest) (frontend.PlayPlan, error) {
			plan = p
			return frontend.PlayPlan{Estimated: true}, nil
		},
		SessionsFunc: func(context.Context, frontend.SessionAccess) ([]frontend.Session, error) {
			return []frontend.Session{{ID: "session", Item: 5, Protocol: "jellyfin", Device: frontend.Device{ID: "device"}}}, nil
		},
		StartStreamFunc: func(context.Context, frontend.SessionAccess, string, frontend.StreamRequest) (string, error) {
			return "/sessions/media/session/grant/file", nil
		},
	}
	lazy := &lazyStub{path: "/sessions/lazy/scoped-secret/file"}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: identity, Experience: experience, Sessions: broker, Capabilities: frontendtest.Capabilities{}, VersionHandles: versions, LazyStreams: lazy, SurfaceURL: func(context.Context, *http.Request) (string, error) { return "https://ether.example/jellyfin", nil }})
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", `MediaBrowser Token="account-secret", DeviceId="device"`)
		w := httptest.NewRecorder()
		inst.Handler().ServeHTTP(w, r)
		return w
	}
	for _, path := range []string{"/Items/" + ids.RatingKeyGUID(5), "/Items/" + handle} {
		w := call("GET", path, "")
		if w.Code != 200 || !strings.Contains(w.Body.String(), handle) || strings.Contains(w.Body.String(), "private-release") || !strings.Contains(w.Body.String(), "https://ether.example/jellyfin/sessions/lazy/scoped-secret/file") {
			t.Fatal(w.Code, w.Body.String())
		}
		if lazy.seen.Version != handle || !lazy.seen.Intent.ExactVersion || !lazy.seen.Intent.Estimate {
			t.Fatal(lazy.seen)
		}
	}
	w := call("POST", "/Items/"+handle+"/PlaybackInfo", `{}`)
	if w.Code != 200 || plan.Item != 5 || plan.Version != handle || !plan.Intent.ExactVersion || !plan.Intent.Estimate {
		t.Fatal(w.Code, plan, w.Body.String())
	}
	w = call("GET", "/Videos/"+handle+"/stream?PlaySessionId=session", "")
	if w.Code != http.StatusTemporaryRedirect || strings.Contains(w.Header().Get("Location"), "account-secret") {
		t.Fatal(w.Code, w.Header())
	}
	versions.denyMatch = true
	w = call("GET", "/Videos/"+handle+"/stream?PlaySessionId=session", "")
	if w.Code != http.StatusNotFound {
		t.Fatal(w.Code, w.Body.String())
	}
	w = call("POST", "/Items/"+ids.RatingKeyGUID(5)+"/PlaybackInfo?MediaSourceId=private-release", `{}`)
	if w.Code != http.StatusNotFound {
		t.Fatal(w.Code, w.Body.String())
	}
}
