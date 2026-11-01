package silo

import (
	"context"
	"errors"
	"github.com/braedonsaunders/ether/internal/conform/openapi"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/coder/websocket"
)

type testTickets struct {
	mu   sync.Mutex
	used bool
}

func (*testTickets) Issue(context.Context, string, frontend.Device, string) (string, error) {
	return "secret", nil
}
func (t *testTickets) Consume(_ context.Context, s, p string) (frontend.Ticket, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s != "secret" || p != "events-ws" || t.used {
		return frontend.Ticket{}, errors.New("invalid")
	}
	t.used = true
	return frontend.Ticket{Viewer: &frontend.Viewer{ID: 7}, Device: frontend.Device{ID: "tv"}}, nil
}

func (*testTickets) Active(context.Context, frontend.Ticket) (bool, error) { return true, nil }

type eventSource struct{ ch chan frontend.Event }

func (s eventSource) Subscribe(context.Context, frontend.EventScope) (<-chan frontend.Event, error) {
	return s.ch, nil
}
func TestEventSocketSendsViewerChange(t *testing.T) {
	tickets := &testTickets{}
	ch := make(chan frontend.Event, 1)
	ident := frontendtest.Identity{ResolveFunc: func(_ context.Context, s string) (*frontend.Viewer, error) {
		if s != "grant" {
			return nil, errors.New("bad")
		}
		return &frontend.Viewer{ID: 7}, nil
	}}
	instance, err := (Frontend{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: ident, Tickets: tickets, Events: eventSource{ch}, Experience: frontendtest.Experience{}})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/api/v2/events/ws-ticket", nil)
	req.Header.Set("Authorization", "Bearer grant")
	req.Header.Set("X-Silo-Device-Id", "tv")
	rec := httptest.NewRecorder()
	instance.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("ticket %d", rec.Code)
	}
	spec, e := openapi.CachedSpec("../../conform/specs/manifest.json", "silo", "")
	if e == nil {
		doc, e := openapi.Parse(spec)
		if e != nil {
			t.Fatal(e)
		}
		if e = doc.Validate("POST", "/api/v2/events/ws-ticket", rec.Code, rec.Header().Get("Content-Type"), rec.Body.Bytes()); e != nil {
			t.Errorf("ticket spec: %v", e)
		}
	} else if !os.IsNotExist(e) {
		t.Fatal(e)
	}
	srv := httptest.NewServer(instance.Handler())
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/api/v2/events/ws", &websocket.DialOptions{Subprotocols: []string{"silo.events.v2", "silo.ticket.secret"}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")
	if resp.Header.Get("Sec-WebSocket-Protocol") != "silo.events.v2" {
		t.Fatal("protocol")
	}
	if _, _, err = c.Read(ctx); err != nil {
		t.Fatal(err)
	}
	if err = c.Write(ctx, websocket.MessageText, []byte(`{"type":"subscribe","channels":["user_state"]}`)); err != nil {
		t.Fatal(err)
	}
	if _, _, err = c.Read(ctx); err != nil {
		t.Fatal(err)
	}
	ch <- frontend.Event{Type: "WatchStateChanged", ViewerID: 7, Item: 1000000000000001}
	_, data, err := c.Read(ctx)
	if err != nil || !strings.Contains(string(data), `"event":"user_state.changed"`) {
		t.Fatalf("event %s %v", data, err)
	}
	_, resp, err = websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/api/v2/events/ws", &websocket.DialOptions{Subprotocols: []string{"silo.events.v2", "silo.ticket.secret"}})
	if err == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatal("replay accepted")
	}
}
