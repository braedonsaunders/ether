package mediabrowser

import (
	"context"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"net/http/httptest"
	"strings"
	"testing"
)

type baseSessions = frontendtest.Sessions
type subtitleFake struct {
	baseSessions
	calls int
}

func (f *subtitleFake) SubtitleURL(_ context.Context, a frontend.SessionAccess, id string, index int, format string) (string, error) {
	f.calls++
	if a.Protocol != "jellyfin" || id != "session" || index != 2 || format != "vtt" {
		panic("bad subtitle request")
	}
	return "/sessions/media/session/scoped/subtitles/2.vtt", nil
}
func TestSubtitleUsesScopedBrokerURL(t *testing.T) {
	viewer := &frontend.Viewer{ID: 1}
	broker := &subtitleFake{baseSessions: frontendtest.Sessions{SessionsFunc: func(context.Context, frontend.SessionAccess) ([]frontend.Session, error) {
		return []frontend.Session{{ID: "session", Item: 5, Protocol: "jellyfin"}}, nil
	}}}
	ident := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: ident, Sessions: broker})
	if err != nil {
		t.Fatal(err)
	}
	id := ids.RatingKeyGUID(5)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/jellyfin/Videos/"+id+"/"+id+"/Subtitles/2/Stream.vtt?PlaySessionId=session&api_key=account-secret", nil)
	inst.Handler().ServeHTTP(w, r)
	location := w.Header().Get("Location")
	if w.Code != 307 || broker.calls != 1 || !strings.HasPrefix(location, "/jellyfin/sessions/media/") || strings.Contains(location, "account-secret") {
		t.Fatal(w.Code, location, broker.calls)
	}
}

func TestPlaybackInfoAdvertisesScopedTextSubtitle(t *testing.T) {
	viewer := &frontend.Viewer{ID: 1}
	broker := &subtitleFake{baseSessions: frontendtest.Sessions{
		PlanFunc: func(context.Context, frontend.PlanRequest) (frontend.PlayPlan, error) {
			return frontend.PlayPlan{ID: "plan", Decision: frontend.Decision{Kind: "directplay"}, Media: frontend.MediaInfo{Streams: []frontend.StreamInfo{{Type: "subtitle", Index: 2}}}}, nil
		},
		OpenFunc: func(context.Context, frontend.OpenRequest) (frontend.Session, error) {
			return frontend.Session{ID: "session"}, nil
		},
		StartStreamFunc: func(context.Context, frontend.SessionAccess, string, frontend.StreamRequest) (string, error) {
			return "/sessions/media/session/scoped/file", nil
		},
	}}
	ident := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: ident, Sessions: broker, Capabilities: frontendtest.Capabilities{}})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/Items/"+ids.RatingKeyGUID(5)+"/PlaybackInfo", strings.NewReader(`{"IsPlayback":true}`))
	r.Header.Set("X-Emby-Token", "grant")
	inst.Handler().ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"IsTextSubtitleStream":true`) || !strings.Contains(w.Body.String(), `"SupportsExternalStream":true`) || !strings.Contains(w.Body.String(), "/sessions/media/session/scoped/subtitles/") {
		t.Fatal(w.Code, w.Body.String())
	}
}
