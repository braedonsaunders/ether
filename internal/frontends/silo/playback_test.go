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

func (fileIDs) Compact(context.Context, string, string, string) (int32, error) { return 1, nil }
func (fileIDs) Resolve(_ context.Context, s, k string, n int32) (string, error) {
	if s != "silo" || k != "file" || n != 1 {
		return "", errors.New("bad")
	}
	return `{"key":1000000000000001,"version":"cut"}`, nil
}
func TestPlaybackUsesScopedBroker(t *testing.T) {
	viewer := &frontend.Viewer{ID: 7}
	var planned frontend.PlanRequest
	var streamed frontend.StreamRequest
	var timeline frontend.Timeline
	session := frontendtest.Sessions{PlanFunc: func(_ context.Context, r frontend.PlanRequest) (frontend.PlayPlan, error) {
		planned = r
		return frontend.PlayPlan{ID: "plan", Media: frontend.MediaInfo{Container: "mp4", Duration: time.Hour}, Decision: frontend.Decision{Kind: "transcode"}}, nil
	}, OpenFunc: func(_ context.Context, r frontend.OpenRequest) (frontend.Session, error) {
		if r.PlanID != "plan" {
			t.Fatal("wrong plan")
		}
		return frontend.Session{ID: "session"}, nil
	}, StartStreamFunc: func(_ context.Context, _ frontend.SessionAccess, _ string, r frontend.StreamRequest) (string, error) {
		streamed = r
		return "/sessions/media/session/scoped/master.m3u8", nil
	}, SessionsFunc: func(_ context.Context, _ frontend.SessionAccess) ([]frontend.Session, error) {
		return []frontend.Session{{ID: "session", ViewerID: 7, Protocol: "silo", Device: frontend.Device{ID: "tv"}}}, nil
	}, ServeMediaFunc: func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sessions/media/session/scoped/master.m3u8" {
			t.Fatal(r.URL.Path)
		}
		w.WriteHeader(200)
	},
		ReportFunc: func(_ context.Context, _ frontend.SessionAccess, _ string, r frontend.Timeline) error {
			timeline = r
			return nil
		}}
	ident := frontendtest.Identity{ResolveFunc: func(_ context.Context, s string) (*frontend.Viewer, error) {
		if s != "account-secret" {
			return nil, errors.New("bad")
		}
		return viewer, nil
	}}
	experience := frontendtest.Experience{ItemFunc: func(context.Context, frontend.ItemRequest) (frontend.ItemDetail, error) {
		return frontend.ItemDetail{Versions: []frontend.Version{{ID: "cut", Media: frontend.MediaInfo{Streams: []frontend.StreamInfo{{Type: "video", Index: 0}, {Type: "audio", Index: 2}, {Type: "subtitle", Index: 5}}}}}}, nil
	}}
	inst, err := (Frontend{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: ident, Experience: experience, Sessions: session, Capabilities: frontendtest.Capabilities{}, IDs: ids.Codecs{Allocator: fileIDs{}}})
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer account-secret")
		req.Header.Set("X-Silo-Device-Id", "tv")
		w := httptest.NewRecorder()
		inst.Handler().ServeHTTP(w, req)
		return w
	}
	w := call("POST", "/api/v2/playback/start", `{"protocol_version":3,"file_id":"1","profile_id":"7","installation_id":"install","playback_attempt_id":"attempt","start_position":12}`)
	if w.Code != 201 || strings.Contains(w.Body.String(), "account-secret") || planned.Version != "cut" || planned.Item != 1000000000000001 || streamed.Format != frontend.StreamHLS {
		t.Fatalf("play %d %s %+v %+v", w.Code, w.Body.String(), planned, streamed)
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
	if !strings.Contains(w.Body.String(), "/api/v2/stream/session/scoped/master.m3u8") {
		t.Fatal("media URL not native", w.Body.String())
	}
	w = call("POST", "/api/v2/playback/start", `{"protocol_version":3,"file_id":"1","profile_id":"7","installation_id":"install","playback_attempt_id":"attempt","audio_track_index":0,"subtitle_track_index":0}`)
	if w.Code != 201 || planned.Intent.Options.AudioStream != 2 || planned.Intent.Options.SubtitleStream != 5 {
		t.Fatalf("track ordinal mapping: %d %+v", w.Code, planned.Intent.Options)
	}
	media := call("GET", "/api/v2/stream/session/scoped/master.m3u8", "")
	if media.Code != 200 {
		t.Fatalf("media %d", media.Code)
	}

	var body map[string]any
	if json.Unmarshal(w.Body.Bytes(), &body) != nil || body["outcome"] != "playable" {
		t.Fatalf("response %s", w.Body.String())
	}
	w = call("POST", "/api/v2/playback/session/progress", `{"installation_id":"install","sequence":1,"position":60}`)
	if w.Code != 200 || timeline.Position != time.Minute || timeline.State != "playing" || timeline.Sequence != 1 {
		t.Fatalf("progress %d %+v", w.Code, timeline)
	}
	if e == nil {
		doc, parseErr := openapi.Parse(spec)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		if err := doc.Validate("POST", "/api/v2/playback/session/progress", w.Code, w.Header().Get("Content-Type"), w.Body.Bytes()); err != nil {
			t.Errorf("progress spec: %v", err)
		}
	}

}

func TestPlaybackCapacityProblem(t *testing.T) {
	w := httptest.NewRecorder()
	playbackError(w, &frontend.TranscodeCapacityError{RetryAfter: 1500 * time.Millisecond})
	if w.Code != 503 || w.Header().Get("Retry-After") != "2" || w.Header().Get("Content-Type") != "application/problem+json" || !strings.Contains(w.Body.String(), "transcode_capacity") {
		t.Fatalf("capacity %d %s", w.Code, w.Body.String())
	}
}

func TestUnknownHDR10PlusOnlySuppressesPQ(t *testing.T) {
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
