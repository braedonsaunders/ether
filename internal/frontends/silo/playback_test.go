package silo

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/braedonsaunders/ether/internal/conform/openapi"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

type fileIDs struct{}

func TestMediaRequiresBoundLiveDeviceGrant(t *testing.T) {
	viewer := &frontend.Viewer{ID: 7}
	identity := deviceTestIdentity{resolveDevice: func(_ context.Context, token string, d frontend.Device) (*frontend.Viewer, time.Time, error) {
		switch token {
		case "tv-grant":
			if d.ID == "tv" {
				return viewer, time.Now().Add(time.Minute), nil
			}
		case "phone-grant":
			if d.ID == "phone" {
				return viewer, time.Now().Add(time.Minute), nil
			}
		case "expired":
			return viewer, time.Now().Add(-time.Minute), nil
		}
		return nil, time.Time{}, errors.New("revoked or wrong device")
	}}
	served := 0
	sessions := frontendtest.Sessions{SessionsFunc: func(context.Context, frontend.SessionAccess) ([]frontend.Session, error) {
		return []frontend.Session{{ID: "session", ViewerID: 7, Protocol: "silo", Device: frontend.Device{ID: "tv"}}}, nil
	}, ServeMediaFunc: func(w http.ResponseWriter, _ *http.Request) { served++; w.WriteHeader(http.StatusOK) }}
	inst, err := (Frontend{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: identity, Sessions: sessions})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		token, device string
		want          int
	}{
		{"tv-grant", "tv", 200},
		{"phone-grant", "tv", 401},    // Same viewer, forged device header.
		{"phone-grant", "phone", 404}, // Valid grant, another session's device.
		{"expired", "tv", 401},
		{"revoked", "tv", 401},
		{"tv-grant", "", 401},
	} {
		r := httptest.NewRequest("GET", "/api/v2/stream/session/scoped/file", nil)
		r.Header.Set("Authorization", "Bearer "+tc.token)
		r.Header.Set("X-Silo-Device-Id", tc.device)
		w := httptest.NewRecorder()
		inst.Handler().ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Errorf("token %s device %s: got %d want %d", tc.token, tc.device, w.Code, tc.want)
		}
	}
	if served != 1 {
		t.Fatalf("served %d requests", served)
	}
	inst, err = (Frontend{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: frontendtest.Identity{}, Sessions: sessions})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/api/v2/stream/session/scoped/file", nil)
	r.Header.Set("Authorization", "Bearer tv-grant")
	r.Header.Set("X-Silo-Device-Id", "tv")
	w := httptest.NewRecorder()
	inst.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("missing device resolver: %d", w.Code)
	}
}

