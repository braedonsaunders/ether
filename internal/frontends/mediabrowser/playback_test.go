package mediabrowser

import (
	"context"
	"github.com/braedonsaunders/ether/internal/conform/openapi"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

func TestPlaybackInfoEstimatesAndOpens(t *testing.T) {
	viewer := &frontend.Viewer{ID: 1}
	var calls []string
	sessions := frontendtest.Sessions{PlanFunc: func(_ context.Context, req frontend.PlanRequest) (frontend.PlayPlan, error) {
		if req.Viewer != viewer || req.Protocol != "jellyfin" || req.Item != 5 || req.Intent.Options.DirectStream {
			t.Fatal(req)
		}
		if req.Intent.Estimate {
			calls = append(calls, "estimate")
		} else {
			calls = append(calls, "plan")
		}
		return frontend.PlayPlan{ID: "plan", Estimated: req.Intent.Estimate, Media: frontend.MediaInfo{Container: "mp4"}, Decision: frontend.Decision{Kind: "directplay"}}, nil
	}, OpenFunc: func(_ context.Context, req frontend.OpenRequest) (frontend.Session, error) {
		calls = append(calls, "open")
		return frontend.Session{ID: "session"}, nil
	}, StartStreamFunc: func(_ context.Context, _ frontend.SessionAccess, id string, _ frontend.StreamRequest) (string, error) {
		calls = append(calls, "stream")
		return "/sessions/media/path/grant/direct", nil
	}}
	ident := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: ident, Sessions: sessions, Capabilities: frontendtest.Capabilities{}})
	if err != nil {
		t.Fatal(err)
	}
	path := "/jellyfin/Items/" + ids.RatingKeyGUID(5) + "/PlaybackInfo"
	call := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", path, strings.NewReader(body))
		r.Header.Set("X-Emby-Token", "grant")
		inst.Handler().ServeHTTP(w, r)
		return w
	}
	w := call(`{"EnableDirectStream":false}`)
	if w.Code != 200 || strings.Join(calls, ",") != "estimate" {
		t.Fatal(w.Code, w.Body.String(), calls)
	}
	spec, e := openapi.CachedSpec("../../conform/specs/manifest.json", "jellyfin", "")
	if e == nil {
		doc, e := openapi.Parse(spec)
		if e != nil {
			t.Fatal(e)
		}
		if e = doc.Validate("POST", strings.TrimPrefix(path, "/jellyfin"), w.Code, w.Header().Get("Content-Type"), w.Body.Bytes()); e != nil {
			t.Fatal(e)
		}
	} else if !os.IsNotExist(e) {
		t.Fatal(e)
	}
	if w := call(`{"IsPlayback":true,"EnableDirectStream":false}`); w.Code != 200 || strings.Join(calls, ",") != "estimate,plan,open,stream" || !strings.Contains(w.Body.String(), "/jellyfin/sessions/media/") {
		t.Fatal(w.Code, w.Body.String(), calls)
	}
}

func TestTimelineReportsAndStops(t *testing.T) {
	viewer := &frontend.Viewer{ID: 1}
	var states []string
	sessions := frontendtest.Sessions{ReportFunc: func(_ context.Context, a frontend.SessionAccess, id string, timeline frontend.Timeline) error {
		if a.Viewer != viewer || id != "session" || timeline.Position.Milliseconds() != 1000 {
			t.Fatal(a, id, timeline)
		}
		states = append(states, timeline.State)
		return nil
	}, StopFunc: func(_ context.Context, _ frontend.SessionAccess, id string) error {
		states = append(states, "stop")
		return nil
	}}
	ident := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: ident, Sessions: sessions})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/Sessions/Playing/Progress", "/Sessions/Playing/Stopped"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", p, strings.NewReader(`{"PlaySessionId":"session","PositionTicks":10000000}`))
		r.Header.Set("X-Emby-Token", "grant")
		inst.Handler().ServeHTTP(w, r)
		if w.Code != 204 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if strings.Join(states, ",") != "playing,stopped,stop" {
		t.Fatal(states)
	}
}

func TestStreamAliasChecksSessionItemAndOmitsCredential(t *testing.T) {
	viewer := &frontend.Viewer{ID: 1}
	calls := 0
	sessions := frontendtest.Sessions{SessionsFunc: func(context.Context, frontend.SessionAccess) ([]frontend.Session, error) {
		return []frontend.Session{{ID: "session", Item: 5, Protocol: "jellyfin"}}, nil
	}, StartStreamFunc: func(_ context.Context, _ frontend.SessionAccess, id string, req frontend.StreamRequest) (string, error) {
		calls++
		if id != "session" || req.Format != frontend.StreamHLS {
			t.Fatal(id, req)
		}
		return "/sessions/media/session/scoped/index.m3u8", nil
	}}
	ident := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: ident, Sessions: sessions})
	if err != nil {
		t.Fatal(err)
	}
	call := func(key frontend.RatingKey) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/jellyfin/Videos/"+ids.RatingKeyGUID(key)+"/master.m3u8?PlaySessionId=session&api_key=secret", nil)
		inst.Handler().ServeHTTP(w, r)
		return w
	}
	if w := call(6); w.Code != 404 || calls != 0 {
		t.Fatal(w.Code, calls)
	}
	if w := call(5); w.Code != 307 || calls != 1 || w.Header().Get("Location") != "/jellyfin/sessions/media/session/scoped/index.m3u8" || strings.Contains(w.Header().Get("Location"), "secret") {
		t.Fatal(w.Code, w.Header(), calls)
	}
}
