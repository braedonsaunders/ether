package mediabrowser

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

type lazyStub struct {
	path       string
	seen       frontend.PlanRequest
	credential string
}

func (s *lazyStub) Issue(_ context.Context, credential string, plan frontend.PlanRequest) (string, error) {
	s.credential = credential
	s.seen = plan
	return s.path, nil
}
func (s *lazyStub) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

func TestLazySourceURLUsesTrustedSurfaceAndScopedGrant(t *testing.T) {
	stub := &lazyStub{path: "/sessions/lazy/scoped-secret/file"}
	i := &instance{host: frontend.Host{LazyStreams: stub, SurfaceURL: func(context.Context, *http.Request) (string, error) { return "https://ether.example/jellyfin", nil }}}
	req := httptest.NewRequest("GET", "http://attacker.invalid/Items/1", nil)
	plan := frontend.PlanRequest{Protocol: "jellyfin", Item: 5, Version: "version-id"}
	got := i.lazySourceURL(req, "account-secret", plan)
	if got != "https://ether.example/jellyfin/sessions/lazy/scoped-secret/file" || strings.Contains(got, "account-secret") || stub.credential != "account-secret" || stub.seen.Version != "version-id" {
		t.Fatal(got, stub.seen)
	}
	for _, invalid := range []string{"https://attacker.invalid/file", "/sessions/lazy/../file", "/sessions/lazy/grant/file?api_key=account-secret", "/sessions/lazy/grant/other"} {
		stub.path = invalid
		if got := i.lazySourceURL(req, "account-secret", plan); got != "" {
			t.Fatalf("accepted %q", got)
		}
	}
	rec := httptest.NewRecorder()
	i.lazyMedia(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatal(rec.Code)
	}
}

func TestLazySourceURLFailsClosedWithoutTrustedBase(t *testing.T) {
	for _, base := range []string{"", "ftp://ether.example", "http://user:pass@ether.example", "http://ether.example?token=secret", "//attacker.invalid"} {
		stub := &lazyStub{path: "/sessions/lazy/grant/file"}
		i := &instance{host: frontend.Host{LazyStreams: stub, SurfaceURL: func(context.Context, *http.Request) (string, error) { return base, nil }}}
		got := i.lazySourceURL(httptest.NewRequest("GET", "http://attacker.invalid/", nil), "account-secret", frontend.PlanRequest{})
		if got != "" || stub.credential != "" {
			t.Fatalf("accepted invalid base %q: %q", base, got)
		}
	}
}

func TestEstimateIssuesLazyPathWithoutOpeningPlayback(t *testing.T) {
	viewer := &frontend.Viewer{ID: 1}
	stub := &lazyStub{path: "/sessions/lazy/scoped-secret/file"}
	plans := 0
	host := frontend.Host{
		Identity: frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }},
		Sessions: frontendtest.Sessions{PlanFunc: func(_ context.Context, p frontend.PlanRequest) (frontend.PlayPlan, error) {
			plans++
			if !p.Intent.Estimate {
				t.Fatal("detail resolved a release")
			}
			return frontend.PlayPlan{Estimated: true, DirectPlay: true}, nil
		}},
		Capabilities: frontendtest.Capabilities{}, LazyStreams: stub,
		SurfaceURL: func(context.Context, *http.Request) (string, error) { return "http://192.0.2.5:8096/jellyfin", nil },
	}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), host)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/jellyfin/Items/"+ids.RatingKeyGUID(5)+"/PlaybackInfo", strings.NewReader(`{}`))
	r.Header.Set("X-Emby-Token", "account-secret")
	r.Header.Set("X-Emby-Device-Id", "client")
	w := httptest.NewRecorder()
	inst.Handler().ServeHTTP(w, r)
	if w.Code != 200 || plans != 1 || !strings.Contains(w.Body.String(), `"Path":"http://192.0.2.5:8096/jellyfin/sessions/lazy/scoped-secret/file"`) || strings.Contains(w.Body.String(), "account-secret") || stub.seen.Item != 5 || !stub.seen.Intent.Estimate {
		t.Fatal(w.Code, w.Body.String(), plans, stub.seen)
	}
	r = httptest.NewRequest("GET", "/jellyfin/sessions/lazy/scoped-secret/file", nil)
	w = httptest.NewRecorder()
	inst.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatal(w.Code)
	}
}

func TestPlaybackPathUsesOnlyVerifiedSurfaceBase(t *testing.T) {
	viewer := &frontend.Viewer{ID: 1}
	for _, tc := range []struct {
		name    string
		surface func(context.Context, *http.Request) (string, error)
		want    string
	}{
		{"absolute", func(context.Context, *http.Request) (string, error) { return "https://ether.example/jellyfin", nil }, "https://ether.example/jellyfin/sessions/media/session/grant/file"},
		{"embedded", nil, "/jellyfin/sessions/media/session/grant/file"},
		{"unavailable", func(context.Context, *http.Request) (string, error) { return "", errors.New("unavailable") }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			broker := frontendtest.Sessions{PlanFunc: func(context.Context, frontend.PlanRequest) (frontend.PlayPlan, error) {
				return frontend.PlayPlan{ID: "plan", Decision: frontend.Decision{Kind: "directplay"}}, nil
			}, OpenFunc: func(context.Context, frontend.OpenRequest) (frontend.Session, error) {
				return frontend.Session{ID: "session"}, nil
			}, StartStreamFunc: func(context.Context, frontend.SessionAccess, string, frontend.StreamRequest) (string, error) {
				return "/sessions/media/session/grant/file", nil
			}}
			identity := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
			inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: identity, Sessions: broker, Capabilities: frontendtest.Capabilities{}, SurfaceURL: tc.surface})
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest("POST", "/jellyfin/Items/"+ids.RatingKeyGUID(5)+"/PlaybackInfo", strings.NewReader(`{"IsPlayback":true}`))
			req.Header.Set("X-Emby-Token", "account-secret")
			req.Header.Set("X-Emby-Device-Id", "tv")
			rec := httptest.NewRecorder()
			inst.Handler().ServeHTTP(rec, req)
			if rec.Code != 200 {
				t.Fatal(rec.Code)
			}
			var payload struct{ MediaSources []struct{ Path string } }
			if err = json.Unmarshal(rec.Body.Bytes(), &payload); err != nil || len(payload.MediaSources) != 1 || payload.MediaSources[0].Path != tc.want || strings.Contains(rec.Body.String(), "account-secret") {
				t.Fatal(rec.Body.String(), err)
			}
		})
	}
}
