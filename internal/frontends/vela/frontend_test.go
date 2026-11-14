package vela

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/braedonsaunders/ether/internal/frontend"
	_ "modernc.org/sqlite"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type identityFake struct{ frontend.SurfaceIdentity }

func (identityFake) ResolveDevice(context.Context, string, frontend.Device) (*frontend.Viewer, time.Time, error) {
	return &frontend.Viewer{ID: 1, Username: "viewer"}, time.Now().Add(time.Hour), nil
}
func TestVelaProtocolInvariants(t *testing.T) {
	db, e := sql.Open("sqlite", ":memory:")
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	_, e = db.Exec(`CREATE TABLE vela_replays(key TEXT PRIMARY KEY,digest TEXT NOT NULL,response BLOB,expires INTEGER NOT NULL)`)
	if e != nil {
		t.Fatal(e)
	}
	seal, e := encryption(make([]byte, 32))
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	i := &instance{host: frontend.Host{Identity: identityFake{}}, ctx: ctx, cancel: cancel, mux: http.NewServeMux(), db: db, seal: seal, replays: map[string]*replay{}}
	calls := 0
	i.route("POST", "/test", func(w http.ResponseWriter, r *http.Request) {
		calls++
		send(w, 200, object{"secret": "access-token", "calls": calls})
	})
	request := func(b string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/vela/v0/test", strings.NewReader(b))
		r.Header.Set("Authorization", "Bearer test")
		r.Header.Set("X-Vela-Device-Id", "device")
		w := httptest.NewRecorder()
		i.serve(w, r)
		return w
	}
	first := request(`{"idempotencyKey":"one"}`)
	second := request(`{"idempotencyKey":"one"}`)
	if first.Code != 200 || second.Body.String() != first.Body.String() || calls != 1 {
		t.Fatalf("replay: %d %d calls %d", first.Code, second.Code, calls)
	}
	conflict := request(`{"idempotencyKey":"one","changed":true}`)
	if conflict.Code != 409 {
		t.Fatalf("conflict: %d", conflict.Code)
	}
	var saved []byte
	if e = db.QueryRow(`SELECT response FROM vela_replays`).Scan(&saved); e != nil || strings.Contains(string(saved), "access-token") {
		t.Fatal("response must be encrypted", e)
	}
	s := &playSession{source: 1, seek: 2, revision: 1, cursor: 1, signal: make(chan struct{}), plan: object{"sessionId": "session", "state": "ready"}}
	staleReply := httptest.NewRecorder()
	if !stale(staleReply, s, 1, 1) || staleReply.Code != 409 {
		t.Fatal("stale seek generation accepted")
	}
	s.publish("plan")
	s.plan["state"] = "paused"
	b, _ := json.Marshal(s.events[0])
	if strings.Contains(string(b), "paused") {
		t.Fatal("retained events alias mutable plan")
	}
	for range 100 {
		s.publish("plan")
	}
	if len(s.events) != 64 {
		t.Fatal("unbounded replay retention")
	}
	r := httptest.NewRequest("GET", "/vela/v0/capabilities", nil)
	w := httptest.NewRecorder()
	cache(w, r, object{"protocolVersion": 0})
	r.Header.Set("If-None-Match", w.Header().Get("ETag"))
	again := httptest.NewRecorder()
	cache(again, r, object{"protocolVersion": 0})
	if again.Code != 304 {
		t.Fatal("etag not honored")
	}
}
