package mediabrowser

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/coder/websocket"
)

type socketEvents chan frontend.Event

func (e socketEvents) Subscribe(_ context.Context, _ frontend.EventScope) (<-chan frontend.Event, error) {
	return e, nil
}

type deviceSocketEvents struct {
	ch chan frontend.Event
	t  *testing.T
}

func (e deviceSocketEvents) Subscribe(_ context.Context, scope frontend.EventScope) (<-chan frontend.Event, error) {
	if scope.Device.ID != "tv" || scope.Protocol != "jellyfin" {
		e.t.Errorf("incorrect event scope: %+v", scope)
	}
	return e.ch, nil
}

type sessionSocketEvents struct{ lifecycle chan frontend.Event }

func (e sessionSocketEvents) Subscribe(_ context.Context, scope frontend.EventScope) (<-chan frontend.Event, error) {
	if scope.Owner {
		return e.lifecycle, nil
	}
	return make(chan frontend.Event), nil
}

func TestSocketSessionsSubscriptionUsesBrokerSnapshots(t *testing.T) {
	viewer := &frontend.Viewer{ID: 1, Username: "owner", IsOwner: true}
	ident := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
	events := sessionSocketEvents{lifecycle: make(chan frontend.Event, 1)}
	broker := frontendtest.Sessions{SessionsFunc: func(_ context.Context, a frontend.SessionAccess) ([]frontend.Session, error) {
		if a.Viewer != viewer || !a.Owner {
			t.Fatal(a)
		}
		return []frontend.Session{{ID: "session", ViewerID: 1, StartedAt: time.Now(), MediaPath: "/secret"}}, nil
	}}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: ident, Sessions: broker, Events: events})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(inst.Handler())
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/socket?api_key=grant", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	if _, _, err = c.Read(ctx); err != nil {
		t.Fatal(err)
	}
	if err = c.Write(ctx, websocket.MessageText, []byte(`{"MessageType":"SessionsStart"}`)); err != nil {
		t.Fatal(err)
	}
	check := func() {
		_, b, err := c.Read(ctx)
		if err != nil || !strings.Contains(string(b), `"MessageType":"Sessions"`) || !strings.Contains(string(b), `"Id":"session"`) || strings.Contains(string(b), "/secret") {
			t.Fatal(string(b), err)
		}
	}
	check()
	events.lifecycle <- frontend.Event{Type: "SessionUpdated"}
	check()
}

func TestSocketUserDataUsesCurrentFavoriteAndRating(t *testing.T) {
	viewer := &frontend.Viewer{ID: 1}
	ident := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
	events := make(socketEvents, 1)
	exp := frontendtest.Experience{UserDataFunc: func(_ context.Context, v *frontend.Viewer, keys []frontend.RatingKey) (map[frontend.RatingKey]frontend.WatchStatus, error) {
		if v != viewer || len(keys) != 1 || keys[0] != 5 {
			t.Fatal(v, keys)
		}
		return map[frontend.RatingKey]frontend.WatchStatus{5: {IsFavorite: true, UserRating: 8}}, nil
	}}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: ident, Experience: exp, Events: events})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(inst.Handler())
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/socket?api_key=grant", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	if _, _, err = c.Read(ctx); err != nil {
		t.Fatal(err)
	}
	events <- frontend.Event{Type: "UserDataChanged", Item: 5}
	_, b, err := c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var msg struct {
		Data struct {
			UserDataList []struct {
				IsFavorite bool
				Likes      bool
				Rating     float64
			}
		}
	}
	if err := json.Unmarshal(b, &msg); err != nil || len(msg.Data.UserDataList) != 1 || !msg.Data.UserDataList[0].IsFavorite || !msg.Data.UserDataList[0].Likes || msg.Data.UserDataList[0].Rating != 8 {
		t.Fatal(string(b), err)
	}
}

func TestSocketPlaystateUsesTargetedBrokerCommand(t *testing.T) {
	viewer := &frontend.Viewer{ID: 1}
	ident := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
	events := make(chan frontend.Event, 2)
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: ident, Events: deviceSocketEvents{ch: events, t: t}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(inst.Handler())
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/socket?api_key=grant&DeviceId=tv", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	if _, _, err = c.Read(ctx); err != nil {
		t.Fatal(err)
	}
	events <- frontend.Event{Type: "SessionCommand", TargetProtocol: "jellyfin", TargetDeviceID: "other", Command: &frontend.Command{Kind: frontend.CommandPause}}
	events <- frontend.Event{Type: "SessionCommand", TargetProtocol: "jellyfin", TargetDeviceID: "tv", Command: &frontend.Command{Kind: frontend.CommandSeek, Position: 3 * time.Second}}
	_, body, err := c.Read(ctx)
	if err != nil || !strings.Contains(string(body), `"MessageType":"Playstate"`) || !strings.Contains(string(body), `"Command":"Seek"`) || !strings.Contains(string(body), `"SeekPositionTicks":30000000`) {
		t.Fatal(string(body), err)
	}
}

func TestSocketAuthenticationAndKeepAlive(t *testing.T) {
	ident := frontendtest.Identity{ResolveFunc: func(_ context.Context, token string) (*frontend.Viewer, error) {
		if token == "grant" {
			return &frontend.Viewer{ID: 1}, nil
		}
		return nil, errors.New("invalid")
	}}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: ident})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(inst.Handler())
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/socket"
	_, resp, err := websocket.Dial(ctx, url, nil)
	if err == nil || resp == nil || resp.StatusCode != 403 {
		t.Fatalf("unauthenticated: %v %v", resp, err)
	}
	c, _, err := websocket.Dial(ctx, url+"?api_key=grant", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")
	_, b, err := c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var msg map[string]any
	if json.Unmarshal(b, &msg) != nil || msg["MessageType"] != "ForceKeepAlive" || msg["Data"] != float64(60) || len(msg["MessageId"].(string)) != 36 {
		t.Fatal(string(b))
	}
	if err = c.Write(ctx, websocket.MessageText, []byte(`{"MessageType":"KeepAlive"}`)); err != nil {
		t.Fatal(err)
	}
	_, b, err = c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal(b, &msg) != nil || msg["MessageType"] != "KeepAlive" {
		t.Fatal(string(b))
	}
}

func TestClosingFrontendClosesSocket(t *testing.T) {
	ident := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return &frontend.Viewer{ID: 1}, nil }}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: ident})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(inst.Handler())
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/socket?api_key=grant", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	if _, _, err = c.Read(ctx); err != nil {
		t.Fatal(err)
	}
	if err = inst.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err = c.Read(ctx); err == nil {
		t.Fatal("socket remained open")
	}
}
