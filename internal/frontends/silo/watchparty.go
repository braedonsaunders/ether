package silo

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/coder/websocket"
)

const roomProtocol = "silo.room.v2"

func (i *instance) watchPartyRoutes() error {
	routes := []struct {
		method, path string
		h            http.HandlerFunc
	}{
		{"POST", "/api/v2/watch-together/rooms", i.watchPartyCreate},
		{"POST", "/api/v2/watch-together/join", i.watchPartyJoin},
		{"GET", "/api/v2/watch-together/rooms/{id}", i.watchPartyGet},
		{"PUT", "/api/v2/watch-together/rooms/{id}/selection", i.watchPartySelect},
		{"PUT", "/api/v2/watch-together/rooms/{id}/staged-selection", i.watchPartyStage},
		{"POST", "/api/v2/watch-together/rooms/{id}/playback/start", i.watchPartyStart},
		{"POST", "/api/v2/watch-together/rooms/{id}/playback/stop", i.watchPartyStop},
		{"PATCH", "/api/v2/watch-together/rooms/{id}/policy", i.watchPartyPolicy},
		{"DELETE", "/api/v2/watch-together/rooms/{id}", i.watchPartyEnd},
		{"POST", "/api/v2/watch-together/rooms/{id}/ws-ticket", i.watchPartyTicket},
		{"GET", "/api/v2/watch-together/rooms/{id}/ws", i.watchPartySocket},
	}
	for _, route := range routes {
		if err := i.router.Handle(route.method, route.path, route.h); err != nil {
			return err
		}
	}
	return nil
}

func (i *instance) partyActor(r *http.Request) (frontend.PartyActor, int) {
	v, _, err := i.viewer(r)
	if err != nil || v == nil {
		return frontend.PartyActor{}, http.StatusUnauthorized
	}
	if i.host.WatchParty == nil {
		return frontend.PartyActor{}, http.StatusServiceUnavailable
	}
	return frontend.PartyActor{Viewer: v, Device: device(r), Protocol: "silo"}, 0
}

func (i *instance) watchPartyCapabilities(w http.ResponseWriter, r *http.Request) {
	if _, _, err := i.viewer(r); err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if i.host.WatchParty == nil {
		reply(w, http.StatusOK, map[string]any{"revision": "ether-watch-party-v1", "state": "unsupported", "allowed": false, "socket_protocol": ""})
		return
	}
	reply(w, http.StatusOK, map[string]any{
		"revision": "ether-watch-party-v1", "state": "available", "allowed": true,
		"staged_selection": true, "lobby_ready": true, "connection_replaced": true,
		"selection_mode_switch": false, "member_state": false, "picker": false,
		"vote_host_override": false, "stop_playback": true, "max_member_state_ids": 0,
		"socket_protocol": roomProtocol,
	})
}

func (i *instance) watchPartyCreate(w http.ResponseWriter, r *http.Request) {
	actor, status := i.partyActor(r)
	if status != 0 {
		http.Error(w, http.StatusText(status), status)
		return
	}
	var body struct {
		RoomID string `json:"room_id"`
		Mode   string `json:"selection_mode"`
	}
	if decode(r, &body) != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	view, err := i.host.WatchParty.Create(r.Context(), actor, frontend.PartyCreate{RoomID: body.RoomID, Mode: body.Mode})
	if err != nil {
		i.partyError(w, err)
		return
	}
	i.writeParty(w, http.StatusCreated, view)
}

