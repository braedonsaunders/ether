package mediabrowser

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

type missingVersionHandles struct{}

func (missingVersionHandles) Issue(context.Context, frontend.VersionScope, frontend.RatingKey, string) (string, error) {
	return "", frontend.ErrNotFound
}
func (missingVersionHandles) Resolve(context.Context, frontend.VersionScope, string) (frontend.VersionTarget, error) {
	return frontend.VersionTarget{}, frontend.ErrNotFound
}
func (missingVersionHandles) MatchSession(context.Context, frontend.VersionScope, string, string) error {
	return frontend.ErrNotFound
}

func TestDetailFallsBackToGenericSourceWhenVersionCannotBeIssued(t *testing.T) {
	viewer := &frontend.Viewer{ID: 1}
	identity := frontendtest.Identity{ResolveFunc: func(_ context.Context, credential string) (*frontend.Viewer, error) {
		if credential != "valid" {
			return nil, errors.New("denied")
		}
		return viewer, nil
	}}
	experience := frontendtest.Experience{ItemFunc: func(context.Context, frontend.ItemRequest) (frontend.ItemDetail, error) {
		return frontend.ItemDetail{Title: frontend.Title{Key: 5, Type: "movie"}, Versions: []frontend.Version{{ID: "private-release", Name: "private-source"}}}, nil
	}}
	for _, dialect := range []struct {
		front frontend.Frontend
		name  string
	}{{Jellyfin{MachineID: "test"}, "jellyfin"}, {Emby{MachineID: "test"}, "emby"}} {
		sessions := frontendtest.Sessions{PlanFunc: func(_ context.Context, req frontend.PlanRequest) (frontend.PlayPlan, error) {
			if !req.Intent.Estimate || req.Version != "" {
				t.Fatal("fallback resolved a version", req)
			}
			if req.Device.ID == "" {
				return frontend.PlayPlan{}, errors.New("device unavailable")
			}
			return frontend.PlayPlan{Estimated: true, DirectPlay: true}, nil
		}}
		inst, err := dialect.front.Mount(context.Background(), frontend.Host{Identity: identity, Experience: experience, Sessions: sessions, Capabilities: frontendtest.Capabilities{}, VersionHandles: missingVersionHandles{}})
		if err != nil {
			t.Fatal(err)
		}
		for _, deviceID := range []string{"", "device"} {
			r := httptest.NewRequest("GET", "/Items/"+ids.RatingKeyGUID(5), nil)
			r.Header.Set("X-Emby-Token", "valid")
			if deviceID != "" {
				r.Header.Set("X-Emby-Device-Id", deviceID)
			}
			w := httptest.NewRecorder()
			inst.Handler().ServeHTTP(w, r)
			var body struct {
				Sources []map[string]any `json:"MediaSources"`
			}
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil || len(body.Sources) != 1 || body.Sources[0]["Id"] != ids.RatingKeyGUID(5) || strings.Contains(w.Body.String(), "private-release") || strings.Contains(w.Body.String(), "private-source") {
				t.Fatal(dialect.name, deviceID, w.Code, w.Body.String())
			}
			if deviceID == "" && (body.Sources[0]["SupportsDirectPlay"] != false || body.Sources[0]["Path"] != nil) {
				t.Fatal("unbound device claimed playback", body.Sources[0])
			}
			if deviceID != "" && body.Sources[0]["SupportsDirectPlay"] != true {
				t.Fatal("broker compatibility lost", body.Sources[0])
			}
		}
		r := httptest.NewRequest("GET", "/Items/"+ids.RatingKeyGUID(5), nil)
		r.Header.Set("X-Emby-Token", "revoked")
		w := httptest.NewRecorder()
		inst.Handler().ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatal("unauthorized detail", dialect.name, w.Code)
		}
	}
}

func TestColdDetailStillOffersGenericSource(t *testing.T) {
	viewer := &frontend.Viewer{ID: 1}
	identity := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
	experience := frontendtest.Experience{ItemFunc: func(context.Context, frontend.ItemRequest) (frontend.ItemDetail, error) {
		return frontend.ItemDetail{Title: frontend.Title{Key: 5, Type: "movie"}}, nil
	}}
	for _, front := range []frontend.Frontend{Jellyfin{MachineID: "test"}, Emby{MachineID: "test"}} {
		inst, err := front.Mount(context.Background(), frontend.Host{Identity: identity, Experience: experience})
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest("GET", "/Items/"+ids.RatingKeyGUID(5), nil)
		r.Header.Set("X-Emby-Token", "valid")
		w := httptest.NewRecorder()
		inst.Handler().ServeHTTP(w, r)
		var body struct {
			Sources []map[string]any `json:"MediaSources"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil || len(body.Sources) != 1 || body.Sources[0]["Id"] != ids.RatingKeyGUID(5) {
			t.Fatal(w.Code, w.Body.String())
		}
	}
}

func TestInfuseDetailOffersStaticWhenPlanRemuxes(t *testing.T) {
	viewer := &frontend.Viewer{ID: 1}
	identity := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
	experience := frontendtest.Experience{ItemFunc: func(context.Context, frontend.ItemRequest) (frontend.ItemDetail, error) {
		return frontend.ItemDetail{Title: frontend.Title{Key: 5, Type: "movie", Title: "Sintel"}}, nil
	}}
	sessions := frontendtest.Sessions{PlanFunc: func(_ context.Context, req frontend.PlanRequest) (frontend.PlayPlan, error) {
		if !req.Intent.Estimate {
			t.Fatal("detail resolved a release")
		}
		return frontend.PlayPlan{Estimated: true, DirectPlay: false, DirectStream: true, Transcoding: true, Decision: frontend.Decision{Kind: "copy"}}, nil
	}}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: identity, Experience: experience, Sessions: sessions, Capabilities: frontendtest.Capabilities{}})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/Items/"+ids.RatingKeyGUID(5)+"?fields=MediaSources,Path", nil)
	r.Header.Set("Authorization", `MediaBrowser Client="Infuse-Direct", Device="AppleTV", DeviceId="appletv", Version="8.5.9", Token="grant"`)
	w := httptest.NewRecorder()
	inst.Handler().ServeHTTP(w, r)
	var body struct {
		Sources []map[string]any `json:"MediaSources"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil || len(body.Sources) != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
	if body.Sources[0]["SupportsDirectPlay"] != true || body.Sources[0]["SupportsDirectStream"] != true || body.Sources[0]["Protocol"] != "File" || body.Sources[0]["IsRemote"] != false {
		t.Fatal(body.Sources[0])
	}
}
