package vela

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/caps"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

// dvHarness plays through the Vela handlers with the declared capabilities
// built as the host builds them, before its owner rules.
type dvHarness struct {
	i     *instance
	plans []frontend.PlanRequest
}

func newDVHarness() *dvHarness {
	h := &dvHarness{}
	broker := frontendtest.Sessions{
		PlanFunc: func(_ context.Context, req frontend.PlanRequest) (frontend.PlayPlan, error) {
			h.plans = append(h.plans, req)
			return frontend.PlayPlan{ID: "plan", ETag: "src", HLS: true, Media: core.MediaInfo{Container: "mkv"}, Decision: core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioStream: -1, SubtitleStream: -1}}, nil
		},
		OpenFunc: func(context.Context, frontend.OpenRequest) (frontend.Session, error) {
			return frontend.Session{ID: "session"}, nil
		},
		StartStreamFunc: func(context.Context, frontend.SessionAccess, string, frontend.StreamRequest) (string, error) {
			return "/sessions/media/session/grant/master.m3u8", nil
		},
	}
	declare := frontendtest.Capabilities{BuildFunc: func(ctx context.Context, req frontend.CapabilityRequest) (frontend.CapabilityResult, error) {
		c, err := caps.Builder{}.Build(ctx, req.Device, req.Profile, req.ProtocolInfo, req.UserAgent)
		return frontend.CapabilityResult{Client: c}, err
	}}
	h.i = &instance{ctx: context.Background(), host: frontend.Host{Identity: identityFake{}, Sessions: broker, Capabilities: declare}, sessions: map[string]*playSession{}}
	return h
}

func (h *dvHarness) post(path string, b object) (*httptest.ResponseRecorder, map[string]any) {
	raw, _ := json.Marshal(b)
	r := httptest.NewRequest("POST", "/vela/v0"+path, nil)
	r.Header.Set("X-Vela-Device-Id", "apple-tv")
	r = r.WithContext(context.WithValue(r.Context(), bodyKey{}, json.RawMessage(raw)))
	w := httptest.NewRecorder()
	if path == "/negotiate" {
		h.i.negotiate(w, r) // signed out, as a first launch negotiates
	} else {
		r.Header.Set("Authorization", "Bearer access")
		h.i.playback(w, r)
	}
	var doc map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &doc)
	return w, doc
}

// dvClient is Vela's declaration as its Apple probe sends it, with the Dolby
// Vision and audio-output members set (or left out) by the caller.
func dvClient(hdr []string, extra map[string]any) object {
	c := object{"protocolVersions": []int{0}, "device": object{"id": "apple-tv", "name": "Den", "platform": "tvos", "appVersion": "0.2.0"}, "hls": true, "mse": false, "containers": []string{"mp4"}, "videoCodecs": []string{"h264", "hevc"}, "audioCodecs": []string{"aac", "ac3", "eac3"}, "subtitleFormats": []string{"webvtt"}, "hdr": hdr}
	for k, v := range extra {
		c[k] = v
	}
	return c
}

var dvDeclarationShapes = []struct {
	name  string
	hdr   []string
	extra map[string]any
}{
	{"absent", []string{"hdr10", "hlg", "dv"}, nil},
	{"empty", []string{"hdr10", "hlg", "dv"}, map[string]any{"dolbyVisionProfiles": []string{}}},
	{"8.1", []string{"hdr10", "hlg", "dv"}, map[string]any{"dolbyVisionProfiles": []string{"8.1"}}},
	{"8.4", []string{"hdr10", "hlg", "dv"}, map[string]any{"dolbyVisionProfiles": []string{"8.4"}}},
	{"reshape-only", []string{}, map[string]any{"dolbyVisionReshape": []string{"5"}, "audioOutput": map[string]any{"passthrough": []string{}, "immersive": []string{"eac3-joc"}, "maxChannels": 2, "lossless": false}}},
	{"apple-dv", []string{"hdr10", "hlg", "dv"}, map[string]any{"dolbyVisionProfiles": []string{"5", "7", "8.1", "8.4"}, "audioOutput": map[string]any{"passthrough": []string{"ac3", "eac3", "eac3-joc"}, "immersive": []string{"eac3-joc", "truehd-atmos"}, "maxChannels": 12, "maxSampleRate": 48000, "lossless": true}}},
}

// dvMembers keeps only the declaration members this test follows; an
// omitted member stays omitted and an empty list stays an empty list.
func dvMembers(t *testing.T, v any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var all map[string]any
	if err := json.Unmarshal(raw, &all); err != nil {
		t.Fatal(err)
	}
	out := map[string]any{}
	for _, k := range []string{"dolbyVisionProfiles", "dolbyVisionReshape", "audioOutput"} {
		if v, ok := all[k]; ok {
			out[k] = v
		}
	}
	return out
}

// Negotiation echoes the Dolby Vision and audio-output members exactly, and
// detail, versions and Play keep them, so omitted (an older client) and an
// explicit empty list stay distinct.
func TestVelaNegotiatedDVVariants(t *testing.T) {
	for _, tc := range dvDeclarationShapes {
		t.Run(tc.name, func(t *testing.T) {
			h := newDVHarness()
			sent := dvClient(tc.hdr, tc.extra)
			want := dvMembers(t, sent)
			w, doc := h.post("/negotiate", object{"client": sent})
			if w.Code != 200 {
				t.Fatalf("negotiate %d %s", w.Code, w.Body.String())
			}
			if got := dvMembers(t, doc["client"]); !reflect.DeepEqual(got, want) {
				t.Fatalf("negotiate echo = %v, want %v", got, want)
			}
			r := httptest.NewRequest("GET", "/vela/v0/titles/x/versions", nil)
			r.Header.Set("X-Vela-Device-Id", "apple-tv")
			if got := dvMembers(t, h.i.negotiatedClientFor(r, requestDevice(r))); !reflect.DeepEqual(got, want) {
				t.Fatalf("detail/versions declaration = %v, want %v", got, want)
			}
			w, _ = h.post("/playback", object{"titleId": ids.RatingKeyGUID(core.MovieKey(1)), "positionSeconds": 0.0, "client": sent})
			if w.Code != 200 {
				t.Fatalf("playback %d %s", w.Code, w.Body.String())
			}
			if got := dvMembers(t, h.i.sessions["session"].client); !reflect.DeepEqual(got, want) {
				t.Fatalf("playback declaration = %v, want %v", got, want)
			}
		})
	}
}