func (i *instance) watchPartyJoin(w http.ResponseWriter, r *http.Request) {
	actor, status := i.partyActor(r)
	if status != 0 {
		http.Error(w, http.StatusText(status), status)
		return
	}
	var body struct {
		Code  string `json:"code"`
		Token string `json:"join_token"`
	}
	if decode(r, &body) != nil || (body.Code == "" && body.Token == "") {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	view, err := i.host.WatchParty.Join(r.Context(), actor, frontend.PartyJoin{Code: body.Code, Token: body.Token})
	if err != nil {
		i.partyError(w, err)
		return
	}
	i.writeParty(w, http.StatusOK, view)
}

func (i *instance) watchPartyGet(w http.ResponseWriter, r *http.Request) {
	actor, view, ok := i.partyRoom(w, r)
	if !ok {
		return
	}
	got, err := i.host.WatchParty.Get(r.Context(), actor, view, r.Header.Get("X-Room-Token"))
	if err != nil {
		i.partyError(w, err)
		return
	}
	i.writeParty(w, http.StatusOK, got)
}

func (i *instance) watchPartySelect(w http.ResponseWriter, r *http.Request) {
	i.partyPick(w, r, false)
}

func (i *instance) watchPartyStage(w http.ResponseWriter, r *http.Request) {
	i.partyPick(w, r, true)
}

func (i *instance) partyPick(w http.ResponseWriter, r *http.Request, staged bool) {
	actor, roomID, ok := i.partyRoom(w, r)
	if !ok {
		return
	}
	var body struct {
		ContentID string `json:"content_id"`
		FileID    string `json:"file_id"`
		LibraryID string `json:"library_id"`
	}
	if decode(r, &body) != nil || body.ContentID == "" {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	key, err := ids.ParseRatingKeyGUID(body.ContentID)
	if err != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	if staged {
		current, err := i.host.WatchParty.Get(r.Context(), actor, roomID, r.Header.Get("X-Room-Token"))
		if err != nil {
			i.partyError(w, err)
			return
		}
		if current.Phase != "lobby" {
			http.Error(w, "Conflict", http.StatusConflict)
			return
		}
	}
	view, err := i.host.WatchParty.Select(r.Context(), actor, roomID, r.Header.Get("X-Room-Token"), frontend.PartyPick{Item: key, FileID: body.FileID, LibraryID: body.LibraryID})
	if err != nil {
		i.partyError(w, err)
		return
	}
	i.writeParty(w, http.StatusOK, view)
}

func (i *instance) watchPartyStart(w http.ResponseWriter, r *http.Request) {
	actor, roomID, ok := i.partyRoom(w, r)
	if !ok {
		return
	}
	view, err := i.host.WatchParty.Start(r.Context(), actor, roomID, r.Header.Get("X-Room-Token"))
	if err != nil {
		i.partyError(w, err)
		return
	}
	i.writeParty(w, http.StatusOK, view)
}

func (i *instance) watchPartyStop(w http.ResponseWriter, r *http.Request) {
	actor, roomID, ok := i.partyRoom(w, r)
	if !ok {
		return
	}
	view, err := i.host.WatchParty.Stop(r.Context(), actor, roomID, r.Header.Get("X-Room-Token"))
	if err != nil {
		i.partyError(w, err)
		return
	}
	i.writeParty(w, http.StatusOK, view)
}

func (i *instance) watchPartyPolicy(w http.ResponseWriter, r *http.Request) {
	actor, roomID, ok := i.partyRoom(w, r)
	if !ok {
		return
	}
	var body struct {
		Policy string `json:"guest_control_policy"`
	}
	if decode(r, &body) != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	view, err := i.host.WatchParty.Policy(r.Context(), actor, roomID, r.Header.Get("X-Room-Token"), body.Policy)
	if err != nil {
		i.partyError(w, err)
		return
	}
	i.writeParty(w, http.StatusOK, view)
}

func (i *instance) watchPartyEnd(w http.ResponseWriter, r *http.Request) {
	actor, roomID, ok := i.partyRoom(w, r)
	if !ok {
		return
	}
	if err := i.host.WatchParty.End(r.Context(), actor, roomID, r.Header.Get("X-Room-Token")); err != nil {
		i.partyError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (i *instance) watchPartyTicket(w http.ResponseWriter, r *http.Request) {
	actor, roomID, ok := i.partyRoom(w, r)
	if !ok {
		return
	}
	ticket, err := i.host.WatchParty.IssueTicket(r.Context(), actor, roomID, r.Header.Get("X-Room-Token"))
	if err != nil {
		i.partyError(w, err)
		return
	}
	reply(w, http.StatusOK, map[string]any{"ticket": ticket.Ticket, "expires_in": ticket.ExpiresIn, "max_connection_seconds": ticket.MaxSeconds, "protocol": ticket.Protocol})
}

func (i *instance) watchPartySocket(w http.ResponseWriter, r *http.Request) {
	if i.host.WatchParty == nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	protocol, ticket := roomSocketProof(r)
	if !protocol || ticket == "" {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	conn, err := i.host.WatchParty.OpenSocket(r.Context(), r.PathValue("id"), ticket)
	if err != nil {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	defer conn.Close()
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{roomProtocol}})
	if err != nil {
		return
	}
	defer ws.Close(websocket.StatusNormalClosure, "")
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(socketMaxSeconds())*time.Second)
	defer cancel()
	write := func(v any) error {
		body, e := json.Marshal(v)
		if e != nil {
			return e
		}
		c, done := context.WithTimeout(ctx, 5*time.Second)
		defer done()
		return ws.Write(c, websocket.MessageText, body)
	}
	incoming := make(chan []byte, 8)
	go func() {
		defer close(incoming)
		for {
			_, body, err := ws.Read(ctx)
			if err != nil {
				return
			}
			select {
			case incoming <- body:
			case <-ctx.Done():
				return
			}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case body, ok := <-incoming:
			if !ok {
				return
			}
			if err := i.roomFrame(ctx, conn.Actor, body); err != nil {
				return
			}
		case ev, ok := <-conn.Events:
			if !ok {
				return
			}
			if write(siloEvent(ev)) != nil {
				return
			}
			if ev.Type == "closed" || ev.Type == "replaced" {
				return
			}
		}
	}
}

func socketMaxSeconds() int { return 300 }

func (i *instance) roomFrame(ctx context.Context, actor frontend.PartyActor, body []byte) error {
	var msg struct {
		Type       string    `json:"type"`
		SessionID  string    `json:"session_id"`
		CommandID  string    `json:"command_id"`
		Action     string    `json:"action"`
		Position   *float64  `json:"position_seconds"`
		Rate       *float64  `json:"rate"`
		Ready      *bool     `json:"is_ready"`
		Lobby      *bool     `json:"ready"`
		ClientSent time.Time `json:"client_sent_at"`
	}
	if json.Unmarshal(body, &msg) != nil || i.host.WatchParty == nil {
		return nil
	}
	pos := time.Duration(0)
	if msg.Position != nil {
		pos = time.Duration(*msg.Position * float64(time.Second))
	}
	rate := 0.0
	if msg.Rate != nil {
		rate = *msg.Rate
	}
	switch msg.Type {
	case "transport_request":
		_ = i.host.WatchParty.Transport(ctx, actor, "", "", frontend.PartyInput{Action: msg.Action, Position: pos, Rate: rate})
	case "ping":
		_ = i.host.WatchParty.Report(ctx, actor, frontend.PartyReport{Kind: "ping", ClientSent: msg.ClientSent})
	case "attach_session":
		_ = i.host.WatchParty.Report(ctx, actor, frontend.PartyReport{Kind: "attach", SessionID: msg.SessionID})
	case "buffering":
		_ = i.host.WatchParty.Report(ctx, actor, frontend.PartyReport{Kind: "buffering", SessionID: msg.SessionID, Position: pos})
	case "ready":
		_ = i.host.WatchParty.Report(ctx, actor, frontend.PartyReport{Kind: "ready", SessionID: msg.SessionID, CommandID: msg.CommandID, Position: pos, Ready: msg.Ready})
	case "state_report":
		ready := msg.Ready
		_ = i.host.WatchParty.Report(ctx, actor, frontend.PartyReport{Kind: "state", SessionID: msg.SessionID, CommandID: msg.CommandID, Position: pos, Ready: ready})
	case "lobby_ready":
		_ = i.host.WatchParty.Report(ctx, actor, frontend.PartyReport{Kind: "lobby", Lobby: msg.Lobby})
	}
	return nil
}

func roomSocketProof(r *http.Request) (protocol bool, ticket string) {
	for _, header := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, part := range strings.Split(header, ",") {
			p := strings.TrimSpace(part)
			if p == roomProtocol {
				protocol = true
			}
			if strings.HasPrefix(p, "silo.ticket.") {
				if ticket != "" {
					return false, ""
				}
				ticket = strings.TrimPrefix(p, "silo.ticket.")
			}
		}
	}
	return protocol, ticket
}

func (i *instance) partyRoom(w http.ResponseWriter, r *http.Request) (frontend.PartyActor, string, bool) {
	actor, status := i.partyActor(r)
	if status != 0 {
		http.Error(w, http.StatusText(status), status)
		return frontend.PartyActor{}, "", false
	}
	if r.Header.Get("X-Room-Token") == "" {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return frontend.PartyActor{}, "", false
	}
	return actor, r.PathValue("id"), true
}

func (i *instance) partyError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, frontend.ErrPartyNotFound):
		http.Error(w, "Not Found", http.StatusNotFound)
	case errors.Is(err, frontend.ErrPartyConflict):
		http.Error(w, "Conflict", http.StatusConflict)
	case errors.Is(err, frontend.ErrPartyDenied), errors.Is(err, frontend.ErrPartyForbidden):
		http.Error(w, "Forbidden", http.StatusForbidden)
	default:
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
	}
}

