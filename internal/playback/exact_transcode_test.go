package playback

import (
	"bytes"
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestExactTranscodeRequiresDeclaredH264VideoProfile(t *testing.T) {
	for _, tc := range []struct {
		name string
		caps core.ClientCaps
		want bool
	}{
		{"missing profile", core.ClientCaps{}, false},
		{"audio only", core.ClientCaps{Profiles: &core.ClientCapabilityProfiles{Transcoding: []core.ClientTranscodeProfile{{MediaType: "audio", VideoCodecs: []string{"h264"}}}}}, false},
		{"unsupported encoder", core.ClientCaps{Profiles: &core.ClientCapabilityProfiles{Transcoding: []core.ClientTranscodeProfile{{MediaType: "video", VideoCodecs: []string{"av1"}}}}}, false},
		{"declared h264", core.ClientCaps{Profiles: &core.ClientCapabilityProfiles{Transcoding: []core.ClientTranscodeProfile{{MediaType: "video", VideoCodecs: []string{"h264"}}}}}, true},
		{"download", core.ClientCaps{Download: true, Profiles: &core.ClientCapabilityProfiles{Transcoding: []core.ClientTranscodeProfile{{MediaType: "video", VideoCodecs: []string{"h264"}}}}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := allowsH264Transcode(tc.caps); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestExactTranscodeAdmissionIsSurfaceAndSelectionScoped(t *testing.T) {
	for _, tc := range []struct {
		name, protocol string
		exact, want    bool
	}{
		{"exact jellyfin", "jellyfin", true, true},
		{"nonexact jellyfin", "jellyfin", false, false},
		{"exact plex", "plex", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, []*fakeRelease{mkv("alternate", 10000, time.Minute), mkv("selected", 20000, time.Minute)}, []string{"alternate", "selected"})
			caps := defaultCaps(tc.name)
			caps.Protocol = core.Protocol(tc.protocol)
			caps.Profiles = &core.ClientCapabilityProfiles{Transcoding: []core.ClientTranscodeProfile{{MediaType: "video", VideoCodecs: []string{"h264"}}}}
			ps, err := h.m.PrepareSurface(context.Background(), owner, movieKey, caps, "selected", tc.exact)
			if err != nil || ps == nil || ps.Resolution.Release.ID != "selected" {
				t.Fatalf("selected session %+v: %v", ps, err)
			}
			h.resolver.mu.Lock()
			flags := append([]bool(nil), h.resolver.exactHEVC...)
			h.resolver.mu.Unlock()
			if len(flags) != 1 || flags[0] != tc.want {
				t.Fatalf("resolver permissions = %v, want %v", flags, tc.want)
			}
			if tc.want {
				rec := httptest.NewRecorder()
				req := httptest.NewRequest("GET", "/", nil)
				req.Header.Set("Range", "bytes=0-63")
				h.m.ServeRange(rec, req, ps.ID)
				if rec.Code != 206 || !bytes.Equal(rec.Body.Bytes(), expected("selected", 0, 64)) {
					t.Fatalf("selected source bytes: status %d, body %x", rec.Code, rec.Body.Bytes())
				}
			}
		})
	}
}
