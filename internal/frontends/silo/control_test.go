package silo

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/conform/openapi"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/coder/websocket"
)

type controlTestTickets struct {
	mu      sync.Mutex
	purpose string
	used    bool
	viewer  *frontend.Viewer
}

func (t *controlTestTickets) Issue(_ context.Context, credential string, d frontend.Device, purpose string) (string, error) {
	if credential != "grant" || d.ID != "tv" {
		return "", errors.New("unauthorized")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.purpose = purpose
	return "secret", nil
}
func (t *controlTestTickets) Consume(_ context.Context, token, purpose string) (frontend.Ticket, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if token != "secret" || t.used || purpose != t.purpose {
		return frontend.Ticket{}, errors.New("invalid")
	}
	t.used = true
	return frontend.Ticket{Viewer: t.viewer, Device: frontend.Device{ID: "tv"}}, nil
}
func (*controlTestTickets) Active(context.Context, frontend.Ticket) (bool, error) { return true, nil }

func TestControlUnavailableWithoutDeviceAuthentication(t *testing.T) {
	viewer := &frontend.Viewer{ID: 7}
	identity := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
	host := frontend.Host{Identity: identity, Sessions: frontendtest.Sessions{}, Capabilities: frontendtest.Capabilities{}, Tickets: &controlTestTickets{}, Events: eventSource{make(chan frontend.Event)}}
	inst, err := (Frontend{MachineID: "test"}).Mount(context.Background(), host)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/api/v2/playback/sessions/control/capabilities", nil)
	r.Header.Set("Authorization", "Bearer grant")
	w := httptest.NewRecorder()
	inst.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"available":false`) {
		t.Fatalf("control requires device authentication: %d %s", w.Code, w.Body.String())
	}
	r = httptest.NewRequest("GET", "/api/v2/playback/capabilities", nil)
	r.Header.Set("Authorization", "Bearer grant")
	w = httptest.NewRecorder()
	inst.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"allowed":false`) {
		t.Fatalf("playback requires device authentication: %d %s", w.Code, w.Body.String())
	}
}

