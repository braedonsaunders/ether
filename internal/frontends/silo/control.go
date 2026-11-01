package silo

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/coder/websocket"
)

const controlProtocol = "silo.playback-control.v2"

func controlPurpose(session string) string {
	digest := sha256.Sum256([]byte(session))
	return "control-ws:" + base64.RawURLEncoding.EncodeToString(digest[:])
}

func (i *instance) controlSession(ctx context.Context, viewer *frontend.Viewer, dev frontend.Device, id string) bool {
	if i.host.Sessions == nil || viewer == nil || dev.ID == "" || id == "" {
		return false
	}
	sessions, err := i.host.Sessions.Sessions(ctx, frontend.SessionAccess{Viewer: viewer, Device: dev, Protocol: "silo"})
	if err != nil {
		return false
	}
	for _, session := range sessions {
		if session.ID == id && session.ViewerID == viewer.ID && session.Protocol == "silo" && session.Device.ID == dev.ID {
			return true
		}
	}
	return false
}

func (i *instance) controlTicket(w http.ResponseWriter, r *http.Request) {
	v, credential, err := i.deviceViewer(r)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if i.host.Tickets == nil || i.host.Events == nil || i.host.Sessions == nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		InstallationID string `json:"installation_id"`
	}
	if decode(r, &body) != nil || body.InstallationID == "" {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	if body.InstallationID != i.installationID() {
		installationChanged(w)
		return
	}
	id := r.PathValue("session")
	dev := device(r)
	if !i.controlSession(r.Context(), v, dev, id) {
		http.NotFound(w, r)
		return
	}
	token, err := i.host.Tickets.Issue(r.Context(), credential, dev, controlPurpose(id))
	if err != nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	reply(w, http.StatusOK, map[string]any{"ticket": token, "protocol": controlProtocol, "expires_in": 30, "max_connection_seconds": 300})
}

func controlCommandID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	raw[6] = raw[6]&0x0f | 0x40
	raw[8] = raw[8]&0x3f | 0x80
	text := hex.EncodeToString(raw[:])
	return text[:8] + "-" + text[8:12] + "-" + text[12:16] + "-" + text[16:20] + "-" + text[20:], nil
}

func siloControlCommand(command *frontend.Command) (string, map[string]any) {
	if command == nil {
		return "", nil
	}
	payload := map[string]any{}
	switch command.Kind {
	case frontend.CommandPause:
		return "pause", payload
	case frontend.CommandUnpause:
		return "unpause", payload
	case frontend.CommandStop:
		return "stop", payload
	case frontend.CommandSeek:
		payload["position_seconds"] = command.Position.Seconds()
		return "seek", payload
	case frontend.CommandMessage:
		payload["message"] = command.Message
		return "display_message", payload
	default:
		return "", nil
	}
}

func (i *instance) controlSocket(w http.ResponseWriter, r *http.Request) {
	_, hasDeviceIdentity := i.host.Identity.(frontend.DeviceIdentity)
	if i.host.Tickets == nil || i.host.Events == nil || i.host.Sessions == nil || !hasDeviceIdentity {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	var token string
	protocol := false
	for _, header := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, part := range strings.Split(header, ",") {
			p := strings.TrimSpace(part)
			if p == controlProtocol {
				protocol = true
			}
			if strings.HasPrefix(p, "silo.ticket.") {
				if token != "" {
					http.Error(w, "Forbidden", http.StatusForbidden)
					return
				}
				token = strings.TrimPrefix(p, "silo.ticket.")
			}
		}
	}
	if !protocol || token == "" {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	id := r.PathValue("session")
	ticket, err := i.host.Tickets.Consume(r.Context(), token, controlPurpose(id))
	if err != nil || ticket.Viewer == nil || !i.controlSession(r.Context(), ticket.Viewer, ticket.Device, id) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	events, err := i.host.Events.Subscribe(ctx, frontend.EventScope{Viewer: ticket.Viewer, Protocol: "silo", SessionID: id, Device: ticket.Device})
	if err != nil {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{controlProtocol}})
	if err != nil {
		return
	}
	defer ws.Close(websocket.StatusNormalClosure, "")
	readCtx, readCancel := context.WithTimeout(ctx, 5*time.Second)
	_, data, err := ws.Read(readCtx)
	readCancel()
	if err != nil {
		return
	}
	var hello struct {
		Type         string `json:"type"`
		SessionID    string `json:"session_id"`
		Capabilities struct {
			Commands []string `json:"commands"`
		} `json:"capabilities"`
	}
	if json.Unmarshal(data, &hello) != nil || hello.Type != "hello" || hello.SessionID != id {
		return
	}
	commands := map[string]bool{}
	for _, name := range hello.Capabilities.Commands {
		commands[name] = true
	}
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		for {
			if _, _, err := ws.Read(ctx); err != nil {
				return
			}
		}
	}()
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-closed:
			return
		case <-ticker.C:
			if ok, err := i.host.Tickets.Active(ctx, ticket); err != nil || !ok || !i.controlSession(ctx, ticket.Viewer, ticket.Device, id) {
				return
			}
		case event, ok := <-events:
			if !ok {
				return
			}
			if event.Type != "SessionCommand" || event.SessionID != id || event.ViewerID != ticket.Viewer.ID || event.TargetProtocol != "silo" || event.TargetDeviceID != ticket.Device.ID {
				continue
			}
			if ok, err := i.host.Tickets.Active(ctx, ticket); err != nil || !ok {
				return
			}
			name, payload := siloControlCommand(event.Command)
			if name == "" || !commands[name] {
				continue
			}
			commandID, err := controlCommandID()
			if err != nil {
				return
			}
			body, err := json.Marshal(map[string]any{"type": "command", "command_id": commandID, "session_id": id, "name": name, "payload": payload})
			if err != nil {
				return
			}
			writeCtx, done := context.WithTimeout(ctx, 5*time.Second)
			err = ws.Write(writeCtx, websocket.MessageText, body)
			done()
			if err != nil {
				return
			}
		}
	}
}
