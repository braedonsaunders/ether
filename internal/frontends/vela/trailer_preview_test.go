package vela

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

type previewBaseSessions = frontendtest.Sessions

type previewBrokerFake struct {
	previewBaseSessions
	result frontend.TrailerPreviewResult
	calls  int
}

func (f *previewBrokerFake) TrailerPreviewAvailable() bool { return true }
func (f *previewBrokerFake) TrailerPreview(_ context.Context, req frontend.TrailerPreviewRequest) (frontend.TrailerPreviewResult, error) {
	f.calls++
	if req.Viewer == nil || req.Viewer.ID != 1 || req.Device.ID != "device" || req.Protocol != "vela" || req.Item != core.MovieKey(9) {
		return frontend.TrailerPreviewResult{}, core.ErrUnauthorized
	}
	return f.result, nil
}

// One lasting protocol invariant: preview grants have independent stop lineage
// and can never enter feature-film admission or the watch observation path.
func TestTrailerPreviewIsolation(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		heroes, _ := heroesFromRows([]object{{"style": "hero", "previewOnFocus": enabled, "continuation": "exhausted", "items": []object{{"kind": "title", "title": object{"id": "title"}, "receiptToken": "issued"}}}})
		flag, _ := heroes[0]["previewOnFocus"].(bool)
		if flag != enabled {
			t.Fatal("hero lost source row preview opt-in")
		}
	}
	stopped, reports, planned := 0, 0, 0
	broker := &previewBrokerFake{result: frontend.TrailerPreviewResult{State: "ready", Grant: &frontend.TrailerPreviewGrant{SessionID: "preview", URL: "/sessions/media/preview/grant/preview.mp4", MIMEType: "video/mp4", ExpiresAt: time.Now().Add(time.Minute), Purpose: "preview", SourceGeneration: 1, SeekGeneration: 1}}}
	broker.previewBaseSessions = frontendtest.Sessions{
		StopFunc: func(_ context.Context, a frontend.SessionAccess, id string) error {
			if a.Viewer.ID != 1 || a.Device.ID != "device" || id != "preview" {
				t.Fatal("preview stop lost scope")
			}
			stopped++
			return nil
		},
		PlanFunc: func(context.Context, frontend.PlanRequest) (frontend.PlayPlan, error) {
			planned++
			return frontend.PlayPlan{}, core.ErrNotAvailable
		},
		ReportFunc: func(context.Context, frontend.SessionAccess, string, frontend.Timeline) error { reports++; return nil },
	}
	inst := &instance{host: frontend.Host{Identity: identityFake{}, Capabilities: frontendtest.Capabilities{}, Sessions: broker}, sessions: map[string]*playSession{}}
	call := func(handler http.HandlerFunc, id, dev string, b object) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(b)
		r := httptest.NewRequest(http.MethodPost, "/vela/v0/test", nil)
		r.SetPathValue("id", id)
		r.Header.Set("Authorization", "Bearer token")
		r.Header.Set("X-Vela-Device-Id", dev)
		r = r.WithContext(context.WithValue(r.Context(), bodyKey{}, json.RawMessage(raw)))
		w := httptest.NewRecorder()
		handler(w, r)
		return w
	}
	request := object{"idempotencyKey": "focus", "client": object{"device": object{"id": "device"}, "protocolVersions": []int{0}, "containers": []string{"mp4"}, "videoCodecs": []string{"h264"}, "audioCodecs": []string{"aac"}}}
	title := ids.RatingKeyGUID(core.MovieKey(9))
	if w := call(inst.trailerPreview, title, "another", request); w.Code != 400 || broker.calls != 0 {
		t.Fatal("client device mismatch accepted", w.Code)
	}
	if w := call(inst.trailerPreview, title, "device", request); w.Code != 200 || broker.calls != 1 {
		t.Fatal("preview issuance failed", w.Code, w.Body.String())
	}
	if len(inst.sessions) != 1 || planned != 0 || reports != 0 {
		t.Fatal("preview opened normal playback")
	}
	stale := object{"kind": "stop", "sourceGeneration": 2, "seekGeneration": 1}
	if w := call(inst.command, "preview", "device", stale); w.Code != 409 || stopped != 0 {
		t.Fatal("stale stop accepted", w.Code)
	}
	observation := object{"sourceGeneration": 1, "seekGeneration": 1, "sequence": 1, "positionSeconds": 5, "state": "playing", "selectedPathId": "primary"}
	if w := call(inst.observe, "preview", "device", observation); w.Code != 400 || reports != 0 {
		t.Fatal("preview reached watch history", w.Code)
	}
	stop := object{"kind": "stop", "sourceGeneration": 1, "seekGeneration": 1}
	if w := call(inst.command, "preview", "another", stop); w.Code != 404 || stopped != 0 {
		t.Fatal("preview stop crossed device", w.Code)
	}
	w := call(inst.command, "preview", "device", stop)
	var ended object
	_ = json.Unmarshal(w.Body.Bytes(), &ended)
	if w.Code != 200 || stopped != 1 || ended["purpose"] != "preview" || ended["state"] != "ended" || planned != 0 || reports != 0 {
		t.Fatal("preview stop isolation lost", w.Code, ended)
	}
	broker.result = frontend.TrailerPreviewResult{State: "preparing", RetryAfterSeconds: 1}
	if w := call(inst.trailerPreview, title, "device", request); w.Code != 200 || w.Header().Get("Retry-After") != "1" {
		t.Fatal("preparing retry contract lost", w.Code)
	}
}