func (i *instance) writeParty(w http.ResponseWriter, status int, view frontend.PartyView) {
	reply(w, status, map[string]any{"room": siloRoom(view), "room_access_token": view.RoomToken})
}

func siloRoom(view frontend.PartyView) map[string]any {
	anchor := view.AnchorAt
	if anchor.IsZero() {
		anchor = time.Unix(0, 0)
	}
	members := make([]any, 0, len(view.Members))
	for _, m := range view.Members {
		members = append(members, map[string]any{
			"user_id": strconv.FormatInt(m.ViewerID, 10), "profile_id": strconv.FormatInt(m.ViewerID, 10),
			"display_name": m.Name, "is_host": m.Host, "is_self": m.Self, "connected": m.Connected,
			"is_ready": m.Ready, "is_buffering": m.Buffering, "is_syncing": view.Playback == "waiting" && m.Connected && !m.Ready,
			"lobby_ready": m.Lobby,
		})
	}
	var content, file, library any
	if view.ContentID != "" {
		content = view.ContentID
	}
	if view.FileID != "" {
		file = view.FileID
	}
	if view.LibraryID != "" {
		library = view.LibraryID
	}
	var session any
	if view.AttachedSessionID != "" {
		session = view.AttachedSessionID
	}
	return map[string]any{
		"room_id": view.RoomID, "phase": view.Phase, "playback_state": view.Playback,
		"selection_mode": view.SelectionMode, "selection_revision": view.SelectionRevision,
		"selected_content_id": content, "selected_file_id": file, "selected_library_id": library,
		"code": view.Code, "guest_control_policy": view.GuestPolicy, "is_paused": view.Paused,
		"anchor_position_seconds": view.Position.Seconds(), "anchor_updated_at": partyTime(anchor),
		"generation": view.Generation, "member_count": len(view.Members), "host_connected": view.HostConnected,
		"self_role": view.Role, "self_can_control_transport": view.CanTransport, "self_can_manage_room": view.CanManage,
		"self_ignore_wait": view.IgnoreWait, "attached_session_id": session, "invite_path": view.InvitePath,
		"playback_rate": view.Rate, "members": members,
	}
}