func TestPlaybackControlIsSessionBoundAndRelaysCommands(t *testing.T) {
	viewer := &frontend.Viewer{ID: 7}
	id := "8c2d329e-71d2-4ea1-9b57-95187baeb0aa"
	tickets := &controlTestTickets{viewer: viewer}
	events := make(chan frontend.Event, 2)
	sessions := frontendtest.Sessions{SessionsFunc: func(_ context.Context, access frontend.SessionAccess) ([]frontend.Session, error) {
		if access.Viewer != viewer || access.Device.ID != "tv" {
			return nil, errors.New("unauthorized")
		}
		return []frontend.Session{{ID: id, ViewerID: viewer.ID, Protocol: "silo", Device: frontend.Device{ID: "tv"}}}, nil
	}}
	host := frontend.Host{Identity: testDeviceIdentity(frontendtest.Identity{ResolveFunc: func(_ context.Context, token string) (*frontend.Viewer, error) {
		if token != "grant" {
			return nil, errors.New("invalid")
		}
		return viewer, nil
	}}, viewer), Sessions: sessions, Tickets: tickets, Events: eventSource{events}}
	inst, err := (Frontend{MachineID: "test"}).Mount(context.Background(), host)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(inst.Handler())
	defer server.Close()
	capRequest := httptest.NewRequest("GET", "/api/v2/playback/sessions/control/capabilities", nil)
	capRequest.Header.Set("Authorization", "Bearer grant")
	capResponse := httptest.NewRecorder()
	inst.Handler().ServeHTTP(capResponse, capRequest)
	if capResponse.Code != 200 || !strings.Contains(capResponse.Body.String(), `"available":true`) || !strings.Contains(capResponse.Body.String(), `"protocol":"silo.playback-control.v2"`) {
		t.Fatalf("control capabilities: %d %s", capResponse.Code, capResponse.Body.String())
	}
	issue := func(session string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/v2/playback/sessions/"+session+"/control/ws-ticket", strings.NewReader(`{"installation_id":"`+inst.(*instance).installationID()+`"}`))
		r.Header.Set("Authorization", "Bearer grant")
		r.Header.Set("X-Silo-Device-Id", "tv")
		w := httptest.NewRecorder()
		inst.Handler().ServeHTTP(w, r)
		return w
	}
	if w := issue("other"); w.Code != http.StatusNotFound {
		t.Fatalf("other session ticket: %d", w.Code)
	}
	spoof := httptest.NewRequest("POST", "/api/v2/playback/sessions/"+id+"/control/ws-ticket", strings.NewReader(`{"installation_id":"`+inst.(*instance).installationID()+`"}`))
	spoof.Header.Set("Authorization", "Bearer another-device-grant")
	spoof.Header.Set("X-Silo-Device-Id", "tv")
	denied := httptest.NewRecorder()
	inst.Handler().ServeHTTP(denied, spoof)
	if denied.Code != http.StatusUnauthorized {
		t.Fatalf("spoofed device ticket: %d", denied.Code)
	}
	ticketResponse := issue(id)
	if ticketResponse.Code != http.StatusOK || !strings.Contains(ticketResponse.Body.String(), `"protocol":"silo.playback-control.v2"`) {
		t.Fatalf("ticket: %d %s", ticketResponse.Code, ticketResponse.Body.String())
	}
	if data, e := openapi.CachedSpec("../../conform/specs/manifest.json", "silo", ""); e == nil {
		doc, e := openapi.Parse(data)
		if e != nil {
			t.Fatal(e)
		}
		for _, check := range []struct {
			method, path string
			response     *httptest.ResponseRecorder
		}{
			{"GET", "/api/v2/playback/sessions/control/capabilities", capResponse},
			{"POST", "/api/v2/playback/sessions/" + id + "/control/ws-ticket", ticketResponse},
		} {
			if e = doc.Validate(check.method, check.path, check.response.Code, check.response.Header().Get("Content-Type"), check.response.Body.Bytes()); e != nil {
				t.Errorf("%s spec: %v", check.path, e)
			}
		}
	} else if !os.IsNotExist(e) {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	connect := func(session string) (*websocket.Conn, *http.Response, error) {
		return websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/api/v2/playback/sessions/"+session+"/control/ws", &websocket.DialOptions{Subprotocols: []string{controlProtocol, "silo.ticket.secret"}})
	}
	if c, response, err := connect("other"); err == nil {
		c.Close(websocket.StatusNormalClosure, "")
		t.Fatal("ticket accepted for another session")
	} else if response == nil || response.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong session response: %v %v", response, err)
	}
	conn, response, err := connect(id)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	if response.Header.Get("Sec-WebSocket-Protocol") != controlProtocol {
		t.Fatal("wrong selected protocol")
	}
	if err = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"hello","session_id":"`+id+`","client":{"name":"Apple","version":"1"},"capabilities":{"commands":["seek"]}}`)); err != nil {
		t.Fatal(err)
	}
	events <- frontend.Event{Type: "SessionCommand", ViewerID: viewer.ID, SessionID: id, TargetProtocol: "silo", TargetDeviceID: "tv", Command: &frontend.Command{Kind: frontend.CommandSeek, Position: time.Minute}}
	_, data, err := conn.Read(ctx)
	if err != nil || !strings.Contains(string(data), `"name":"seek"`) || !strings.Contains(string(data), `"position_seconds":60`) || !strings.Contains(string(data), `"session_id":"`+id+`"`) {
		t.Fatalf("command: %s %v", data, err)
	}
	if c, response, err := connect(id); err == nil {
		c.Close(websocket.StatusNormalClosure, "")
		t.Fatal("ticket replay accepted")
	} else if response == nil || response.StatusCode != http.StatusForbidden {
		t.Fatalf("replay response: %v %v", response, err)
	}
}
