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
	if json.Unmarshal(b, &msg) != nil || msg["MessageType"] != "ForceKeepAlive" {
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
