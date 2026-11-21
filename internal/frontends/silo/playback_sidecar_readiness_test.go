package silo

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

// readinessSessions adds the optional subtitle sidecar readiness predicate to
// the scoped-media test host.
type readinessSessions struct {
	testScopedMedia
	ready bool
}

func (s readinessSessions) SubtitleSidecarReady(context.Context, frontend.SessionAccess, string, int) bool {
	return s.ready
}

// A client fetches the selected subtitle before it starts media, so an embedded
// track that has to be read out of the whole source stalls startup on a
// full-length release. When the host reports that the sidecar cannot be produced
// cheaply the plan must start playback without it rather than advertise an
// artifact the client will block on. A host that cannot answer keeps the
// previous behaviour.
func TestPlaybackSidecarReadinessGatesSelectedEmbeddedSubtitle(t *testing.T) {
	const sessionID = "5f1c8a2e-4b7d-4c31-9a6e-2d0f7b8c1a44"
	viewer := &frontend.Viewer{ID: 7}
	mediaInfo := frontend.MediaInfo{
		Container: "mkv",
		Duration:  time.Hour,
		Streams: []frontend.StreamInfo{
			{Type: "video", Index: 0, Codec: "h264"},
			{Type: "audio", Index: 1, Codec: "aac", Language: "eng"},
			{Type: "subtitle", Index: 5, Codec: "subrip", Language: "eng"},
		},
	}
	sessions := frontendtest.Sessions{
		PlanFunc: func(context.Context, frontend.PlanRequest) (frontend.PlayPlan, error) {
			return frontend.PlayPlan{
				ID:       "plan-sidecar",
				Media:    mediaInfo,
				Decision: frontend.Decision{Kind: core.DecisionTranscode, VideoCopy: true, AudioStream: 1, SubtitleStream: 5},
			}, nil
		},
		OpenFunc: func(context.Context, frontend.OpenRequest) (frontend.Session, error) {
			return frontend.Session{ID: sessionID}, nil
		},
		StartStreamFunc: func(context.Context, frontend.SessionAccess, string, frontend.StreamRequest) (string, error) {
			return "/sessions/media/" + sessionID + "/scoped/master.m3u8", nil
		},
		SubtitleURLFunc: func(_ context.Context, _ frontend.SessionAccess, id string, track int, format string) (string, error) {
			return "/sessions/media/" + id + "/scoped/subtitles/" + strconv.Itoa(track) + "." + format, nil
		},
		SessionsFunc: func(context.Context, frontend.SessionAccess) ([]frontend.Session, error) {
			return []frontend.Session{{ID: sessionID, ViewerID: 7, Protocol: "silo", Device: frontend.Device{ID: "tv"}, Media: &mediaInfo}}, nil
		},
		ServeMediaFunc: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) },
	}
	ident := frontendtest.Identity{ResolveFunc: func(_ context.Context, s string) (*frontend.Viewer, error) {
		if s != "account-secret" {
			return nil, core.ErrUnauthorized
		}
		return viewer, nil
	}}
	experience := frontendtest.Experience{ItemFunc: func(context.Context, frontend.ItemRequest) (frontend.ItemDetail, error) {
		return frontend.ItemDetail{Versions: []frontend.Version{{ID: "cut", Media: mediaInfo}}}, nil
	}}

	// readiness nil mounts a host that does not implement the predicate.
	for _, tc := range []struct {
		name          string
		readiness     *bool
		wantMode      string
		wantArtifact  bool
		wantSubSelect bool
	}{
		{name: "ready", readiness: boolPtr(true), wantMode: "convert", wantArtifact: true, wantSubSelect: true},
		{name: "not_ready", readiness: boolPtr(false), wantMode: "off", wantArtifact: false, wantSubSelect: false},
		{name: "unimplemented", readiness: nil, wantMode: "convert", wantArtifact: true, wantSubSelect: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scoped := testScopedMedia{sessions}
			var hostSessions frontend.Sessions = scoped
			if tc.readiness != nil {
				hostSessions = readinessSessions{testScopedMedia: scoped, ready: *tc.readiness}
			}
			inst, err := (Frontend{MachineID: "test"}).Mount(context.Background(), frontend.Host{
				Identity:       testDeviceIdentity(ident, viewer),
				Experience:     experience,
				Sessions:       hostSessions,
				Capabilities:   frontendtest.Capabilities{},
				VersionHandles: testVersionHandles{},
				SourceOffers:   testSourceOffers{},
				IDs:            ids.Codecs{Allocator: fileIDs{}},
			})
			if err != nil {
				t.Fatal(err)
			}
			body := `{"protocol_version":3,"file_id":"101","profile_id":"7","installation_id":"` + inst.(*instance).installationID() +
				`","playback_attempt_id":"attempt-readiness","subtitle_track_index":5,"client_capabilities":{"codecs_video":["h264"]},` +
				`"client_playback_context":{"deliveries":{"hls":{"enabled":true,"supported_on_device":true,"subtitles":{"sidecar_text":true}}}}}`
			req := httptest.NewRequest("POST", "/api/v2/playback/start", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer account-secret")
			req.Header.Set("X-Silo-Device-Id", "tv")
			w := httptest.NewRecorder()
			inst.Handler().ServeHTTP(w, req)
			if w.Code != http.StatusCreated {
				t.Fatalf("playback start: %d %s", w.Code, w.Body.String())
			}
			var envelope struct {
				Plan struct {
					Subtitle struct {
						Mode     string `json:"mode"`
						Artifact *struct {
							URL string `json:"url"`
						} `json:"artifact"`
					} `json:"subtitle"`
					Selected map[string]json.RawMessage `json:"selected_tracks"`
				} `json:"playback_plan"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
				t.Fatal(err)
			}
			plan := envelope.Plan
			if plan.Subtitle.Mode != tc.wantMode {
				t.Fatalf("subtitle mode = %q, want %q (body %s)", plan.Subtitle.Mode, tc.wantMode, w.Body.String())
			}
			if got := plan.Subtitle.Artifact != nil; got != tc.wantArtifact {
				t.Fatalf("artifact present = %v, want %v (body %s)", got, tc.wantArtifact, w.Body.String())
			}
			_, selected := plan.Selected["subtitle"]
			if selected != tc.wantSubSelect {
				t.Fatalf("selected subtitle = %v, want %v (body %s)", selected, tc.wantSubSelect, w.Body.String())
			}
			// The track stays discoverable so a later switch can still find it.
			if !strings.Contains(w.Body.String(), `"inventory"`) {
				t.Fatalf("subtitle inventory missing: %s", w.Body.String())
			}
		})
	}
}

func boolPtr(v bool) *bool { return &v }
