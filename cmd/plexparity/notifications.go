package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	protocol "github.com/braedonsaunders/ether/internal/conform/plex"
	"github.com/coder/websocket"
)

// notificationCapture performs real long-lived subscriptions and generates a
// real watch-state event after the subscription has been established.
func notificationCapture(ctx context.Context, client *http.Client, base, viewer, name, key string, profile http.Header) ([]capture, error) {
	var captures []capture
	h := profile.Clone()
	for k, values := range h {
		for i := range values {
			values[i] = strings.ReplaceAll(values[i], "{{token}}", "parity-"+viewer)
		}
		h[k] = values
	}
	h.Set("X-Plex-Client-Identifier", "parity-"+viewer+"-"+name+"-notifications")
	emit := func() error {
		req := protocol.Request{Method: http.MethodGet, Path: "/:/scrobble?identifier=com.plexapp.plugins.library&key=" + key, Headers: map[string]http.Header{name: h}}
		r, err := strictFetch(ctx, client, base, "parity-"+viewer, req, name, nil, false)
		if err != nil || r.Status != http.StatusOK {
			return fmt.Errorf("notification event: status=%d error=%v", r.Status, err)
		}
		return nil
	}
	streamCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(streamCtx, http.MethodGet, base+"/:/eventsource/notifications?filters=timeline", nil)
	if err != nil {
		return nil, err
	}
	req.Header = h.Clone()
	response, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	reader := bufio.NewReader(io.LimitReader(response.Body, 64<<10))
	initial, err := reader.ReadString('\n')
	if err != nil || initial != "retry: 5000\n" {
		response.Body.Close()
		return nil, fmt.Errorf("eventsource greeting=%q error=%v", initial, err)
	}
	blank, err := reader.ReadString('\n')
	if err != nil || blank != "\n" {
		response.Body.Close()
		return nil, fmt.Errorf("eventsource separator=%q error=%v", blank, err)
	}
	if err = emit(); err != nil {
		response.Body.Close()
		return nil, err
	}
	var event bytes.Buffer
	for {
		line, readErr := reader.ReadString('\n')
		if readErr != nil {
			response.Body.Close()
			return nil, readErr
		}
		event.WriteString(line)
		if line == "\n" {
			break
		}
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.HasPrefix(event.String(), "event: timeline\n") {
		return nil, fmt.Errorf("invalid eventsource status=%d event=%q", response.StatusCode, event.String())
	}
	body := append([]byte(initial+blank), event.Bytes()...)
	captures = append(captures, capture{viewer, name, "/:/eventsource/notifications", false, protocol.Response{Status: response.StatusCode, Headers: response.Header, Body: body}})
	for _, path := range []string{"/:/websocket/notifications", "/:/websockets/notifications"} {
		wsCtx, stop := context.WithTimeout(ctx, 15*time.Second)
		ws, handshake, dialErr := websocket.Dial(wsCtx, "ws"+strings.TrimPrefix(base, "http")+path+"?filters=timeline", &websocket.DialOptions{HTTPClient: client, HTTPHeader: h.Clone()})
		if dialErr != nil {
			stop()
			return nil, fmt.Errorf("%s websocket dial: %w", path, dialErr)
		}
		type wsResult struct {
			kind websocket.MessageType
			body []byte
			err  error
		}
		read := make(chan wsResult, 1)
		go func() { kind, body, err := ws.Read(wsCtx); read <- wsResult{kind, body, err} }()
		// A successful ping proves the server has installed its subscription.
		if err = ws.Ping(wsCtx); err != nil {
			ws.CloseNow()
			stop()
			return nil, fmt.Errorf("%s websocket ping: %w", path, err)
		}
		if err = emit(); err != nil {
			ws.CloseNow()
			stop()
			return nil, err
		}
		message := <-read
		ws.CloseNow()
		stop()
		if message.err != nil || message.kind != websocket.MessageText || !bytes.Contains(message.body, []byte(`"type":"timeline"`)) {
			return nil, fmt.Errorf("%s websocket event kind=%d error=%v body=%s", path, message.kind, message.err, message.body)
		}
		captures = append(captures, capture{viewer, name, path, false, protocol.Response{Status: handshake.StatusCode, Headers: handshake.Header, Body: message.body}})
	}
	return captures, nil
}
