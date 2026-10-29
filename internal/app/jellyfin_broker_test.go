package app

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/frontends/mediabrowser"
	"github.com/braedonsaunders/ether/internal/sessions"
)

func TestJellyfinRealBrokerPlayback(t *testing.T) {
	ctx := context.Background()
	viewer := &core.Viewer{ID: 2}
	p := &activityPlayback{entries: map[string]*core.PlaySession{}}
	active := func(_ context.Context, id int64) (*core.Viewer, error) {
		if id != viewer.ID {
			return nil, core.ErrUnauthorized
		}
		return viewer, nil
	}
	events := sessions.NewEventBus(nil, active)
	defer events.Close()
	broker, err := sessions.NewBroker(sessions.BrokerDeps{Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, k core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: k}, nil
	}, Playback: p, Transcoder: activityTranscoder{}, Progress: activityProgress{}, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Events: events})
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close(ctx)
	ident := frontendtest.Identity{ResolveFunc: func(_ context.Context, token string) (*core.Viewer, error) {
		if token != "grant" {
			return nil, core.ErrUnauthorized
		}
		return viewer, nil
	}}
	inst, err := (mediabrowser.Jellyfin{MachineID: "test"}).Mount(ctx, frontend.Host{Identity: ident, Sessions: sessions.BindSurface(broker, "jellyfin"), Capabilities: frontendtest.Capabilities{}})
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", `MediaBrowser Client="TV", DeviceId="tv", Token="grant"`)
		inst.Handler().ServeHTTP(w, r)
		return w
	}
	path := "/Items/" + ids.RatingKeyGUID(core.MovieKey(5)) + "/PlaybackInfo"
	w := call("POST", path, `{"EnableDirectPlay":true}`)
	if w.Code != 200 || len(p.Sessions()) != 0 {
		t.Fatalf("estimate %d %s sessions=%d", w.Code, w.Body, len(p.Sessions()))
	}
	w = call("POST", path, `{"IsPlayback":true,"EnableDirectPlay":true}`)
	if w.Code != 200 {
		t.Fatalf("play %d %s", w.Code, w.Body)
	}
	var result struct {
		PlaySessionId string
		MediaSources  []struct{ Path string }
	}
	if err = json.Unmarshal(w.Body.Bytes(), &result); err != nil || result.PlaySessionId == "" || len(result.MediaSources) != 1 || !strings.HasPrefix(result.MediaSources[0].Path, "/sessions/media/") {
		t.Fatal(result, err)
	}
	if len(p.Sessions()) != 1 {
		t.Fatal("session not admitted")
	}
	w = call("GET", result.MediaSources[0].Path, "")
	if w.Code != 200 {
		t.Fatalf("serve %d %s", w.Code, w.Body)
	}
	w = call("POST", "/Sessions/Playing/Stopped", `{"PlaySessionId":"`+result.PlaySessionId+`","PositionTicks":10000000}`)
	if w.Code != 204 || len(p.Sessions()) != 0 {
		t.Fatalf("stop %d %s", w.Code, w.Body)
	}
}