func partyTime(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

func siloEvent(ev frontend.PartyEvent) any {
	switch ev.Type {
	case "snapshot":
		return map[string]any{"type": "snapshot", "room": siloRoom(ev.View)}
	case "transport":
		return map[string]any{"type": "transport_command", "command": map[string]any{
			"command_id": ev.Command.ID, "session_id": ev.Command.SessionID,
			"selection_revision": ev.Command.SelectionRevision, "action": ev.Command.Action,
			"position_seconds": ev.Command.Position.Seconds(), "execute_at": partyTime(ev.Command.ExecuteAt),
			"issued_at": partyTime(ev.Command.IssuedAt), "playback_state": ev.Command.Playback,
			"rate": ev.Command.Rate,
		}}
	case "pong":
		return map[string]any{"type": "pong", "client_sent_at": partyTime(ev.ClientSent), "server_received_at": partyTime(ev.Received), "server_sent_at": partyTime(ev.Sent)}
	case "closed":
		reason := ev.Reason
		if reason == "" {
			reason = "ended"
		}
		return map[string]any{"type": "room_closed", "reason": reason}
	case "replaced":
		return map[string]any{"type": "connection_replaced", "reason": ev.Reason}
	case "error":
		return map[string]any{"type": "error", "code": ev.ErrorCode, "message": ev.ErrorMessage}
	default:
		return map[string]any{"type": ev.Type}
	}
}
