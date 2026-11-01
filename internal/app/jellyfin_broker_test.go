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
	"github.com/braedonsaunders/ether/internal/state"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/coder/websocket"
)

type coldFixturePlayback struct{ *activityPlayback }

func (p coldFixturePlayback) Prepare(ctx context.Context, viewer *core.User, key core.RatingKey, caps core.ClientCaps) (*core.PlaySession, error) {
	session, err := p.activityPlayback.Prepare(ctx, viewer, key, caps)
	if err == nil {
		session.Info.Container, session.Info.Size = "mp4", 24680
	}
	return session, err
}

func (p coldFixturePlayback) PrepareSurface(ctx context.Context, viewer *core.User, key core.RatingKey, caps core.ClientCaps, release string, exact bool) (*core.PlaySession, error) {
	if release != "" || exact {
		return nil, core.ErrNotAvailable
	}
	return p.Prepare(ctx, viewer, key, caps)
}

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
	}, Playback: coldFixturePlayback{p}, Transcoder: activityTranscoder{}, Progress: activityProgress{}, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Events: events})
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
	w := call("POST", path, `{"IsPlayback":false,"EnableDirectPlay":true}`)
	if w.Code != 200 || len(p.Sessions()) != 0 {
		t.Fatalf("estimate %d %s sessions=%d", w.Code, w.Body, len(p.Sessions()))
	}
	w = call("POST", path, `{"EnableDirectPlay":true}`)
	if w.Code != 200 {
		t.Fatalf("play %d %s", w.Code, w.Body)
	}
	var result struct {
		PlaySessionId string
		MediaSources  []struct {
			Path, Container    string
			Size               int64
			SupportsDirectPlay bool
		}
	}
	if err = json.Unmarshal(w.Body.Bytes(), &result); err != nil || result.PlaySessionId == "" || len(result.MediaSources) != 1 || !strings.HasPrefix(result.MediaSources[0].Path, "/sessions/media/") || result.MediaSources[0].Container != "mp4" || result.MediaSources[0].Size != 24680 || !result.MediaSources[0].SupportsDirectPlay {
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

func TestPlexWatchReachesJellyfinSocket(t *testing.T) {
	// Race instrumentation of store open, mount, and the handshake exceeds 5s
	// on the shared worker. The budget bounds a stuck socket; it is not the assertion.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	viewer := &core.Viewer{ID: 2}
	active := func(_ context.Context, id int64) (*core.Viewer, error) {
		if id != 2 {
			return nil, core.ErrUnauthorized
		}
		return viewer, nil
	}
	events := sessions.NewEventBus(nil, active)
	defer events.Close()
	watch := state.New(store.OpenTest(t), state.Options{})
	defer watch.Close(context.Background())
	unsub := subscribeSessionState(watch, events, nil)
	defer unsub()
	ident := frontendtest.Identity{ResolveFunc: func(_ context.Context, token string) (*core.Viewer, error) {
		if token != "grant" {
			return nil, core.ErrUnauthorized
		}
		return viewer, nil
	}}
	exp := frontendtest.Experience{UserDataFunc: func(ctx context.Context, v *core.Viewer, keys []core.RatingKey) (map[core.RatingKey]core.WatchStatus, error) {
		return watch.GetMany(ctx, v.ID, keys)
	}}
	inst, err := (mediabrowser.Jellyfin{MachineID: "test"}).Mount(ctx, frontend.Host{Identity: ident, Experience: exp, Events: sessions.BindEvents(events, "jellyfin")})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(inst.Handler())
	defer srv.Close()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/socket?api_key=grant", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")
	if _, _, err = c.Read(ctx); err != nil {
		t.Fatal(err)
	}
	key := core.MovieKey(5)
	if err = watch.Scrobble(state.WithOrigin(ctx, state.Origin{Surface: "plex", ClientID: "plex-tv"}), viewer.ID, key); err != nil {
		t.Fatal(err)
	}
	_, body, err := c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var message struct {
		MessageType string
		Data        struct {
			UserDataList []struct {
				Played bool
				ItemId string
			}
		}
	}
	if err = json.Unmarshal(body, &message); err != nil || message.MessageType != "UserDataChanged" || len(message.Data.UserDataList) != 1 || !message.Data.UserDataList[0].Played || message.Data.UserDataList[0].ItemId != ids.RatingKeyGUID(key) {
		t.Fatal(string(body), err)
	}
}
