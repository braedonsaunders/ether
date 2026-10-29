package silo

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/coder/websocket"
)

const eventsProtocol = "silo.events.v2"

func (i *instance) eventTicket(w http.ResponseWriter, r *http.Request) {
	v, token, err := i.viewer(r)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	if i.host.Tickets == nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	ticket, err := i.host.Tickets.Issue(r.Context(), token, device(r), "events-ws")
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	reply(w, 200, map[string]any{"ticket": ticket, "protocol": eventsProtocol, "expires_in": 30, "max_connection_seconds": 300})
}
func (i *instance) eventsSocket(w http.ResponseWriter, r *http.Request) {
	if i.host.Tickets == nil || i.host.Events == nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	var token string
	protocol := false
	for _, header := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, part := range strings.Split(header, ",") {
			p := strings.TrimSpace(part)
			if p == eventsProtocol {
				protocol = true
			}
			if strings.HasPrefix(p, "silo.ticket.") {
				if token != "" {
					http.Error(w, "Forbidden", 403)
					return
				}
				token = strings.TrimPrefix(p, "silo.ticket.")
			}
		}
	}
	if !protocol || token == "" {
		http.Error(w, "Forbidden", 403)
		return
	}
	ticket, err := i.host.Tickets.Consume(r.Context(), token, "events-ws")
	if err != nil || ticket.Viewer == nil {
		http.Error(w, "Forbidden", 403)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	events, err := i.host.Events.Subscribe(ctx, frontend.EventScope{Viewer: ticket.Viewer, Protocol: "silo"})
	if err != nil {
		http.Error(w, "Forbidden", 403)
		return
	}
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{eventsProtocol}})
	if err != nil {
		return
	}
	defer ws.Close(websocket.StatusNormalClosure, "")
	write := func(x any) error {
		body, e := json.Marshal(x)
		if e != nil {
			return e
		}
		c, done := context.WithTimeout(ctx, 5*time.Second)
		defer done()
		return ws.Write(c, websocket.MessageText, body)
	}
	if write(map[string]any{"type": "hello", "schema_version": 2, "connection_id": "", "available_channels": []string{"user_state", "catalog"}, "required_action": "subscribe"}) != nil {
		return
	}
	c, done := context.WithTimeout(ctx, 5*time.Second)
	_, body, err := ws.Read(c)
	done()
	if err != nil {
		return
	}
	var req struct {
		Type      string   `json:"type"`
		RequestID string   `json:"request_id"`
		Channels  []string `json:"channels"`
	}
	if json.Unmarshal(body, &req) != nil || req.Type != "subscribe" {
		return
	}
	allowed := []string{}
	rejected := []any{}
	userState := false
	for _, channel := range req.Channels {
		if channel == "user_state" || channel == "catalog" {
			allowed = append(allowed, channel)
			if channel == "user_state" {
				userState = true
			}
		} else {
			rejected = append(rejected, map[string]any{"channel": channel, "code": "unsupported", "message": "Unsupported channel"})
		}
	}
	if write(map[string]any{"type": "subscribed", "request_id": req.RequestID, "channels": allowed, "rejected": rejected}) != nil {
		return
	}
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		for {
			if _, _, err := ws.Read(ctx); err != nil {
				return
			}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if ok, e := i.host.Tickets.Active(ctx, ticket); e != nil || !ok {
				_ = write(map[string]any{"type": "access_changed"})
				ws.Close(websocket.StatusCode(4001), "access changed")
				return
			}
		case <-closed:
			return
		case event, ok := <-events:
			if !ok {
				return
			}
			if !userState || (event.Type != "UserDataChanged" && event.Type != "WatchStateChanged") || event.Item == 0 {
				continue
			}
			if ok, e := i.host.Tickets.Active(ctx, ticket); e != nil || !ok {
				return
			}
			if i.host.Experience == nil {
				continue
			}
			if _, err := i.host.Experience.Item(ctx, frontend.ItemRequest{Viewer: ticket.Viewer, Device: ticket.Device, Key: event.Item}); err != nil {
				continue
			}
			payload := map[string]any{"profile_id": strconv.FormatInt(ticket.Viewer.ID, 10), "content_id": ids.RatingKeyGUID(event.Item), "change": "progress"}
			if event.Type == "WatchStateChanged" {
				payload["change"] = "watched"
			}
			if write(map[string]any{"type": "event", "channel": "user_state", "event": "user_state.changed", "timestamp": time.Now().UTC().Format(time.RFC3339Nano), "data": payload}) != nil {
				return
			}
		}
	}
}
func (i *instance) eventRoutes() error {
	if err := i.router.Handle("POST", "/api/v2/events/ws-ticket", http.HandlerFunc(i.eventTicket)); err != nil {
		return err
	}
	return i.router.Handle("GET", "/api/v2/events/ws", http.HandlerFunc(i.eventsSocket))
}