func (fileIDs) Compact(context.Context, string, string, string) (int32, error) { return 1, nil }
func (fileIDs) Resolve(_ context.Context, s, k string, n int32) (string, error) {
	if s != "silo" || k != "file" || n != 1 {
		return "", errors.New("bad")
	}
	return `{"key":1000000000000001,"version":"cut"}`, nil
}
func TestPlaybackUsesScopedBroker(t *testing.T) {
	viewer := &frontend.Viewer{ID: 7}
	sessionID := "8c2d329e-71d2-4ea1-9b57-95187baeb0aa"
	hdr10Plus := true
	var planned frontend.PlanRequest
	var streamed frontend.StreamRequest
	var timeline frontend.Timeline
	var stopped string
	session := frontendtest.Sessions{PlanFunc: func(_ context.Context, r frontend.PlanRequest) (frontend.PlayPlan, error) {
		planned = r
		return frontend.PlayPlan{ID: "plan", Media: frontend.MediaInfo{Container: "mp4", Duration: time.Hour, Streams: []frontend.StreamInfo{{Type: "video", ColorTransfer: "smpte2084", HDR10Plus: &hdr10Plus}}}, Decision: frontend.Decision{Kind: "transcode", VideoCopy: true}}, nil
	}, OpenFunc: func(_ context.Context, r frontend.OpenRequest) (frontend.Session, error) {
		if r.PlanID != "plan" {
			t.Fatal("wrong plan")
		}
		return frontend.Session{ID: sessionID}, nil
	}, StartStreamFunc: func(_ context.Context, _ frontend.SessionAccess, _ string, r frontend.StreamRequest) (string, error) {
		streamed = r
		return "/sessions/media/" + sessionID + "/scoped/master.m3u8", nil
	}, SessionsFunc: func(_ context.Context, _ frontend.SessionAccess) ([]frontend.Session, error) {
		return []frontend.Session{{ID: sessionID, ViewerID: 7, Protocol: "silo", Device: frontend.Device{ID: "tv"}}}, nil
	}, ServeMediaFunc: func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sessions/media/"+sessionID+"/scoped/master.m3u8" {
			t.Fatal(r.URL.Path)
		}
		w.WriteHeader(200)
	},
		ReportFunc: func(_ context.Context, _ frontend.SessionAccess, _ string, r frontend.Timeline) error {
			timeline = r
			return nil
		}, StopFunc: func(_ context.Context, _ frontend.SessionAccess, id string) error { stopped = id; return nil }}
	ident := frontendtest.Identity{ResolveFunc: func(_ context.Context, s string) (*frontend.Viewer, error) {
		if s != "account-secret" {
			return nil, errors.New("bad")
		}
		return viewer, nil
	}}
	experience := frontendtest.Experience{ItemFunc: func(context.Context, frontend.ItemRequest) (frontend.ItemDetail, error) {
		return frontend.ItemDetail{Versions: []frontend.Version{{ID: "cut", Media: frontend.MediaInfo{Streams: []frontend.StreamInfo{{Type: "video", Index: 0}, {Type: "audio", Index: 2}, {Type: "subtitle", Index: 5}}}}}}, nil
	}}
	inst, err := (Frontend{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: testDeviceIdentity(ident, viewer), Experience: experience, Sessions: session, Capabilities: frontendtest.Capabilities{}, IDs: ids.Codecs{Allocator: fileIDs{}}})
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body string) *httptest.ResponseRecorder {
		body = strings.ReplaceAll(body, `"installation_id":"install"`, `"installation_id":"`+inst.(*instance).installationID()+`"`)
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer account-secret")
		req.Header.Set("X-Silo-Device-Id", "tv")
		w := httptest.NewRecorder()
		inst.Handler().ServeHTTP(w, req)
		return w
	}
	bad := call("POST", "/api/v2/playback/start", `{"protocol_version":3,"file_id":"1","profile_id":"7","installation_id":"stale","playback_attempt_id":"attempt"}`)
	if bad.Code != 409 || !strings.Contains(bad.Body.String(), `"type":"/problems/installation_changed"`) {
		t.Fatalf("stale installation: %d %s", bad.Code, bad.Body.String())
	}
	w := call("POST", "/api/v2/playback/start", `{"protocol_version":3,"file_id":"1","profile_id":"7","installation_id":"install","playback_attempt_id":"attempt","start_position":12}`)
	if w.Code != 201 || strings.Contains(w.Body.String(), "account-secret") || planned.Version != "cut" || planned.Item != 1000000000000001 || streamed.Format != frontend.StreamHLS {
		t.Fatalf("play %d %s %+v %+v", w.Code, w.Body.String(), planned, streamed)
	}
	if !strings.Contains(w.Body.String(), `"hdr10_plus":true`) {
		t.Fatalf("known HDR10+ not reported: %s", w.Body.String())
	}
	spec, e := openapi.CachedSpec("../../conform/specs/manifest.json", "silo", "")
	if e == nil {
		doc, e := openapi.Parse(spec)
		if e != nil {
			t.Fatal(e)
		}
		if e = doc.Validate("POST", "/api/v2/playback/start", w.Code, w.Header().Get("Content-Type"), w.Body.Bytes()); e != nil {
			t.Errorf("play spec: %v", e)
		}
	} else if !os.IsNotExist(e) {
		t.Fatal(e)
	}
	if !strings.Contains(w.Body.String(), "/api/v2/stream/"+sessionID+"/scoped/master.m3u8") || !strings.Contains(w.Body.String(), `"session_id":"`+sessionID+`"`) {
		t.Fatal("media URL not native", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"X-Silo-Device-Id":"tv"`) {
		t.Fatal("media request missing device binding", w.Body.String())
	}
	w = call("POST", "/api/v2/playback/start", `{"protocol_version":3,"file_id":"1","profile_id":"7","installation_id":"install","playback_attempt_id":"attempt","audio_track_index":0,"subtitle_track_index":0}`)
	if w.Code != 201 || planned.Intent.Options.AudioStream != 2 || planned.Intent.Options.SubtitleStream != 5 {
		t.Fatalf("track ordinal mapping: %d %+v", w.Code, planned.Intent.Options)
	}
	media := call("GET", "/api/v2/stream/"+sessionID+"/scoped/master.m3u8", "")
	if media.Code != 200 {
		t.Fatalf("media %d", media.Code)
	}
	wrongDevice := httptest.NewRequest("GET", "/api/v2/stream/"+sessionID+"/scoped/master.m3u8", nil)
	wrongDevice.Header.Set("Authorization", "Bearer account-secret")
	wrongDevice.Header.Set("X-Silo-Device-Id", "other")
	denied := httptest.NewRecorder()
	inst.Handler().ServeHTTP(denied, wrongDevice)
	if denied.Code != http.StatusUnauthorized {
		t.Fatalf("cross-device media: %d", denied.Code)
	}
	spoofProgress := httptest.NewRequest("POST", "/api/v2/playback/"+sessionID+"/progress", strings.NewReader(`{"installation_id":"`+inst.(*instance).installationID()+`","sequence":1,"position":5,"is_paused":false}`))
	spoofProgress.Header.Set("Authorization", "Bearer another-device-grant")
	spoofProgress.Header.Set("X-Silo-Device-Id", "tv")
	denied = httptest.NewRecorder()
	inst.Handler().ServeHTTP(denied, spoofProgress)
	if denied.Code != http.StatusUnauthorized || timeline.Sequence != 0 {
		t.Fatalf("cross-device progress: %d %+v", denied.Code, timeline)
	}

	var body map[string]any
	if json.Unmarshal(w.Body.Bytes(), &body) != nil || body["outcome"] != "playable" {
		t.Fatalf("response %s", w.Body.String())
	}
	w = call("POST", "/api/v2/playback/"+sessionID+"/progress", `{"installation_id":"install","sequence":1,"position":60,"is_paused":false}`)
	if w.Code != 200 || timeline.Position != time.Minute || timeline.State != "playing" || timeline.Sequence != 1 {
		t.Fatalf("progress %d %+v", w.Code, timeline)
	}
	if e == nil {
		doc, parseErr := openapi.Parse(spec)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		if err := doc.Validate("POST", "/api/v2/playback/"+sessionID+"/progress", w.Code, w.Header().Get("Content-Type"), w.Body.Bytes()); err != nil {
			t.Errorf("progress spec: %v", err)
		}
	}
	w = call("DELETE", "/api/v2/playback/"+sessionID, `{"installation_id":"install","stop_id":"stop"}`)
	if w.Code != 200 || stopped != sessionID || !strings.Contains(w.Body.String(), `"stop_id":"stop"`) {
		t.Fatalf("stop without final sample: %d %s", w.Code, w.Body.String())
	}
	w = call("DELETE", "/api/v2/playback/"+sessionID, `{"installation_id":"install","stop_id":"stop","sequence":2}`)
	if w.Code != 400 {
		t.Fatalf("partial final sample: %d %s", w.Code, w.Body.String())
	}

}

func TestPlaybackCapacityProblem(t *testing.T) {
	w := httptest.NewRecorder()
	playbackError(w, &frontend.TranscodeCapacityError{RetryAfter: 1500 * time.Millisecond})
	if w.Code != 503 || w.Header().Get("Retry-After") != "2" || w.Header().Get("Content-Type") != "application/problem+json" || !strings.Contains(w.Body.String(), "transcode_capacity") {
		t.Fatalf("capacity %d %s", w.Code, w.Body.String())
	}
}

func TestUnknownTransferRemainsPlayableAndUnknownPQIsSuppressed(t *testing.T) {
	positive, negative := true, false
	for _, tc := range []struct {
		transfer string
		probe    *bool
		want     bool
	}{{"bt709", nil, true}, {"arib-std-b67", nil, true}, {"", nil, true}, {"smpte2084", nil, false}, {"smpte2084", &positive, true}, {"smpte2084", &negative, true}} {
		media := frontend.MediaInfo{Streams: []frontend.StreamInfo{{Type: "video", ColorTransfer: tc.transfer, HDR10Plus: tc.probe}}}
		if got := siloSourceHasKnownHDR10Plus(media); got != tc.want {
			t.Errorf("transfer %q: got %v", tc.transfer, got)
		}
	}
}
