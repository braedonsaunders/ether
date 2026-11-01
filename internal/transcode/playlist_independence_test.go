package transcode

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

func TestPlaylistIndependenceRequiresProofAcrossRungsAndFailover(t *testing.T) {
	const tag = "#EXT-X-INDEPENDENT-SEGMENTS\n"
	for _, tc := range []struct {
		name string
		req  func(*testing.T, string) core.HLSRequest
	}{
		{"copied-source", func(t *testing.T, id string) core.HLSRequest { return remuxRequest(t, id, time.Hour) }},
		{"encoded", transcodeRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fr := newFakeRunner()
			m := newTestManager(t, fr, config.Transcode{ABRLadder: true})
			req := tc.req(t, "strict-"+tc.name)
			req.RequireIndependentProof = true
			s := startSession(t, m, req)
			check := func() {
				t.Helper()
				rungs := s.Rungs()
				if len(rungs) == 0 {
					t.Fatal("no advertised rungs")
				}
				for _, rung := range rungs {
					w := httptest.NewRecorder()
					s.ServeRungPlaylist(w, httptest.NewRequest(http.MethodGet, "/", nil), rung.ID)
					if w.Code != http.StatusOK || strings.Contains(w.Body.String(), tag) {
						t.Fatalf("rung %s: status %d body %q", rung.ID, w.Code, w.Body.String())
					}
				}
				if s.IndependentSegments() {
					t.Fatal("unverified session claimed segment independence")
				}
			}
			check()
			if err := s.SwapInput(context.Background(), "http://127.0.0.1:1/source/b", req.Info, 4*time.Second); err != nil {
				t.Fatal(err)
			}
			check()
		})
	}
}

func TestPlaylistIndependenceExplicitAndLegacy(t *testing.T) {
	name := func(n int) string { return SegmentName(n, ContainerMPEGTS) }
	legacy := string(mediaPlaylist(12*time.Second, 4*time.Second, ContainerMPEGTS, name))
	strict := string(mediaPlaylistWithIndependence(12*time.Second, 4*time.Second, ContainerMPEGTS, name, false))
	proven := string(mediaPlaylistWithIndependence(12*time.Second, 4*time.Second, ContainerMPEGTS, name, true))
	const tag = "#EXT-X-INDEPENDENT-SEGMENTS\n"
	if proven != legacy || strings.Replace(legacy, tag, "", 1) != strict {
		t.Fatalf("legacy, unproven and proven playlists differ unexpectedly")
	}
}

// A real open-GOP sample contains pictures that precede the random-access
// frame in presentation order; a copy playlist cannot claim independence.
func TestOpenGOPCopyPlaylistOmitsUnprovenIndependence(t *testing.T) {
	e := newRealEnv(t)
	e.gen(t, "open-gop.mkv", 6, openGOPx265(24, "ctu=64")...)
	if lead, _ := e.keyframe(t, "open-gop.mkv", 1); lead <= 0 {
		t.Fatal("fixture did not contain open-GOP leading pictures")
	}
	info, err := e.m.Probe(t.Context(), e.url("open-gop.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	hs, err := e.m.StartHLS(t.Context(), core.HLSRequest{SessionID: "open-gop-strict", InputURL: e.url("open-gop.mkv"), Info: *info, RequireIndependentProof: true, Decision: core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioCopy: true, Container: ContainerFMP4}})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	hs.ServePlaylist(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "#EXT-X-INDEPENDENT-SEGMENTS") {
		t.Fatalf("open-GOP playlist: %d %q", w.Code, w.Body.String())
	}
}
