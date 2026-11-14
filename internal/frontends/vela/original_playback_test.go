package vela

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

func TestOriginalPlaybackContract(t *testing.T) {
	no := false
	yes := true
	ctx := context.Background()
	starts := []frontend.StreamRequest{}
	broker := frontendtest.Sessions{
		PlanFunc: func(_ context.Context, req frontend.PlanRequest) (frontend.PlayPlan, error) {
			if !req.Intent.OriginalPlayback.Valid() {
				t.Fatal("runtime capability lost")
			}
			return frontend.PlayPlan{ID: "plan", ETag: "pinned-source", HLS: true, Transcoding: true, Decision: core.Decision{Kind: core.DecisionDirectPlay, Reason: "original_runtime", AudioStream: 4, SubtitleStream: -1}, Media: core.MediaInfo{Container: "mkv", Size: 1024, Duration: time.Hour, Streams: []core.StreamInfo{{Type: "video", Index: 0, Codec: "hevc", Width: 3840, Height: 2160, DolbyVisionProfile: 7, DolbyVisionELPresent: &no, DolbyVisionRPUPresent: &yes}, {Type: "audio", Index: 4, Codec: "truehd"}}}}, nil
		},
		OpenFunc: func(context.Context, frontend.OpenRequest) (frontend.Session, error) {
			return frontend.Session{ID: "session"}, nil
		},
		StartStreamFunc: func(_ context.Context, _ frontend.SessionAccess, _ string, req frontend.StreamRequest) (string, error) {
			starts = append(starts, req)
			return "/sessions/media/session/grant/file", nil
		},
	}
	i := &instance{ctx: ctx, host: frontend.Host{Identity: identityFake{}, Sessions: broker, Capabilities: frontendtest.Capabilities{}}, sessions: map[string]*playSession{}}
	call := func(handler http.HandlerFunc, b object) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(b)
		r := httptest.NewRequest("POST", "/vela/v0/test", nil)
		r.SetPathValue("id", "session")
		r.Header.Set("Authorization", "Bearer access")
		r.Header.Set("X-Vela-Device-Id", "device")
		r = r.WithContext(context.WithValue(r.Context(), bodyKey{}, json.RawMessage(raw)))
		w := httptest.NewRecorder()
		handler(w, r)
		return w
	}
	w := call(i.playback, object{"titleId": ids.RatingKeyGUID(core.MovieKey(1)), "positionSeconds": 123.0, "client": object{"device": object{"id": "device"}, "hls": true, "originalPlayback": object{"version": 1, "range": true, "demuxContainers": []string{"mkv"}, "videoDecoders": []string{"hevc"}}}})
	if w.Code != 200 {
		t.Fatalf("playback %d %s", w.Code, w.Body.String())
	}
	plan := i.sessions["session"].plan
	paths := plan["paths"].([]object)
	if len(starts) != 1 || starts[0].Format != frontend.StreamDirect || paths[0]["original"] != true || paths[0]["range"] != true || paths[0]["byteLength"] != int64(1024) || paths[1]["url"] != nil || paths[1]["readiness"] != "on-demand" {
		t.Fatalf("incorrect original/fallback: %+v", plan)
	}
	facts := paths[0]["originalVideo"].(object)
	if facts["dvProfile"] != 7 || facts["elPresent"] != false || facts["rpuPresent"] != true || facts["blPresent"] != nil || paths[0]["mimeType"] != "video/x-matroska" {
		t.Fatalf("invented or lost source evidence %+v", facts)
	}
	tracks := plan["audioTracks"].([]object)
	if tracks[0]["streamIndex"] != 4 {
		t.Fatal("track index lost")
	}
	w = call(i.command, object{"kind": "switch", "pathId": "fallback-hls", "atSeconds": 456.0, "sourceGeneration": 1, "seekGeneration": 1})
	if w.Code != 200 || len(starts) != 2 || starts[1].Position != 456*time.Second || starts[1].Format != frontend.StreamHLS {
		t.Fatalf("fallback switch %d %s %+v", w.Code, w.Body.String(), starts)
	}
	if paths[1]["timelineOriginSeconds"] != 456.0 || plan["sourceGeneration"] != uint64(1) || plan["seekGeneration"] != uint64(2) || paths[0]["url"] != nil {
		t.Fatalf("timeline/source changed %+v", plan)
	}
}

func TestOriginalSecureOriginAssets(t *testing.T) {
	dir := t.TempDir()
	for path, content := range map[string]string{"index.html": "<html>Vela</html>", "sw.js": "self.addEventListener('fetch',()=>{})"} {
		if err := os.WriteFile(filepath.Join(dir, path), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	origin := "https://192-168-68-65.server.plex.direct:32550"
	i := &instance{ctx: context.Background(), assets: dir, host: frontend.Host{SecureOrigins: func(context.Context, *http.Request) []string { return []string{origin} }}}
	for _, path := range []string{"/vela", "/vela/watch/movie", "/vela/sw.js"} {
		r := httptest.NewRequest("GET", origin+path, nil)
		w := httptest.NewRecorder()
		i.serve(w, r)
		if w.Code != 200 || w.Header().Get("Set-Cookie") != "" {
			t.Fatalf("secure asset %s %d", path, w.Code)
		}
		if path == "/vela/sw.js" && w.Header().Get("Service-Worker-Allowed") != "/vela" {
			t.Fatal("worker scope lost")
		}
	}
	r := httptest.NewRequest("GET", origin+"/vela/v0/capabilities", nil)
	origins := i.requestCapabilities(r)["secureOrigins"].([]string)
	if len(origins) != 1 || origins[0] != origin || i.trustedSecureOrigin(r, "https://attacker.invalid") {
		t.Fatal("origin trust lost")
	}
}

type originalScopeIdentity struct {
	identityFake
	active bool
}

func (s *originalScopeIdentity) GrantScopeCurrent(context.Context, string, frontend.Device) (*frontend.Viewer, error) {
	if !s.active {
		return nil, frontend.ErrUnauthorized
	}
	return &frontend.Viewer{ID: 1}, nil
}
func TestOriginalMediaGrantLineage(t *testing.T) {
	identity := &originalScopeIdentity{active: true}
	served := 0
	i := &instance{host: frontend.Host{Identity: identity, Sessions: frontendtest.Sessions{ServeMediaFunc: func(http.ResponseWriter, *http.Request) { served++ }}}, sessions: map[string]*playSession{"play": {scope: "family", device: "device", viewer: 1}}}
	request := httptest.NewRequest("GET", "/sessions/media/play/grant/file", nil)
	i.serveMedia(httptest.NewRecorder(), request)
	identity.active = false
	out := httptest.NewRecorder()
	i.serveMedia(out, request)
	if served != 1 || out.Code != 404 {
		t.Fatal("revoked family delivered media", served, out.Code)
	}
}
