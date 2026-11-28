package silo

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/braedonsaunders/ether/internal/conform/openapi/spectest"
	"github.com/braedonsaunders/ether/internal/core"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/caps"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

type fileIDs struct{}

type liveTestIDs struct{ guid string }

func (a liveTestIDs) Compact(_ context.Context, surface, kind, ref string) (int32, error) {
	if surface != "silo" || kind != "live-channel" || ref != a.guid {
		return 0, errors.New("unexpected compact ID")
	}
	return 1, nil
}
func (a liveTestIDs) Resolve(_ context.Context, surface, kind string, n int32) (string, error) {
	if surface != "silo" || kind != "live-channel" || n != 1 {
		return "", errors.New("unknown compact ID")
	}
	return a.guid, nil
}

type testVersionHandles struct {
	issue func(frontend.VersionScope, frontend.RatingKey, string) (string, error)
}

func TestExactSourceOfferFailure(t *testing.T) {
	viewer := &frontend.Viewer{ID: 7}
	planned := 0
	sessions := frontendtest.Sessions{PlanFunc: func(context.Context, frontend.PlanRequest) (frontend.PlayPlan, error) {
		planned++
		return frontend.PlayPlan{}, nil
	}}
	for _, offers := range []frontend.SourceOffers{nil, testSourceOffers{resolve: func(scope frontend.VersionScope, id string) (frontend.SourceOffer, error) {
		if scope.Viewer != viewer || scope.Device.ID != "tv" || scope.Protocol != "silo" || len(scope.Caps.VideoCodecs) != 1 || id != "101" {
			t.Fatal("wrong offer scope")
		}
		return frontend.SourceOffer{}, frontend.ErrSourceGone
	}}} {
		inst, err := (Frontend{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: testDeviceIdentity(frontendtest.Identity{}, viewer), Sessions: sessions, SourceOffers: offers, Capabilities: frontendtest.Capabilities{BuildFunc: func(_ context.Context, req frontend.CapabilityRequest) (frontend.CapabilityResult, error) {
			return frontend.CapabilityResult{Client: frontend.ClientCaps{VideoCodecs: req.Device.VideoCodecs}}, nil
		}}})
		if err != nil {
			t.Fatal(err)
		}
		body := `{"protocol_version":3,"file_id":"101","profile_id":"7","installation_id":"` + inst.(*instance).installationID() + `","playback_attempt_id":"attempt","client_capabilities":{"codecs_video":["h264"]}}`
		r := httptest.NewRequest("POST", "/api/v2/playback/start", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer grant")
		r.Header.Set("X-Silo-Device-Id", "tv")
		w := httptest.NewRecorder()
		inst.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "version-unavailable") || planned != 0 {
			t.Fatalf("expired or missing offer: %d %s planned=%d", w.Code, w.Body.String(), planned)
		}
	}
}

func (h testVersionHandles) Issue(_ context.Context, scope frontend.VersionScope, key frontend.RatingKey, release string) (string, error) {
	if h.issue != nil {
		return h.issue(scope, key, release)
	}
	return "handle", nil
}
func (testVersionHandles) Resolve(_ context.Context, _ frontend.VersionScope, handle string) (frontend.VersionTarget, error) {
	if handle != "handle" {
		return frontend.VersionTarget{}, errors.New("bad handle")
	}
	return frontend.VersionTarget{Item: 1000000000000001}, nil
}
func (testVersionHandles) MatchSession(context.Context, frontend.VersionScope, string, string) error {
	return nil
}

