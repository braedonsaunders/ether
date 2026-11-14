package vela

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
)

type outputFixture struct {
	core.HLSSession
	rungs []core.HLSRung
}

func (f *outputFixture) Rungs() []core.HLSRung { return f.rungs }
func (f *outputFixture) PlaybackDiagnostics() core.HLSPlaybackDiagnostics {
	return core.HLSPlaybackDiagnostics{}
}

type sessionFake = frontendtest.Sessions

type trackFixture struct{ sessionFake }

func (*trackFixture) StreamDecision(context.Context, frontend.SessionAccess, string) (core.Decision, error) {
	return core.Decision{Kind: core.DecisionRemux, AudioCopy: true, AudioStream: 7}, nil
}

func TestVelaPlaybackOutputInvariants(t *testing.T) {
	t.Run("codec evidence and generations", func(t *testing.T) {
		output := &outputFixture{rungs: []core.HLSRung{{ID: "base", Width: 1920, Height: 1080, Bandwidth: 8000000, Codecs: "estimated-should-not-escape"}}}
		sessions := &frontendtest.Sessions{TranscodeForFunc: func(context.Context, string) (core.HLSSession, bool) { return output, true }}
		i := &instance{host: frontend.Host{Sessions: sessions}}
		s := &playSession{mse: true, source: 3, seek: 4, plan: object{"paths": []object{{"transport": "hls", "url": "/sessions/media/id/scoped-secret/master.m3u8", "qualities": []object{}}}}}
		i.refreshOutput(context.Background(), "id", s)
		if s.plan["paths"].([]object)[0]["initialRenditionId"] != "base" {
			t.Fatal("initial recommendation is not a real advertised rung")
		}
		q := s.plan["paths"].([]object)[0]["qualities"].([]object)[0]
		if q["videoCodec"] != "" || q["codecStatus"] != "pending" || q["url"] != "/sessions/media/id/scoped-secret/base/index.m3u8" {
			t.Fatal(q)
		}
		output.rungs[0].OutputCodecs = "avc1.640029,mp4a.40.2"
		if !i.refreshOutput(context.Background(), "id", s) {
			t.Fatal("produced init did not update plan")
		}
		q = s.plan["paths"].([]object)[0]["qualities"].([]object)[0]
		if q["videoCodec"] != "avc1.640029" || q["audioCodec"] != "mp4a.40.2" || q["codecStatus"] != "verified" {
			t.Fatal(q)
		}
		p := s.plan["preparation"].(object)
		if p["sourceGeneration"] != uint64(3) || p["seekGeneration"] != uint64(4) {
			t.Fatal("unscoped output evidence")
		}
	})
	t.Run("track command and stale generation", func(t *testing.T) {
		calls := 0
		sessions := &trackFixture{}
		sessions.StartStreamFunc = func(_ context.Context, a frontend.SessionAccess, id string, r frontend.StreamRequest) (string, error) {
			calls++
			if a.Viewer.ID != 1 || a.Device.ID != "device" || id != "session" || r.Position != 47*time.Second || r.AudioStream == nil || *r.AudioStream != 7 {
				t.Fatal("restart lost authorization, position or track", a, r)
			}
			return "/sessions/media/session/grant/master.m3u8", nil
		}
		s := &playSession{scope: "fake-family", viewer: 1, device: "device", source: 2, seek: 3, position: 47 * time.Second, signal: make(chan struct{}), plan: object{"sessionId": "session", "paths": []object{{"transport": "file", "url": "/file"}}, "audioTracks": []object{{"id": "1"}, {"id": "7"}}, "selectedAudioTrackId": "1"}}
		i := &instance{host: frontend.Host{Identity: identityFake{}, Sessions: sessions}, sessions: map[string]*playSession{"session": s}}
		request := func() *httptest.ResponseRecorder {
			r := httptest.NewRequest("POST", "/vela/v0/sessions/session/commands", nil)
			r.SetPathValue("id", "session")
			r.Header.Set("Authorization", "Bearer token")
			r.Header.Set("X-Vela-Device-Id", "device")
			r = r.WithContext(context.WithValue(r.Context(), bodyKey{}, json.RawMessage(`{"kind":"tracks","audioTrackId":"7","sourceGeneration":2,"seekGeneration":3}`)))
			w := httptest.NewRecorder()
			i.command(w, r)
			return w
		}
		if w := request(); w.Code != 200 {
			t.Fatalf("command %d %s", w.Code, w.Body.String())
		}
		if s.source != 3 || s.seek != 4 || s.position != 47*time.Second || s.plan["selectedAudioTrackId"] != "7" {
			t.Fatal("track change lost generation/position", s.plan)
		}
		if w := request(); w.Code != 409 || calls != 1 {
			t.Fatal("stale track command restarted encoder", w.Code, calls)
		}

		if origin := s.plan["paths"].([]object)[0]["timelineOriginSeconds"]; origin != float64(47) {
			t.Fatal("track restart lost playlist origin", origin)
		}
		sessions.StartStreamFunc = func(_ context.Context, _ frontend.SessionAccess, _ string, r frontend.StreamRequest) (string, error) {
			if r.Position != 1433500*time.Millisecond || r.AudioStream != nil {
				t.Fatal("seek offset was not absolute", r)
			}
			return "/sessions/media/session/grant/master.m3u8", nil
		}
		r := httptest.NewRequest("POST", "/vela/v0/sessions/session/commands", nil)
		r.SetPathValue("id", "session")
		r.Header.Set("Authorization", "Bearer token")
		r.Header.Set("X-Vela-Device-Id", "device")
		r = r.WithContext(context.WithValue(r.Context(), bodyKey{}, json.RawMessage(`{"kind":"seek","positionSeconds":1433.5,"sourceGeneration":3,"seekGeneration":4}`)))
		w := httptest.NewRecorder()
		i.command(w, r)
		if w.Code != 200 || s.plan["paths"].([]object)[0]["timelineOriginSeconds"] != 1433.5 || s.position.Seconds() != 1433.5 {
			t.Fatal("seek origin was doubled/lost", w.Code, s.plan)
		}
	})
	t.Run("sanitized manifests", func(t *testing.T) {
		got := sanitizePlaylist("#EXTM3U\n#EXT-X-MAP:URI=\"https://provider.invalid/source/init.mp4?token=secret\"\n/sessions/media/id/grant/00001.m4s?token=secret\n")
		if strings.Contains(got, "secret") || strings.Contains(got, "grant") || strings.Contains(got, "provider") || !strings.Contains(got, `URI="init.mp4"`) {
			t.Fatal(got)
		}
	})
}
