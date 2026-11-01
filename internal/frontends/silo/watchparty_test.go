package silo

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/watchparty"
	"github.com/coder/websocket"
)

func partyIdentity(host, guest *frontend.Viewer) frontendtest.Identity {
	return frontendtest.Identity{ResolveFunc: func(_ context.Context, token string) (*frontend.Viewer, error) {
		switch token {
		case "host":
			return host, nil
		case "guest":
			return guest, nil
		default:
			return nil, errors.New("bad")
		}
	}}
}

func TestWatchPartyCreateJoinAndDeniedGuest(t *testing.T) {
	host := &frontend.Viewer{ID: 1, Username: "ada"}
	guest := &frontend.Viewer{ID: 2, Username: "bea"}
	engine := watchparty.New(watchparty.Options{
		Allow: func(_ context.Context, _ frontend.Protocol, v *core.Viewer, _ core.RatingKey) error {
			if v.ID == guest.ID {
				return core.ErrNotFound
			}
			return nil
		},
		ItemID: ids.RatingKeyGUID,
	})
	inst := mountParty(t, partyIdentity(host, guest), engine)
	caps := callParty(inst, "GET", "/api/v2/watch-together/capabilities", "host", "", "")
	if caps.Code != 200 || !strings.Contains(caps.Body.String(), `"socket_protocol":"silo.room.v2"`) || !strings.Contains(caps.Body.String(), `"allowed":true`) {
		t.Fatal(caps.Code, caps.Body.String())
	}
	play := callParty(inst, "GET", "/api/v2/playback/capabilities", "host", "", "")
	if !strings.Contains(play.Body.String(), "watch_party_coordinator_v1") || !strings.Contains(play.Body.String(), "fixed_media_file_v1") {
		t.Fatal(play.Body.String())
	}
	created := callParty(inst, "POST", "/api/v2/watch-together/rooms", "host", "", `{"room_id":"11111111-1111-4111-8111-111111111111","selection_mode":"host_pick"}`)
	if created.Code != 201 {
		t.Fatal(created.Code, created.Body.String())
	}
	var room struct {
		Room struct {
			RoomID string `json:"room_id"`
			Code   string `json:"code"`
		} `json:"room"`
		Token string `json:"room_access_token"`
	}
	if json.Unmarshal(created.Body.Bytes(), &room) != nil || room.Room.RoomID == "" || room.Token == "" || room.Room.Code == "" {
		t.Fatal(created.Body.String())
	}
	selected := callParty(inst, "PUT", "/api/v2/watch-together/rooms/"+room.Room.RoomID+"/selection", "host", room.Token, `{"content_id":"`+ids.RatingKeyGUID(5)+`"}`)
	if selected.Code != 200 || !strings.Contains(selected.Body.String(), ids.RatingKeyGUID(5)) {
		t.Fatal(selected.Code, selected.Body.String())
	}
	denied := callParty(inst, "POST", "/api/v2/watch-together/join", "guest", "", `{"code":"`+room.Room.Code+`"}`)
	if denied.Code != 403 || strings.Contains(denied.Body.String(), ids.RatingKeyGUID(5)) {
		t.Fatal(denied.Code, denied.Body.String())
	}
}

func TestWatchPartySocketPingAndRejoin(t *testing.T) {
	host := &frontend.Viewer{ID: 4, Username: "ada", Title: "Ada"}
	engine := watchparty.New(watchparty.Options{ItemID: ids.RatingKeyGUID, UseLead: true})
	inst := mountParty(t, partyIdentity(host, host), engine)
	srv := httptest.NewServer(inst.Handler())
	defer srv.Close()
	created := callParty(inst, "POST", "/api/v2/watch-together/rooms", "host", "", `{"selection_mode":"host_pick"}`)
	var room struct {
		Room struct {
			RoomID string `json:"room_id"`
			Code   string `json:"code"`
		} `json:"room"`
		Token string `json:"room_access_token"`
	}
	if created.Code != 201 || json.Unmarshal(created.Body.Bytes(), &room) != nil {
		t.Fatal(created.Code, created.Body.String())
	}
	ticket := callParty(inst, "POST", "/api/v2/watch-together/rooms/"+room.Room.RoomID+"/ws-ticket", "host", room.Token, "")
	var proof struct {
		Ticket   string `json:"ticket"`
		Protocol string `json:"protocol"`
	}
	if ticket.Code != 200 || json.Unmarshal(ticket.Body.Bytes(), &proof) != nil || proof.Protocol != "silo.room.v2" || proof.Ticket == "" {
		t.Fatal(ticket.Code, ticket.Body.String())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/api/v2/watch-together/rooms/"+room.Room.RoomID+"/ws", &websocket.DialOptions{Subprotocols: []string{"silo.room.v2", "silo.ticket." + proof.Ticket}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	_, first, err := conn.Read(ctx)
	if err != nil || !strings.Contains(string(first), `"type":"snapshot"`) {
		t.Fatal(string(first), err)
	}
	sent := time.Now().UTC().Format(time.RFC3339Nano)
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"ping","client_sent_at":"`+sent+`"}`)); err != nil {
		t.Fatal(err)
	}
	_, pong, err := conn.Read(ctx)
	if err != nil || !strings.Contains(string(pong), `"type":"pong"`) || !strings.Contains(string(pong), `"server_received_at"`) {
		t.Fatal(string(pong), err)
	}
	conn.CloseNow()
	again := callParty(inst, "POST", "/api/v2/watch-together/join", "host", "", `{"code":"`+room.Room.Code+`"}`)
	if again.Code != 200 || !strings.Contains(again.Body.String(), room.Room.RoomID) || strings.Count(again.Body.String(), `"user_id"`) != 1 {
		t.Fatal(again.Code, again.Body.String())
	}
}

func mountParty(t *testing.T, ident frontend.SurfaceIdentity, engine frontend.WatchParty) frontend.Instance {
	t.Helper()
	inst, err := (Frontend{MachineID: "party"}).Mount(context.Background(), frontend.Host{Identity: ident, WatchParty: engine})
	if err != nil {
		t.Fatal(err)
	}
	return inst
}

func callParty(inst frontend.Instance, method, path, token, room, body string) *httptest.ResponseRecorder {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, path, reader)
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("X-Silo-Device-Id", "couch")
	if room != "" {
		r.Header.Set("X-Room-Token", room)
	}
	w := httptest.NewRecorder()
	inst.Handler().ServeHTTP(w, r)
	return w
}