func TestPlaybackAdmissionBusyReturnsRetryableProblem(t *testing.T) {
	w := httptest.NewRecorder()
	playbackError(w, &frontend.SessionBusyError{RetryAfter: 1500 * time.Millisecond})
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != "2" || w.Header().Get("Content-Type") != "application/problem+json" || strings.Contains(w.Body.String(), "transcode") {
		t.Fatalf("busy response: %d %v %s", w.Code, w.Header(), w.Body.String())
	}
	if doc := spectest.Document(t, "silo"); doc != nil {
		if err := doc.Validate("POST", "/api/v2/playback/start", w.Code, w.Header().Get("Content-Type"), w.Body.Bytes()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPlaybackPreparingQueuedReturnsRetryableProblem(t *testing.T) {
	w := httptest.NewRecorder()
	playbackError(w, &frontend.PreparingError{RetryAfter: 5 * time.Second, Err: &frontend.UncachedError{Err: frontend.ErrDownloadQueued}})
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != "5" || w.Header().Get("X-Ether-Media-Error") != "preparing" || w.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatalf("preparing response: %d %v %s", w.Code, w.Header(), w.Body.String())
	}
	if doc := spectest.Document(t, "silo"); doc != nil {
		if err := doc.Validate("POST", "/api/v2/playback/start", w.Code, w.Header().Get("Content-Type"), w.Body.Bytes()); err != nil {
			t.Fatal(err)
		}
	}
	w2 := httptest.NewRecorder()
	playbackError(w2, &frontend.PreparingError{RetryAfter: 5 * time.Second, Err: &frontend.UncachedError{Err: &frontend.DownloadQueuedError{Provider: "TorBox", ActiveDownloads: 3, Err: frontend.ErrDownloadQueued}}})
	if w2.Code != http.StatusServiceUnavailable || !strings.Contains(w2.Body.String(), "Waiting for the download service (queued at TorBox behind 3 active downloads)…") {
		t.Fatalf("preparing queued-behind response: %d %s", w2.Code, w2.Body.String())
	}
}

func TestTranscodeCapacityReturnsValidRetryableProblem(t *testing.T) {
	w := httptest.NewRecorder()
	playbackError(w, &frontend.TranscodeCapacityError{RetryAfter: 1200 * time.Millisecond})
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != "2" || w.Header().Get("X-Request-ID") == "" || !strings.Contains(w.Body.String(), "transcode_capacity") {
		t.Fatalf("transcode response: %d %v %s", w.Code, w.Header(), w.Body.String())
	}
	if doc := spectest.Document(t, "silo"); doc != nil {
		if err := doc.Validate("POST", "/api/v2/playback/start", w.Code, w.Header().Get("Content-Type"), w.Body.Bytes()); err != nil {
			t.Fatal(err)
		}
	}
}

type fakeBroker = frontendtest.Sessions
type testScopedMedia struct{ fakeBroker }

func (s testScopedMedia) ServeMediaAccess(w http.ResponseWriter, r *http.Request, a frontend.SessionAccess) {
	if a.Viewer == nil || a.Viewer.ID != 7 || a.Protocol != "silo" || a.Device.ID != "tv" {
		http.NotFound(w, r)
		return
	}
	s.ServeMedia(w, r)
}

func (s testScopedMedia) ServeSessionMedia(w http.ResponseWriter, r *http.Request, a frontend.SessionAccess, sessionID, resource string) {
	forward := r.Clone(r.Context())
	u := *r.URL
	u.Path = "/sessions/media/" + sessionID + "/scoped/" + resource
	u.RawPath = ""
	forward.URL = &u
	s.ServeMediaAccess(w, forward, a)
}

func TestMediaRequiresBoundLiveDeviceGrant(t *testing.T) {
	viewer := &frontend.Viewer{ID: 7}
	revoked := false
	identity := deviceTestIdentity{resolveDevice: func(_ context.Context, token string, d frontend.Device) (*frontend.Viewer, time.Time, error) {
		switch token {
		case "tv-grant":
			if d.ID == "tv" && !revoked {
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
	inst, err := (Frontend{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: identity, Sessions: testScopedMedia{sessions}})
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
		r := httptest.NewRequest("GET", "/api/v2/stream/session", nil)
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
	revoked = true
	request := httptest.NewRequest("GET", "/api/v2/stream/session", nil)
	request.Header.Set("Authorization", "Bearer tv-grant")
	request.Header.Set("X-Silo-Device-Id", "tv")
	revokedResponse := httptest.NewRecorder()
	inst.Handler().ServeHTTP(revokedResponse, request)
	if revokedResponse.Code != http.StatusUnauthorized || served != 1 {
		t.Fatalf("revoked after open: status %d, served %d", revokedResponse.Code, served)
	}
	revoked = false
	inst, err = (Frontend{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: frontendtest.Identity{}, Sessions: testScopedMedia{sessions}})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/api/v2/stream/session", nil)
	r.Header.Set("Authorization", "Bearer tv-grant")
	r.Header.Set("X-Silo-Device-Id", "tv")
	w := httptest.NewRecorder()
	inst.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("missing device resolver: %d", w.Code)
	}
	inst, err = (Frontend{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: identity, Sessions: sessions})
	if err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	inst.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("missing scoped broker authorization: %d", w.Code)
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
	var openErr error
	var planErr error
	session := frontendtest.Sessions{PlanFunc: func(_ context.Context, r frontend.PlanRequest) (frontend.PlayPlan, error) {
		planned = r
		if planErr != nil {
			return frontend.PlayPlan{}, planErr
		}
		return frontend.PlayPlan{ID: "plan", Media: frontend.MediaInfo{Container: "mp4", Duration: time.Hour, Streams: []frontend.StreamInfo{{Type: "video", ColorTransfer: "bt709", HDR10Plus: &hdr10Plus}, {Type: "audio", Index: 1, Codec: "aac"}, {Type: "audio", Index: 2, Codec: "ac3"}}}, Decision: frontend.Decision{Kind: "transcode", VideoCopy: true, AudioStream: 1}}, nil
	}, OpenFunc: func(_ context.Context, r frontend.OpenRequest) (frontend.Session, error) {
		if openErr != nil {
			return frontend.Session{}, openErr
		}
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
		if !strings.HasPrefix(r.URL.Path, "/sessions/media/"+sessionID+"/scoped/") || strings.Contains(r.URL.Path, "secret") {
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
	inst, err := (Frontend{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: testDeviceIdentity(ident, viewer), Experience: experience, Sessions: testScopedMedia{session}, Capabilities: frontendtest.Capabilities{}, VersionHandles: testVersionHandles{}, SourceOffers: testSourceOffers{}, IDs: ids.Codecs{Allocator: fileIDs{}}})
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body string) *httptest.ResponseRecorder {
		body = strings.ReplaceAll(body, `"installation_id":"install"`, `"installation_id":"`+inst.(*instance).installationID()+`"`)
		if path == "/api/v2/playback/start" {
			body = strings.TrimSuffix(body, "}") + `,"client_capabilities":{"codecs_video":["h264"]}}`
		}
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer account-secret")
		req.Header.Set("X-Silo-Device-Id", "tv")
		w := httptest.NewRecorder()
		inst.Handler().ServeHTTP(w, req)
		return w
	}
	bad := call("POST", "/api/v2/playback/start", `{"protocol_version":3,"file_id":"101","profile_id":"7","installation_id":"stale","playback_attempt_id":"attempt"}`)
	if bad.Code != 409 || !strings.Contains(bad.Body.String(), `"type":"/problems/installation_changed"`) {
		t.Fatalf("stale installation: %d %s", bad.Code, bad.Body.String())
	}
	w := call("POST", "/api/v2/playback/start", `{"protocol_version":3,"file_id":"101","profile_id":"7","installation_id":"install","playback_attempt_id":"attempt","start_position":12}`)
	// The playback start is a viewer pressing Play (PlayIntent.ViewerPlay).
	if w.Code != 201 || strings.Contains(w.Body.String(), "account-secret") || planned.Version != "handle" || !planned.Intent.ExactVersion || !planned.Intent.ViewerPlay || planned.Item != 1000000000000001 || streamed.Format != frontend.StreamHLS {
		t.Fatalf("play %d %s %+v %+v", w.Code, w.Body.String(), planned, streamed)
	}
	if planned.Intent.AudioOrdinal != nil || planned.Intent.SubtitleOrdinal != nil {
		t.Fatalf("omitted tracks must preserve preferences: %+v", planned.Intent)
	}
	if !strings.Contains(w.Body.String(), `"hdr10_plus":true`) {
		t.Fatalf("known HDR10+ not reported: %s", w.Body.String())
	}
	// No fixed transcode ladder exists, and the source is not a menu rung.
	if !strings.Contains(w.Body.String(), `"available_qualities":[]`) {
		t.Fatalf("invented quality rungs: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"color_range":"unknown"`) {
		t.Fatalf("unknown color range missing: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"dynamic_range":"hdr10_plus"`) || strings.Contains(w.Body.String(), `"dynamic_range":"sdr"`) {
		t.Fatalf("positive HDR10+ mislabeled as SDR: %s", w.Body.String())
	}
	doc := spectest.Document(t, "silo")
	if doc != nil {
		if e := doc.Validate("POST", "/api/v2/playback/start", w.Code, w.Header().Get("Content-Type"), w.Body.Bytes()); e != nil {
			t.Errorf("play spec: %v", e)
		}
	}
	wantMedia := "/api/v2/playback/transcode/" + sessionID + "/master.m3u8"
	if !strings.Contains(w.Body.String(), `"url":"`+wantMedia+`"`) || strings.Contains(w.Body.String(), "/scoped/") || !strings.Contains(w.Body.String(), `"session_id":"`+sessionID+`"`) {
		t.Fatal("media URL not native", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"X-Silo-Device-Id":"tv"`) {
		t.Fatal("media request missing device binding", w.Body.String())
	}
	w = call("POST", "/api/v2/playback/start", `{"protocol_version":3,"file_id":"101","profile_id":"7","installation_id":"install","playback_attempt_id":"attempt","audio_track_index":0,"subtitle_track_index":5}`)
	if w.Code != 201 || planned.Intent.AudioOrdinal == nil || *planned.Intent.AudioOrdinal != 0 || planned.Intent.SubtitleOrdinal == nil || *planned.Intent.SubtitleOrdinal != 5 || planned.Intent.AudioSet || planned.Intent.SubtitleSet {
		t.Fatalf("source-bound track selection: %d %+v", w.Code, planned.Intent)
	}
	w = call("POST", "/api/v2/playback/start", `{"protocol_version":3,"file_id":"101","profile_id":"7","installation_id":"install","playback_attempt_id":"attempt","subtitle_track_index":-1}`)
	if w.Code != 201 || planned.Intent.AudioOrdinal != nil || planned.Intent.SubtitleOrdinal == nil || *planned.Intent.SubtitleOrdinal != -1 {
		t.Fatalf("subtitle off: %d %+v", w.Code, planned.Intent)
	}
	for _, tc := range []struct {
		err    error
		status int
	}{{frontend.ErrTrackNotFound, 422}, {frontend.ErrTrackSourceUnavailable, 503}} {
		planErr = tc.err
		failed := call("POST", "/api/v2/playback/start", `{"protocol_version":3,"file_id":"101","profile_id":"7","installation_id":"install","playback_attempt_id":"attempt","subtitle_track_index":5}`)
		if failed.Code != tc.status {
			t.Fatalf("track admission error %v: %d %s", tc.err, failed.Code, failed.Body.String())
		}
	}
	planErr = nil
	for _, body := range []string{
		`{"protocol_version":3,"file_id":"101","profile_id":"7","installation_id":"install","playback_attempt_id":"attempt","audio_track_index":-1}`,
		`{"protocol_version":3,"file_id":"101","profile_id":"7","installation_id":"install","playback_attempt_id":"attempt","subtitle_track_index":-2}`,
	} {
		if invalid := call("POST", "/api/v2/playback/start", body); invalid.Code != 400 {
			t.Fatalf("invalid track: %d %s", invalid.Code, invalid.Body.String())
		}
	}
	media := call("GET", "/api/v2/playback/transcode/"+sessionID+"/master.m3u8", "")
	if media.Code != 200 {
		t.Fatalf("media %d", media.Code)
	}
	variant := call("GET", "/api/v2/playback/transcode/"+sessionID+"/base/index.m3u8?seek=12", "")
	if variant.Code != 200 {
		t.Fatalf("variant %d", variant.Code)
	}
	replan := call("POST", "/api/v2/playback/"+sessionID+"/replan", `{"installation_id":"install","protocol_version":3,"playback_attempt_id":"attempt","replan_request_id":"apple-replan-1","failed_plan_id":"failedplan","plan_attempt_id":"planattempt","plan_attempt_key":"plankey123","attempted_plan_keys":["plankey123"],"attempt_count":1,"quality_preference":"auto","position_seconds":12,"selected_tracks":{},"client_capabilities":{},"client_playback_context":{}}`)
	if replan.Code != 200 || !strings.Contains(replan.Body.String(), wantMedia) || !strings.Contains(replan.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("replan %d %s", replan.Code, replan.Body.String())
	}
	// Silo Android sends every track pick as a track_change intent with no
	// attempt history. The playing session takes the selection (its server
	// stream restarts on the new audio); no subtitle means off.
	streamed = frontend.StreamRequest{}
	pick := call("POST", "/api/v2/playback/"+sessionID+"/replan", `{"installation_id":"install","protocol_version":3,"operation":"track_change","playback_attempt_id":"attempt","replan_request_id":"android-pick-1","failed_plan_id":"failedplan","plan_attempt_id":"planattempt","plan_attempt_key":"plankey123","attempted_plan_keys":[],"attempt_count":1,"quality_preference":"auto","position_seconds":40,"selected_tracks":{"audio":{"id":"file:101:audio:1","index":1}},"client_capabilities":{"codecs_video":["h264"]},"client_playback_context":{}}`)
	if pick.Code != 200 || streamed.AudioStream == nil || *streamed.AudioStream != 2 || streamed.Position != 40*time.Second || !strings.Contains(pick.Body.String(), `"session_id":"`+sessionID+`"`) || !strings.Contains(pick.Body.String(), `"audio":{"id":"file:101:audio:1","index":1}`) || !strings.Contains(pick.Body.String(), `"mode":"off"`) {
		t.Fatalf("track pick %d %s %+v", pick.Code, pick.Body.String(), streamed)
	}
	missing := call("POST", "/api/v2/playback/missing-session/replan", `{"installation_id":"install","protocol_version":3,"playback_attempt_id":"attempt","replan_request_id":"apple-replan-2","failed_plan_id":"failedplan","plan_attempt_id":"planattempt","plan_attempt_key":"plankey123","attempted_plan_keys":["plankey123"],"attempt_count":1,"quality_preference":"auto","position_seconds":1,"selected_tracks":{},"client_capabilities":{},"client_playback_context":{}}`)
	if missing.Code != 404 || missing.Header().Get("Content-Type") != "application/problem+json" || !strings.Contains(missing.Body.String(), `"status":404`) {
		t.Fatalf("missing replan %d %s", missing.Code, missing.Body.String())
	}
	routeEvent := call("POST", "/api/v2/playback/route-events", `{"installation_id":"install","event_id":"event-1","protocol_version":3,"playback_attempt_id":"attempt","event":"plan_failed","diagnostics":{"error_cause":"ValidationError"}}`)
	if routeEvent.Code != 202 || !strings.Contains(routeEvent.Body.String(), `"outcome":"accepted"`) || !strings.Contains(routeEvent.Body.String(), `"event_id":"event-1"`) {
		t.Fatalf("route event %d %s", routeEvent.Code, routeEvent.Body.String())
	}
	images := call("GET", "/api/v2/images/capabilities", "")
	if images.Code != 200 || !strings.Contains(images.Body.String(), `"state":"available"`) || !strings.Contains(images.Body.String(), `"still_width"`) {
		t.Fatalf("image capabilities %d %s", images.Code, images.Body.String())
	}
	wrongDevice := httptest.NewRequest("GET", "/api/v2/playback/transcode/"+sessionID+"/master.m3u8", nil)
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
	if doc != nil {
		if err := doc.Validate("POST", "/api/v2/playback/"+sessionID+"/progress", w.Code, w.Header().Get("Content-Type"), w.Body.Bytes()); err != nil {
			t.Errorf("progress spec: %v", err)
		}
	}
	w = call("DELETE", "/api/v2/playback/"+sessionID, `{"installation_id":"install","stop_id":"stop"}`)
	if w.Code != 200 || stopped != sessionID || !strings.Contains(w.Body.String(), `"stop_id":"stop"`) {
		t.Fatalf("stop without final sample: %d %s", w.Code, w.Body.String())
	}
	if _, ok := inst.(*instance).rememberedPlan(sessionID); ok {
		t.Fatal("playback plan retained after stop")
	}
	w = call("DELETE", "/api/v2/playback/"+sessionID, `{"installation_id":"install","stop_id":"stop","sequence":2}`)
	if w.Code != 400 {
		t.Fatalf("partial final sample: %d %s", w.Code, w.Body.String())
	}
	openErr = &frontend.TranscodeCapacityError{RetryAfter: 1500 * time.Millisecond}
	w = call("POST", "/api/v2/playback/start", `{"protocol_version":3,"file_id":"101","profile_id":"7","installation_id":"install","playback_attempt_id":"another"}`)
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != "2" || !strings.Contains(w.Body.String(), "transcode_capacity") {
		t.Fatalf("open capacity: %d %v %s", w.Code, w.Header(), w.Body.String())
	}
	openErr = nil
	planErr = errors.New("exact release unavailable")
	w = call("POST", "/api/v2/playback/start", `{"protocol_version":3,"file_id":"101","profile_id":"7","installation_id":"install","playback_attempt_id":"another"}`)
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "version-unavailable") {
		t.Fatalf("exact plan failure: %d %s", w.Code, w.Body.String())
	}
}

func TestPlaybackCapacityProblem(t *testing.T) {
	w := httptest.NewRecorder()
	playbackError(w, &frontend.TranscodeCapacityError{RetryAfter: 1500 * time.Millisecond})
	if w.Code != 503 || w.Header().Get("Retry-After") != "2" || w.Header().Get("Content-Type") != "application/problem+json" || !strings.Contains(w.Body.String(), "transcode_capacity") {
		t.Fatalf("capacity %d %s", w.Code, w.Body.String())
	}
}

func TestOnNowChannelStartsLivePlayback(t *testing.T) {
	viewer := &frontend.Viewer{ID: 7}
	channelID, err := ids.StringGUID(ids.Channel, "news")
	if err != nil {
		t.Fatal(err)
	}
	var tuned string
	var format frontend.StreamFormat
	sessions := frontendtest.Sessions{
		PlanFunc: func(_ context.Context, r frontend.PlanRequest) (frontend.PlayPlan, error) {
			if r.Item != 0 || r.Channel != "news" || r.Protocol != "silo" || r.Intent.AllowTranscoding == nil || !*r.Intent.AllowTranscoding {
				t.Fatal(r.Item, r.Channel, r.Protocol, r.Intent.AllowTranscoding)
			}
			tuned = r.Channel
			return frontend.PlayPlan{ID: "plan", Decision: core.Decision{Kind: core.DecisionRemux}, Media: core.MediaInfo{Container: "mpegts", Estimated: true}}, nil
		},
		OpenFunc: func(context.Context, frontend.OpenRequest) (frontend.Session, error) {
			return frontend.Session{ID: "session"}, nil
		},
		StartStreamFunc: func(_ context.Context, _ frontend.SessionAccess, _ string, r frontend.StreamRequest) (string, error) {
			format = r.Format
			return "/sessions/media/session/grant/live/index.m3u8", nil
		},
	}
	live := frontendtest.LiveTV{ResolveChannelFunc: func(_ context.Context, v *frontend.Viewer, id string) (frontend.LiveChannel, error) {
		if v != viewer || id != channelID {
			t.Fatal(v, id)
		}
		return frontend.LiveChannel{ID: "news", Name: "News", Number: "2"}, nil
	}}
	inst, err := (Frontend{MachineID: "test"}).Mount(context.Background(), frontend.Host{
		Identity: testDeviceIdentity(frontendtest.Identity{}, viewer), Sessions: sessions, LiveTV: live, Capabilities: frontendtest.Capabilities{},
	})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"protocol_version":3,"file_id":"` + channelID + `","profile_id":"7","installation_id":"` + inst.(*instance).installationID() + `","playback_attempt_id":"attempt","start_position":12}`
	r := httptest.NewRequest("POST", "/api/v2/playback/start", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer grant")
	r.Header.Set("X-Silo-Device-Id", "tv")
	w := httptest.NewRecorder()
	inst.Handler().ServeHTTP(w, r)
	if w.Code != 201 || tuned != "news" || format != frontend.StreamHLS || !strings.Contains(w.Body.String(), `/api/v2/playback/transcode/session/master.m3u8`) || !strings.Contains(w.Body.String(), `"delivery":"server_remux_hls"`) || !strings.Contains(w.Body.String(), `"can_seek_anywhere":false`) {
		t.Fatal(w.Code, tuned, format, w.Body.String())
	}
}

func TestOnNowLiveCardFullAppleTVSequence(t *testing.T) {
	viewer := &frontend.Viewer{ID: 7}
	channelID, err := ids.StringGUID(ids.Channel, "news")
	if err != nil {
		t.Fatal(err)
	}
	otherChannelID, err := ids.StringGUID(ids.Channel, "restricted")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Minute)
	start := now.Add(-15 * time.Minute)
	end := start.Add(time.Hour)
	sessionID := "8c2d329e-71d2-4ea1-9b57-95187baeb0aa"
	var tuned string
	var format frontend.StreamFormat
	var stopped string
	var reported frontend.Timeline

	exp := frontendtest.Experience{
		RowsFunc: func(context.Context, frontend.RowRequest) ([]frontend.Row, error) {
			return []frontend.Row{{
				ID: "live", Title: "On Now", Purpose: frontend.RowLive,
				Entries: []frontend.RowEntry{{Kind: frontend.RowEntryLiveChannel, LiveChannel: &frontend.RowLiveChannel{
					ID: "news", Number: "2", Name: "News",
					Current: &frontend.RowProgramme{ID: "prog-1", Title: "Evening News", Start: start, End: end},
				}}},
			}}, nil
		},
		UserDataFunc: func(context.Context, *frontend.Viewer, []frontend.RatingKey) (map[frontend.RatingKey]frontend.WatchStatus, error) {
			return map[frontend.RatingKey]frontend.WatchStatus{}, nil
		},
	}
	live := frontendtest.LiveTV{
		ChannelsFunc: func(_ context.Context, v *frontend.Viewer) ([]frontend.LiveChannel, error) {
			if v != viewer {
				return nil, frontend.ErrUnauthorized
			}
			return []frontend.LiveChannel{{ID: "news", Number: "2", Name: "News", Favorite: true, Images: []frontend.Image{{Kind: frontend.ImageChannel, Path: "/channel/news.png"}}}}, nil
		},
		ResolveChannelFunc: func(_ context.Context, v *frontend.Viewer, id string) (frontend.LiveChannel, error) {
			if v != viewer || !strings.EqualFold(id, channelID) {
				return frontend.LiveChannel{}, frontend.ErrNotFound
			}
			return frontend.LiveChannel{ID: "news", Number: "2", Name: "News", Favorite: true, Images: []frontend.Image{{Kind: frontend.ImageChannel, Path: "/channel/news.png"}}}, nil
		},
		GuideFunc: func(_ context.Context, v *frontend.Viewer, q frontend.LiveGuideQuery) ([]frontend.LiveProgramme, error) {
			if v != viewer {
				return nil, frontend.ErrUnauthorized
			}
			return []frontend.LiveProgramme{{
				ID:        "prog-1",
				ChannelID: "news",
				Title:     "Evening News",
				Summary:   "Live local headlines and weather.",
				Rating:    "TV-G",
				Start:     start,
				End:       end,
			}}, nil
		},
	}
	sessions := testScopedMedia{frontendtest.Sessions{
		PlanFunc: func(_ context.Context, r frontend.PlanRequest) (frontend.PlayPlan, error) {
			if r.Item != 0 || r.Channel != "news" || r.Protocol != "silo" || r.Intent.AllowTranscoding == nil || !*r.Intent.AllowTranscoding {
				t.Fatalf("unexpected live plan request: %+v", r)
			}
			tuned = r.Channel
			return frontend.PlayPlan{ID: "live-plan", Decision: core.Decision{Kind: core.DecisionRemux}, Media: core.MediaInfo{Container: "mpegts", Estimated: true}}, nil
		},
		OpenFunc: func(context.Context, frontend.OpenRequest) (frontend.Session, error) {
			return frontend.Session{ID: sessionID}, nil
		},
		StartStreamFunc: func(_ context.Context, _ frontend.SessionAccess, _ string, r frontend.StreamRequest) (string, error) {
			format = r.Format
			return "/sessions/media/" + sessionID + "/scoped/live/index.m3u8", nil
		},
		ServeMediaFunc: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("#EXTM3U\n#EXT-X-VERSION:3\n"))
		},
		ReportFunc: func(_ context.Context, _ frontend.SessionAccess, _ string, tl frontend.Timeline) error {
			reported = tl
			return nil
		},
		StopFunc: func(_ context.Context, _ frontend.SessionAccess, id string) error {
			stopped = id
			return nil
		},
	}}
	grants := frontendtest.ImageGrants{
		IssueFunc: func(_ context.Context, req frontend.ImageGrantRequest) (string, error) {
			if req.Channel != "news" {
				t.Fatalf("expected channel image grant for news, got %+v", req)
			}
			return "channel-news-grant", nil
		},
	}
	host := frontend.Host{
		IDs:          ids.Codecs{Allocator: liveTestIDs{guid: channelID}},
		Identity:     testDeviceIdentity(frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}, viewer),
		Experience:   exp,
		LiveTV:       live,
		Sessions:     sessions,
		Capabilities: frontendtest.Capabilities{},
		ImageGrants:  grants,
	}
	inst, err := (Frontend{MachineID: "test"}).Mount(context.Background(), host)
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body string) *httptest.ResponseRecorder {
		var reader *strings.Reader
		if body != "" {
			reader = strings.NewReader(body)
		} else {
			reader = strings.NewReader("")
		}
		r := httptest.NewRequest(method, path, reader)
		r.Header.Set("Authorization", "Bearer grant")
		r.Header.Set("X-Silo-Device-Id", "tv")
		w := httptest.NewRecorder()
		inst.Handler().ServeHTTP(w, r)
		return w
	}

	doc := spectest.Document(t, "silo")
	validate := func(method, path string, w *httptest.ResponseRecorder) {
		t.Helper()
		if doc == nil {
			return
		}
		if err := doc.Validate(method, path, w.Code, w.Header().Get("Content-Type"), w.Body.Bytes()); err != nil {
			t.Fatalf("%s %s spec validation failed: %v\nbody: %s", method, path, err, w.Body.String())
		}
	}

	// 1. Home sections include the On Now live shelf with 16:9 episode-shaped card.
	home := call("GET", "/api/v2/home/sections", "")
	if home.Code != http.StatusOK {
		t.Fatalf("home sections: %d %s", home.Code, home.Body.String())
	}
	validate("GET", "/api/v2/home/sections", home)
	var homeDoc struct {
		Sections []struct {
			SectionType string           `json:"section_type"`
			Items       []map[string]any `json:"items"`
		} `json:"sections"`
	}
	if err := json.Unmarshal(home.Body.Bytes(), &homeDoc); err != nil {
		t.Fatal(err)
	}
	if len(homeDoc.Sections) != 1 || homeDoc.Sections[0].SectionType != "live" || len(homeDoc.Sections[0].Items) != 1 {
		t.Fatalf("unexpected home sections: %+v", homeDoc)
	}
	card := homeDoc.Sections[0].Items[0]
	if card["content_id"] != channelID || card["play_content_id"] != channelID || card["type"] != "episode" || card["item_source"] != "live" || card["title"] != "Evening News" || card["series_title"] != "2 News" {
		t.Fatalf("unexpected On Now card: %+v", card)
	}

	// 2. Focusing/tapping the On Now card fetches GET /api/v2/catalog/items/{id}.
	detailPath := "/api/v2/catalog/items/" + channelID
	detailRes := call("GET", detailPath, "")
	if detailRes.Code != http.StatusOK {
		t.Fatalf("catalog item detail: %d %s", detailRes.Code, detailRes.Body.String())
	}
	validate("GET", detailPath, detailRes)
	var detailDoc map[string]any
	if err := json.Unmarshal(detailRes.Body.Bytes(), &detailDoc); err != nil {
		t.Fatal(err)
	}
	if detailDoc["content_id"] != channelID || detailDoc["type"] != "movie" || detailDoc["title"] != "Evening News" || detailDoc["overview"] != "Live local headlines and weather." || detailDoc["series_title"] != "2 News" {
		t.Fatalf("unexpected live detail payload: %+v", detailDoc)
	}
	if detailDoc["poster_url"] != "/api/v2/images/channel-news-grant" || detailDoc["backdrop_url"] != "/api/v2/images/channel-news-grant" {
		t.Fatalf("missing channel artwork grant on detail: %+v", detailDoc)
	}
	detailVersions, ok := detailDoc["versions"].([]any)
	if !ok || len(detailVersions) != 1 {
		t.Fatalf("expected 1 version on live detail, got %+v", detailDoc["versions"])
	}
	detailVersion := detailVersions[0].(map[string]any)
	fileID, _ := detailVersion["file_id"].(string)
	parsedFileID, err := strconv.ParseInt(fileID, 10, 64)
	if err != nil || parsedFileID <= 0 || strconv.FormatInt(parsedFileID, 10) != fileID {
		t.Fatalf("silo-apple catalogLegacyID requires positive decimal integer file_id, got %q (err=%v)", fileID, err)
	}

	// 3. ItemDetailViewModel & PlaybackSessionBridge fetch GET /api/v2/watch/{id}.
	watchPath := "/api/v2/watch/" + channelID
	watchRes := call("GET", watchPath, "")
	if watchRes.Code != http.StatusOK {
		t.Fatalf("watch detail: %d %s", watchRes.Code, watchRes.Body.String())
	}
	validate("GET", watchPath, watchRes)
	var watchDoc map[string]any
	if err := json.Unmarshal(watchRes.Body.Bytes(), &watchDoc); err != nil {
		t.Fatal(err)
	}
	watchVersions, ok := watchDoc["versions"].([]any)
	if !ok || len(watchVersions) != 1 {
		t.Fatalf("expected 1 watch version, got %+v", watchDoc["versions"])
	}
	watchVersion := watchVersions[0].(map[string]any)
	if watchVersion["file_id"] != fileID || watchVersion["duration_seconds"] != float64(3600) {
		t.Fatalf("unexpected watch version: %+v", watchVersion)
	}

	// 4. Versions endpoint GET /api/v2/catalog/items/{id}/versions.
	versionsPath := "/api/v2/catalog/items/" + channelID + "/versions"
	versionsRes := call("GET", versionsPath, "")
	if versionsRes.Code != http.StatusOK {
		t.Fatalf("versions: %d %s", versionsRes.Code, versionsRes.Body.String())
	}
	validate("GET", versionsPath, versionsRes)

	// 5. TVSimilarRail on TVMovieDetailView fetches GET /api/v2/recommendations/similar/{id}?limit=12.
	similarPath := "/api/v2/recommendations/similar/" + channelID
	similarRes := call("GET", similarPath+"?limit=12", "")
	if similarRes.Code != http.StatusOK || !strings.Contains(similarRes.Body.String(), `"items":[]`) {
		t.Fatalf("similar: %d %s", similarRes.Code, similarRes.Body.String())
	}
	validate("GET", similarPath, similarRes)

	// 6. Capabilities fetch before playback start.
	capRes := call("GET", "/api/v2/playback/capabilities", "")
	if capRes.Code != http.StatusOK {
		t.Fatalf("capabilities: %d %s", capRes.Code, capRes.Body.String())
	}
	validate("GET", "/api/v2/playback/capabilities", capRes)
	var capDoc struct {
		InstallationID string `json:"installation_id"`
	}
	if err := json.Unmarshal(capRes.Body.Bytes(), &capDoc); err != nil || capDoc.InstallationID == "" {
		t.Fatalf("capabilities decode: %v %+v", err, capDoc)
	}

	// 7. PlaybackSessionBridge starts playback using the positive integer file_id from watch.versions[0].file_id.
	startBody := `{"protocol_version":3,"file_id":"` + fileID + `","profile_id":"7","installation_id":"` + capDoc.InstallationID + `","playback_attempt_id":"attempt-live-1","quality_preference":"auto","start_position":0,"client_capabilities":{"codecs_video":["h264","hevc"],"codecs_audio":["aac","ac3"],"containers":["mp4","mov"],"hdr":true,"max_resolution":"2160p"}}`
	startRes := call("POST", "/api/v2/playback/start", startBody)
	if startRes.Code != http.StatusCreated {
		t.Fatalf("playback start: %d %s", startRes.Code, startRes.Body.String())
	}
	validate("POST", "/api/v2/playback/start", startRes)
	var startDoc struct {
		Outcome      string `json:"outcome"`
		SessionID    string `json:"session_id"`
		PlaybackPlan struct {
			Delivery             string `json:"delivery"`
			RequestedMediaFileID string `json:"requested_media_file_id"`
			EffectiveMediaFileID string `json:"effective_media_file_id"`
			Source               struct {
				MediaFileID string `json:"media_file_id"`
			} `json:"source"`
			Stream struct {
				URL      string `json:"url"`
				Protocol string `json:"protocol"`
			} `json:"stream"`
			Timeline struct {
				CanSeekAnywhere bool `json:"can_seek_anywhere"`
			} `json:"timeline"`
		} `json:"playback_plan"`
	}
	if err := json.Unmarshal(startRes.Body.Bytes(), &startDoc); err != nil {
		t.Fatal(err)
	}
	if startDoc.Outcome != "playable" || tuned != "news" || format != frontend.StreamHLS ||
		startDoc.PlaybackPlan.RequestedMediaFileID != fileID ||
		startDoc.PlaybackPlan.EffectiveMediaFileID != fileID ||
		startDoc.PlaybackPlan.Source.MediaFileID != fileID ||
		startDoc.PlaybackPlan.Delivery != "server_remux_hls" ||
		startDoc.PlaybackPlan.Timeline.CanSeekAnywhere ||
		startDoc.PlaybackPlan.Stream.URL != "/api/v2/playback/transcode/"+sessionID+"/master.m3u8" {
		t.Fatalf("unexpected playback start plan: %+v", startDoc)
	}

	// 8. Stateless resolution: a fresh instance that has not served home/detail yet
	// still resolves the numeric live file_id via LiveTV.Channels.
	freshInst, err := (Frontend{MachineID: "test"}).Mount(context.Background(), host)
	if err != nil {
		t.Fatal(err)
	}
	freshReq := httptest.NewRequest("POST", "/api/v2/playback/start", strings.NewReader(startBody))
	freshReq.Header.Set("Authorization", "Bearer grant")
	freshReq.Header.Set("X-Silo-Device-Id", "tv")
	freshRes := httptest.NewRecorder()
	freshInst.Handler().ServeHTTP(freshRes, freshReq)
	if freshRes.Code != http.StatusCreated {
		t.Fatalf("stateless live playback start failed: %d %s", freshRes.Code, freshRes.Body.String())
	}

	// 9. Fetch HLS master playlist, report progress, and stop session.
	hlsRes := call("GET", startDoc.PlaybackPlan.Stream.URL, "")
	if hlsRes.Code != http.StatusOK || !strings.Contains(hlsRes.Body.String(), "#EXTM3U") {
		t.Fatalf("hls master: %d %s", hlsRes.Code, hlsRes.Body.String())
	}
	progRes := call("POST", "/api/v2/playback/"+sessionID+"/progress", `{"installation_id":"`+capDoc.InstallationID+`","sequence":1,"position":5,"is_paused":false}`)
	if progRes.Code != http.StatusOK || reported.Sequence != 1 {
		t.Fatalf("live progress: %d %s", progRes.Code, progRes.Body.String())
	}
	stopRes := call("DELETE", "/api/v2/playback/"+sessionID, `{"installation_id":"`+capDoc.InstallationID+`","stop_id":"stop-live-1"}`)
	if stopRes.Code != http.StatusOK || stopped != sessionID {
		t.Fatalf("live stop: %d %s", stopRes.Code, stopRes.Body.String())
	}

	// 10. Restricted/unknown channel returns v2 problem+json 404 (never Go's raw "404 page not found\n").
	for _, path := range []string{
		"/api/v2/catalog/items/" + otherChannelID,
		"/api/v2/watch/" + otherChannelID,
		"/api/v2/recommendations/similar/" + otherChannelID,
	} {
		miss := call("GET", path, "")
		if miss.Code != http.StatusNotFound || miss.Header().Get("Content-Type") != "application/problem+json" || strings.TrimSpace(miss.Body.String()) == "404 page not found" {
			t.Fatalf("expected problem+json 404 for %s, got %d (%s): %s", path, miss.Code, miss.Header().Get("Content-Type"), miss.Body.String())
		}
	}
}

func TestUnknownPQStaysHDR10WithoutClaimingHDR10Plus(t *testing.T) {
	stream := frontend.StreamInfo{Type: "video", ColorTransfer: "smpte2084"}
	if got := siloDynamicRange(stream); got != "hdr10" {
		t.Fatalf("unknown PQ classified as %q", got)
	}
	positive := true
	stream.HDR10Plus = &positive
	if got := siloDynamicRange(stream); got != "hdr10_plus" {
		t.Fatalf("known HDR10+ classified as %q", got)
	}
}

type testSubtitleSessions struct {
	testScopedMedia
	hasAuthored    func(context.Context, frontend.SessionAccess, string, int) bool
	serveASS       func(context.Context, frontend.SessionAccess, string, int, int, bool) (frontend.SubtitleASSArtifact, error)
	fontBundle     func(context.Context, frontend.SessionAccess, string, int) ([]frontend.SubtitleFontBundleItem, error)
	fontAttachment func(context.Context, frontend.SessionAccess, string, int, int) (frontend.SubtitleFontAttachment, error)
}

func (s testSubtitleSessions) HasAuthoredASSSubtitle(ctx context.Context, a frontend.SessionAccess, id string, track int) bool {
	if s.hasAuthored != nil {
		return s.hasAuthored(ctx, a, id, track)
	}
	return false
}

func (s testSubtitleSessions) ServeASSSubtitle(ctx context.Context, a frontend.SessionAccess, id string, track, syncMS int, requireAuthored bool) (frontend.SubtitleASSArtifact, error) {
	if s.serveASS != nil {
		return s.serveASS(ctx, a, id, track, syncMS, requireAuthored)
	}
	return frontend.SubtitleASSArtifact{}, frontend.ErrNotAvailable
}

func (s testSubtitleSessions) SubtitleFontBundle(ctx context.Context, a frontend.SessionAccess, id string, track int) ([]frontend.SubtitleFontBundleItem, error) {
	if s.fontBundle != nil {
		return s.fontBundle(ctx, a, id, track)
	}
	return nil, frontend.ErrNotAvailable
}

func (s testSubtitleSessions) SubtitleFontAttachment(ctx context.Context, a frontend.SessionAccess, id string, track, fontIndex int) (frontend.SubtitleFontAttachment, error) {
	if s.fontAttachment != nil {
		return s.fontAttachment(ctx, a, id, track, fontIndex)
	}
	return frontend.SubtitleFontAttachment{}, frontend.ErrNotAvailable
}

func TestPlaybackASSSidecarAndFontBundle(t *testing.T) {
	viewer := &frontend.Viewer{ID: 7}
	sessionID := "8c2d329e-71d2-4ea1-9b57-95187baeb0aa"
	assPayload := "[Script Info]\nTitle: Styled Subtitles\n[V4+ Styles]\nFormat: Name, Fontname, Fontsize\nStyle: Default,CustomFont,48\n[Events]\nFormat: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\nDialogue: 0,0:00:01.00,0:00:04.00,Default,,0,0,0,,Hello {\\fnHeaderFont}World\n"
	fontPayload := []byte("TTF-FONT-PAYLOAD-0123456789")
	authoredAvailable := true

	mediaInfo := frontend.MediaInfo{
		Container: "mkv",
		Duration:  time.Hour,
		Streams: []frontend.StreamInfo{
			{Type: "video", Index: 0, Codec: "h264"},
			{Type: "audio", Index: 1, Codec: "aac", Language: "jpn"},
			{Type: "subtitle", Index: 5, Codec: "ass", Language: "eng"},
		},
	}

	session := frontendtest.Sessions{
		PlanFunc: func(_ context.Context, r frontend.PlanRequest) (frontend.PlayPlan, error) {
			return frontend.PlayPlan{
				ID:       "plan-ass",
				Media:    mediaInfo,
				Decision: frontend.Decision{Kind: core.DecisionTranscode, VideoCopy: true, AudioStream: 1, SubtitleStream: 5},
			}, nil
		},
		OpenFunc: func(context.Context, frontend.OpenRequest) (frontend.Session, error) {
			return frontend.Session{ID: sessionID}, nil
		},
		StartStreamFunc: func(_ context.Context, _ frontend.SessionAccess, _ string, _ frontend.StreamRequest) (string, error) {
			return "/sessions/media/" + sessionID + "/scoped/master.m3u8", nil
		},
		SubtitleURLFunc: func(_ context.Context, _ frontend.SessionAccess, id string, track int, format string) (string, error) {
			return "/sessions/media/" + id + "/scoped/subtitles/" + strconv.Itoa(track) + "." + format, nil
		},
		SessionsFunc: func(_ context.Context, _ frontend.SessionAccess) ([]frontend.Session, error) {
			return []frontend.Session{{ID: sessionID, ViewerID: 7, Protocol: "silo", Device: frontend.Device{ID: "tv"}, Media: &mediaInfo}}, nil
		},
		ServeMediaFunc: func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		},
	}
	subSessions := testSubtitleSessions{
		testScopedMedia: testScopedMedia{session},
		hasAuthored: func(_ context.Context, a frontend.SessionAccess, id string, track int) bool {
			t.Fatal("playback startup must not scan subtitle artifacts")
			return false
		},
		serveASS: func(_ context.Context, a frontend.SessionAccess, id string, track, _ int, requireAuthored bool) (frontend.SubtitleASSArtifact, error) {
			if a.Viewer == nil || a.Viewer.ID != 7 || a.Device.ID != "tv" {
				return frontend.SubtitleASSArtifact{}, frontend.ErrUnauthorized
			}
			if id != sessionID || track != 5 {
				return frontend.SubtitleASSArtifact{}, frontend.ErrNotFound
			}
			if !authoredAvailable {
				if requireAuthored {
					return frontend.SubtitleASSArtifact{}, frontend.ErrSubtitleAuthoringUnavailable
				}
				return frontend.SubtitleASSArtifact{Data: []byte("[Script Info]\nTitle: Converted\n"), Authored: false}, nil
			}
			return frontend.SubtitleASSArtifact{Data: []byte(assPayload), Authored: true}, nil
		},
		fontBundle: func(_ context.Context, a frontend.SessionAccess, id string, track int) ([]frontend.SubtitleFontBundleItem, error) {
			if a.Viewer == nil || a.Viewer.ID != 7 || a.Device.ID != "tv" {
				return nil, frontend.ErrUnauthorized
			}
			if id != sessionID || track != 5 {
				return nil, frontend.ErrNotFound
			}
			return []frontend.SubtitleFontBundleItem{{Name: "CustomFont.ttf", Data: base64.StdEncoding.EncodeToString(fontPayload)}}, nil
		},
		fontAttachment: func(_ context.Context, a frontend.SessionAccess, id string, track, fontIndex int) (frontend.SubtitleFontAttachment, error) {
			if a.Viewer == nil || a.Viewer.ID != 7 || a.Device.ID != "tv" {
				return frontend.SubtitleFontAttachment{}, frontend.ErrUnauthorized
			}
			if id != sessionID || track != 5 || fontIndex != 0 {
				return frontend.SubtitleFontAttachment{}, frontend.ErrNotFound
			}
			return frontend.SubtitleFontAttachment{Index: 0, Name: "CustomFont.ttf", MIMEType: "font/ttf", Data: append([]byte(nil), fontPayload...)}, nil
		},
	}

	ident := frontendtest.Identity{ResolveFunc: func(_ context.Context, s string) (*frontend.Viewer, error) {
		if s != "account-secret" {
			return nil, errors.New("bad")
		}
		return viewer, nil
	}}
	experience := frontendtest.Experience{ItemFunc: func(context.Context, frontend.ItemRequest) (frontend.ItemDetail, error) {
		return frontend.ItemDetail{Versions: []frontend.Version{{ID: "cut", Media: mediaInfo}}}, nil
	}}
	inst, err := (Frontend{MachineID: "test"}).Mount(context.Background(), frontend.Host{
		Identity:       testDeviceIdentity(ident, viewer),
		Experience:     experience,
		Sessions:       subSessions,
		Capabilities:   frontendtest.Capabilities{},
		VersionHandles: testVersionHandles{},
		SourceOffers:   testSourceOffers{},
		IDs:            ids.Codecs{Allocator: fileIDs{}},
	})
	if err != nil {
		t.Fatal(err)
	}

	call := func(method, path, body, token, device string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Silo-Device-Id", device)
		w := httptest.NewRecorder()
		inst.Handler().ServeHTTP(w, req)
		return w
	}

	startBody := `{"protocol_version":3,"file_id":"101","profile_id":"7","installation_id":"` + inst.(*instance).installationID() + `","playback_attempt_id":"attempt-ass","subtitle_track_index":5,"client_capabilities":{"codecs_video":["h264"]},"client_playback_context":{"deliveries":{"hls":{"enabled":true,"supported_on_device":true,"subtitles":{"ass_styling":true,"font_attachments":true,"sidecar_text":true}}}}}`
	startRes := call("POST", "/api/v2/playback/start", startBody, "account-secret", "tv")
	if startRes.Code != http.StatusCreated {
		t.Fatalf("playback start: %d %s", startRes.Code, startRes.Body.String())
	}
	doc := spectest.Document(t, "silo")
	if doc != nil {
		if err := doc.Validate("POST", "/api/v2/playback/start", startRes.Code, startRes.Header().Get("Content-Type"), startRes.Body.Bytes()); err != nil {
			t.Fatalf("POST /api/v2/playback/start spec validation: %v\nbody: %s", err, startRes.Body.String())
		}
	}
	if !strings.Contains(startRes.Body.String(), `"font_bundle_url":"/api/v2/stream/`+sessionID+`/subtitles/5/fonts"`) {
		t.Fatalf("expected font_bundle_url in subtitle inventory: %s", startRes.Body.String())
	}
	if !strings.Contains(startRes.Body.String(), `"url":"/api/v2/stream/`+sessionID+`/subtitles/5.ass"`) || !strings.Contains(startRes.Body.String(), `"ass_styling_preserved":false`) {
		t.Fatalf("expected deferred .ass artifact without an unverified fidelity claim: %s", startRes.Body.String())
	}

	// Serve native .ass sidecar.
	assRes := call("GET", "/api/v2/stream/"+sessionID+"/subtitles/5.ass", "", "account-secret", "tv")
	if assRes.Code != http.StatusOK || !strings.Contains(assRes.Header().Get("Content-Type"), "text/x-ssa") || !strings.Contains(assRes.Body.String(), "[V4+ Styles]") {
		t.Fatalf("GET .ass sidecar failed: %d (%s) %s", assRes.Code, assRes.Header().Get("Content-Type"), assRes.Body.String())
	}

	// Serve MKV font bundle JSON.
	fontsRes := call("GET", "/api/v2/stream/"+sessionID+"/subtitles/5/fonts", "", "account-secret", "tv")
	if fontsRes.Code != http.StatusOK {
		t.Fatalf("GET /fonts failed: %d %s", fontsRes.Code, fontsRes.Body.String())
	}
	if doc != nil {
		if err := doc.Validate("GET", "/api/v2/stream/"+sessionID+"/subtitles/5/fonts", fontsRes.Code, fontsRes.Header().Get("Content-Type"), fontsRes.Body.Bytes()); err != nil {
			t.Fatalf("GET /fonts spec validation: %v\nbody: %s", err, fontsRes.Body.String())
		}
	}
	var bundle struct {
		Items []struct {
			Name string `json:"name"`
			Data string `json:"data"`
		} `json:"items"`
	}
	if err := json.Unmarshal(fontsRes.Body.Bytes(), &bundle); err != nil {
		t.Fatal(err)
	}
	if len(bundle.Items) != 1 || bundle.Items[0].Name != "CustomFont.ttf" || bundle.Items[0].Data != base64.StdEncoding.EncodeToString(fontPayload) {
		t.Fatalf("unexpected font bundle items: %+v", bundle.Items)
	}

	// Serve individual binary font attachment by index.
	fontFileRes := call("GET", "/api/v2/stream/"+sessionID+"/subtitles/5/fonts/0", "", "account-secret", "tv")
	if fontFileRes.Code != http.StatusOK || fontFileRes.Body.String() != string(fontPayload) {
		t.Fatalf("GET /fonts/0 failed: %d %s", fontFileRes.Code, fontFileRes.Body.String())
	}

	// Startup advertises a deferred artifact, never scans media. The artifact
	// request enforces authored fidelity and fails instead of silently converting.
	authoredAvailable = false
	if strictASS := call("GET", "/api/v2/stream/"+sessionID+"/subtitles/5.ass?require_authored_fidelity=1", "", "account-secret", "tv"); strictASS.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 503 when require_authored_fidelity=1 and authored ASS unavailable, got %d %s", strictASS.Code, strictASS.Body.String())
	}
	preserveBody := `{"protocol_version":3,"file_id":"101","profile_id":"7","installation_id":"` + inst.(*instance).installationID() + `","playback_attempt_id":"attempt-preserve","subtitle_track_index":5,"subtitle_fidelity_preference":"preserve","client_capabilities":{"codecs_video":["h264"]},"client_playback_context":{"deliveries":{"hls":{"enabled":true,"supported_on_device":true,"subtitles":{"ass_styling":true,"font_attachments":true,"sidecar_text":true}}}}}`
	if preserveRes := call("POST", "/api/v2/playback/start", preserveBody, "account-secret", "tv"); preserveRes.Code != http.StatusCreated || strings.Contains(preserveRes.Body.String(), `"ass_styling_preserved":true`) {
		t.Fatalf("expected deferred artifact with no unverified fidelity claim, got %d %s", preserveRes.Code, preserveRes.Body.String())
	}
	compatRes := call("POST", "/api/v2/playback/start", startBody, "account-secret", "tv")
	if compatRes.Code != http.StatusCreated || strings.Contains(compatRes.Body.String(), `"ass_styling_preserved":true`) {
		t.Fatalf("expected ass_styling_preserved=false when authored ASS unavailable, got %d %s", compatRes.Code, compatRes.Body.String())
	}
	// The app sends "preserve" on every start. A plain-text track chosen by the
	// viewer's rules converts to WebVTT without loss and must never refuse the
	// start ("Selected track unavailable" stopped every Silo start, W182).
	mediaInfo.Streams[2].Codec = "subrip"
	if textRes := call("POST", "/api/v2/playback/start", preserveBody, "account-secret", "tv"); textRes.Code != http.StatusCreated || !strings.Contains(textRes.Body.String(), `"mode":"convert"`) || !strings.Contains(textRes.Body.String(), `"subtitle_fidelity_policy":"require_authored_fidelity"`) {
		t.Fatalf("preserve with a text subtitle: %d %s", textRes.Code, textRes.Body.String())
	}
	mediaInfo.Streams[2].Codec = "ass"
	authoredAvailable = true

	// Unauthorized device or token must be rejected on both .ass and /fonts.
	if unauth := call("GET", "/api/v2/stream/"+sessionID+"/subtitles/5/fonts", "", "account-secret", "wrong-device"); unauth.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 on wrong device for /fonts, got %d", unauth.Code)
	}
	if unauth := call("GET", "/api/v2/stream/"+sessionID+"/subtitles/5.ass", "", "wrong-token", "tv"); unauth.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 on wrong token for .ass, got %d", unauth.Code)
	}
	// Non-ASS subtitle track index must 404.
	if missing := call("GET", "/api/v2/stream/"+sessionID+"/subtitles/99/fonts", "", "account-secret", "tv"); missing.Code != http.StatusNotFound {
		t.Fatalf("expected 404 on unknown track for /fonts, got %d", missing.Code)
	}
}

// Embedded text subtitles must be on screen with the picture, never behind a
// whole-file extraction: original-file playback names the stream for the
// player's own demux, and a server HLS stream carries it as a WebVTT rendition.
func TestEmbeddedSubtitleNeedsNoWholeFileArtifact(t *testing.T) {
	viewer := &frontend.Viewer{ID: 7}
	sessionID := "4f7c2a9e-2b51-4a43-8d0e-5c3b5d8e9a10"
	media := frontend.MediaInfo{Container: "mkv", Duration: time.Hour, Streams: []frontend.StreamInfo{
		{Type: "video", Index: 0, Codec: "h264"},
		{Type: "audio", Index: 1, Codec: "aac", Language: "eng"},
		{Type: "subtitle", Index: 4, ContainerTrackID: 5, Codec: "subrip", Language: "eng"},
	}}
	kind := core.DecisionDirectPlay
	session := frontendtest.Sessions{
		PlanFunc: func(context.Context, frontend.PlanRequest) (frontend.PlayPlan, error) {
			return frontend.PlayPlan{ID: "plan-sub", Media: media, Decision: frontend.Decision{Kind: kind, VideoCopy: true, AudioCopy: true, AudioStream: 1, SubtitleStream: 4}}, nil
		},
		OpenFunc: func(context.Context, frontend.OpenRequest) (frontend.Session, error) {
			return frontend.Session{ID: sessionID}, nil
		},
		StartStreamFunc: func(_ context.Context, _ frontend.SessionAccess, _ string, req frontend.StreamRequest) (string, error) {
			if req.Format == frontend.StreamHLS {
				return "/sessions/media/" + sessionID + "/scoped/master.m3u8", nil
			}
			return "/sessions/media/" + sessionID + "/scoped/file", nil
		},
		SubtitleURLFunc: func(_ context.Context, _ frontend.SessionAccess, id string, track int, format string) (string, error) {
			if format == "hls" {
				if kind == core.DecisionDirectPlay {
					return "", frontend.ErrNotFound
				}
				return "/sessions/media/" + id + "/scoped/text-" + strconv.Itoa(track) + "/index.m3u8", nil
			}
			return "/sessions/media/" + id + "/scoped/subtitles/" + strconv.Itoa(track) + "." + format, nil
		},
	}
	ident := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
	inst, err := (Frontend{MachineID: "test"}).Mount(context.Background(), frontend.Host{
		Identity: testDeviceIdentity(ident, viewer), Sessions: testScopedMedia{session}, Capabilities: frontendtest.Capabilities{},
		VersionHandles: testVersionHandles{}, SourceOffers: testSourceOffers{}, IDs: ids.Codecs{Allocator: fileIDs{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	start := func() map[string]any {
		body := `{"protocol_version":3,"file_id":"101","profile_id":"7","installation_id":"` + inst.(*instance).installationID() + `","playback_attempt_id":"attempt-sub","subtitle_fidelity_preference":"preserve","client_features":["playback_plan_v3","embedded_subtitles_v1","hls_subtitle_renditions_v1","hls_remux_subtitle_renditions_v1"],"client_capabilities":{"codecs_video":["h264"]},"client_playback_context":{"deliveries":{"original_http":{"enabled":true,"supported_on_device":true,"subtitles":{"ass_styling":true,"native_embedded":[{"container":"mkv","codecs":["subrip","ass","hdmv_pgs_subtitle"],"track_identity":"ffmpeg_stream_index","ass_styling":true,"font_attachments":true}]}},"hls":{"enabled":true,"supported_on_device":true}}}}`
		req := httptest.NewRequest("POST", "/api/v2/playback/start", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer account-secret")
		req.Header.Set("X-Silo-Device-Id", "tv")
		w := httptest.NewRecorder()
		inst.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusCreated {
			t.Fatalf("start: %d %s", w.Code, w.Body.String())
		}
		if doc := spectest.Document(t, "silo"); doc != nil {
			if err := doc.Validate("POST", "/api/v2/playback/start", w.Code, w.Header().Get("Content-Type"), w.Body.Bytes()); err != nil {
				t.Fatalf("spec: %v\n%s", err, w.Body.String())
			}
		}
		var out struct {
			Plan struct {
				Subtitle map[string]any `json:"subtitle"`
			} `json:"playback_plan"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out.Plan.Subtitle
	}
	direct := start()
	embedded, _ := direct["embedded"].(map[string]any)
	if direct["mode"] != "render" || direct["artifact"] != nil || embedded == nil || embedded["stream_index"] != float64(4) || embedded["container_track_id"] != "5" {
		t.Fatalf("direct play subtitle: %+v", direct)
	}
	if row := direct["inventory"].([]any)[0].(map[string]any); !strings.Contains(row["url"].(string), "embedded_stream_index=4") {
		t.Fatalf("inventory row does not name its stream: %+v", row)
	}
	kind = core.DecisionTranscode
	hls := start()
	rendition, _ := hls["hls_rendition"].(map[string]any)
	if hls["mode"] != "convert" || hls["artifact"] != nil || hls["embedded"] != nil || rendition == nil || rendition["group_id"] != "subs" || rendition["name"] != "eng (4)" {
		t.Fatalf("HLS subtitle: %+v", hls)
	}
}

// Silo Android tablets decode TrueHD/DTS in software under original_http
// without an AV receiver (audio_passthrough_codecs is empty), and Android TV
// declares Dolby Vision support in hdr_details. Both must reach capability
// building, and seek_reanchor replans with empty attempted_plan_keys must
// re-anchor the playing HLS stream instead of returning 400.
func TestSiloOriginalAudioDecodeHDRDetailsAndSeekReanchor(t *testing.T) {
	viewer := &frontend.Viewer{ID: 7}
	sessionID := "8d1f0a6b-9c34-4e11-a217-5e9a3d0f2b12"
	var gotDevice caps.DeviceInfo
	var startedPositions []time.Duration
	session := frontendtest.Sessions{
		PlanFunc: func(_ context.Context, req frontend.PlanRequest) (frontend.PlayPlan, error) {
			return frontend.PlayPlan{
				ID: "plan-dv",
				Media: frontend.MediaInfo{
					Container: "mkv", Duration: time.Hour,
					Streams: []frontend.StreamInfo{
						{Type: "video", Index: 0, Codec: "hevc", Width: 3840, Height: 2160},
						{Type: "audio", Index: 1, Codec: "truehd", Channels: 8, Default: true},
					},
				},
				Decision: frontend.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioCodec: "eac3", AudioStream: 1},
			}, nil
		},
		OpenFunc: func(context.Context, frontend.OpenRequest) (frontend.Session, error) {
			return frontend.Session{ID: sessionID}, nil
		},
		SessionsFunc: func(context.Context, frontend.SessionAccess) ([]frontend.Session, error) {
			return []frontend.Session{{ID: sessionID, ViewerID: viewer.ID, Protocol: "silo", Device: frontend.Device{ID: "tv"}}}, nil
		},
		StartStreamFunc: func(_ context.Context, _ frontend.SessionAccess, _ string, req frontend.StreamRequest) (string, error) {
			startedPositions = append(startedPositions, req.Position)
			return "/sessions/media/" + sessionID + "/scoped/master.m3u8", nil
		},
	}
	capBuilder := frontendtest.Capabilities{BuildFunc: func(_ context.Context, req frontend.CapabilityRequest) (frontend.CapabilityResult, error) {
		gotDevice = req.Device
		return frontend.CapabilityResult{Client: core.ClientCaps{
			VideoCodecs: req.Device.VideoCodecs, AudioCodecs: req.Device.AudioCodecs,
			Containers: req.Device.Containers, HDR10: req.Device.HDR10,
			DolbyVision: req.Device.DolbyVision, DVProfiles: req.Device.DVProfiles,
		}}, nil
	}}
	ident := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
	inst, err := (Frontend{MachineID: "test"}).Mount(context.Background(), frontend.Host{
		Identity: testDeviceIdentity(ident, viewer), Sessions: testScopedMedia{session}, Capabilities: capBuilder,
		VersionHandles: testVersionHandles{}, SourceOffers: testSourceOffers{}, IDs: ids.Codecs{Allocator: fileIDs{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	installID := inst.(*instance).installationID()
	startBody := `{"protocol_version":3,"file_id":"101","profile_id":"7","installation_id":"` + installID + `","playback_attempt_id":"attempt-1","quality_preference":"auto","client_features":["playback_plan_v3","seek_reanchor_v1"],"client_capabilities":{"codecs_video":["h264","hevc"],"codecs_audio":["aac","eac3"],"containers":["mp4"],"hdr":false,"hdr_details":{"hdr10":true,"dolby_vision_profiles":[5,8]},"audio_passthrough":{"passthrough_codecs":["dtshd"]}},"client_playback_context":{"deliveries":{"original_http":{"enabled":true,"supported_on_device":true,"containers":["mkv"],"video_codecs":["av1"],"audio_decode_codecs":["truehd","mlp","dts"],"audio_passthrough_codecs":[]},"hls":{"enabled":true,"supported_on_device":true}}}}`
	req := httptest.NewRequest("POST", "/api/v2/playback/start", strings.NewReader(startBody))
	req.Header.Set("Authorization", "Bearer account-secret")
	req.Header.Set("X-Silo-Device-Id", "tv")
	w := httptest.NewRecorder()
	inst.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("start: %d %s", w.Code, w.Body.String())
	}
	for _, want := range []string{"truehd", "mlp", "dts", "dtshd"} {
		if !slices.Contains(gotDevice.AudioCodecs, want) {
			t.Fatalf("missing audio codec %q in %v", want, gotDevice.AudioCodecs)
		}
	}
	if !gotDevice.HDR10 || !gotDevice.DolbyVision || !slices.Equal(gotDevice.DVProfiles, []int{5, 8}) || !slices.Contains(gotDevice.Containers, "mkv") || !slices.Contains(gotDevice.VideoCodecs, "av1") {
		t.Fatalf("unexpected device caps: %+v", gotDevice)
	}
	var started struct {
		Features []string `json:"server_features"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	for _, feat := range []string{"seek_reanchor_v1", "direct_stream_resume_v1", "embedded_subtitles_v1", "plan_active_quality_v1"} {
		if !slices.Contains(started.Features, feat) {
			t.Fatalf("server_features missing %q: %v", feat, started.Features)
		}
	}

	reanchorBody := `{"installation_id":"` + installID + `","protocol_version":3,"operation":"seek_reanchor","playback_attempt_id":"attempt-1","replan_request_id":"seek-1","failed_plan_id":"plan-dv","plan_attempt_id":"planattempt-1","plan_attempt_key":"plan-dv","attempted_plan_keys":[],"attempt_count":1,"quality_preference":"","position_seconds":600,"selected_tracks":{},"client_capabilities":{},"client_playback_context":{}}`
	rreq := httptest.NewRequest("POST", "/api/v2/playback/"+sessionID+"/replan", strings.NewReader(reanchorBody))
	rreq.Header.Set("Authorization", "Bearer account-secret")
	rreq.Header.Set("X-Silo-Device-Id", "tv")
	rw := httptest.NewRecorder()
	inst.Handler().ServeHTTP(rw, rreq)
	if rw.Code != http.StatusOK {
		t.Fatalf("seek_reanchor replan = %d %s", rw.Code, rw.Body.String())
	}
	if len(startedPositions) != 2 || startedPositions[1] != 600*time.Second {
		t.Fatalf("seek_reanchor StartStream positions = %v, want [0s, 10m0s]", startedPositions)
	}
	var replanned struct {
		Outcome string `json:"outcome"`
		Plan    struct {
			Timeline struct {
				SourceStart float64 `json:"source_start_seconds"`
				Origin      float64 `json:"stream_origin_seconds"`
				Offset      float64 `json:"timeline_offset_seconds"`
			} `json:"timeline"`
		} `json:"playback_plan"`
	}
	if err := json.Unmarshal(rw.Body.Bytes(), &replanned); err != nil {
		t.Fatal(err)
	}
	if replanned.Outcome != "playable" || replanned.Plan.Timeline.SourceStart != 600 || replanned.Plan.Timeline.Origin != 600 || replanned.Plan.Timeline.Offset != 600 {
		t.Fatalf("unexpected seek_reanchor response: %+v", replanned)
	}
}
