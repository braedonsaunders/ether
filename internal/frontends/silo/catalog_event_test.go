package silo

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/coder/websocket"
)

func TestSiloCatalogChangedWhenAWatchlistTitleBecomesAvailable(t *testing.T) {
	movie := core.MovieKey(1)
	ident := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) {
		return &frontend.Viewer{ID: 7}, nil
	}}
	tickets := &testTickets{}
	ch := make(chan frontend.Event, 1)
	events, err := (Frontend{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: ident, Tickets: tickets, Events: eventSource{ch}, Experience: frontendtest.Experience{}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(events.Handler())
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/api/v2/events/ws-ticket", nil)
	req.Header.Set("Authorization", "Bearer grant")
	req.Header.Set("X-Silo-Device-Id", "tv")
	rec := httptest.NewRecorder()
	events.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/api/v2/events/ws", &websocket.DialOptions{Subprotocols: []string{"silo.events.v2", "silo.ticket.secret"}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")
	if _, _, err = c.Read(ctx); err != nil {
		t.Fatal(err)
	}
	if err = c.Write(ctx, websocket.MessageText, []byte(`{"type":"subscribe","channels":["catalog"]}`)); err != nil {
		t.Fatal(err)
	}
	if _, _, err = c.Read(ctx); err != nil {
		t.Fatal(err)
	}
	ch <- frontend.Event{Type: "LibraryChanged", ViewerID: 7, Item: movie}
	_, data, err := c.Read(ctx)
	if err != nil || !strings.Contains(string(data), `"event":"catalog.changed"`) || !strings.Contains(string(data), `"change":"available"`) || !strings.Contains(string(data), ids.RatingKeyGUID(movie)) {
		t.Fatalf("%s %v", data, err)
	}
}
