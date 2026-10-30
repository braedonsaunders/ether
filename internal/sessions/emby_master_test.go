package sessions

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"testing"
)

func TestEmbyMasterMetadataRequiresProvenIndependence(t *testing.T) {
	original := "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1000\nbase/index.m3u8\n"
	got := embyMasterMetadata(original, 3, false)
	if !strings.HasPrefix(got, "#EXTM3U\n#EXT-X-VERSION:3\n") || strings.Contains(got, "#EXT-X-INDEPENDENT-SEGMENTS") {
		t.Fatalf("unknown independence: %q", got)
	}
	got = embyMasterMetadata(original, 7, true)
	if !strings.Contains(got, "#EXT-X-INDEPENDENT-SEGMENTS\n") || !strings.HasSuffix(got, "base/index.m3u8\n") {
		t.Fatalf("proven independence: %q", got)
	}
}

type provenLadder struct{ fakeLadderSession }

func (*provenLadder) IndependentSegments() bool { return true }

func TestBrokerEmbyMasterUsesSessionGuarantee(t *testing.T) {
	play := &fakePlayback{sessions: map[string]*core.PlaySession{"p": {ID: "p"}}}
	bindings := NewBindingTable[frontend.PlayIntent]()
	bindings.Put(&Binding[frontend.PlayIntent]{ID: "s", Decision: core.Decision{Container: "mpegts"}})
	broker := &Broker{deps: BrokerDeps{Playback: play}, bindings: bindings}
	session := &brokerSession{public: frontend.Session{ID: "s", Protocol: "emby"}, playID: "p"}
	for _, tc := range []struct {
		name    string
		session core.HLSSession
		want    bool
	}{{"unknown", &fakeLadderSession{}, false}, {"proven", &provenLadder{}, true}} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			broker.serveMaster(w, httptest.NewRequest(http.MethodGet, "/", nil), session, tc.session)
			if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "#EXT-X-VERSION:3\n") || strings.Contains(w.Body.String(), "#EXT-X-INDEPENDENT-SEGMENTS\n") != tc.want || w.Header().Get("Content-Length") != strconv.Itoa(w.Body.Len()) {
				t.Fatalf("response: %d %q headers=%v", w.Code, w.Body.String(), w.Header())
			}
		})
	}
	bindings.Put(&Binding[frontend.PlayIntent]{ID: "s", Decision: core.Decision{Container: "fmp4"}})
	w := httptest.NewRecorder()
	broker.serveMaster(w, httptest.NewRequest(http.MethodHead, "/", nil), session, &fakeLadderSession{})
	if w.Code != http.StatusOK || w.Body.Len() != 0 || w.Header().Get("Content-Length") == "" {
		t.Fatalf("HEAD: %d %q", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	broker.serveMaster(w, httptest.NewRequest(http.MethodGet, "/", nil), session, &fakeLadderSession{})
	if !strings.Contains(w.Body.String(), "#EXT-X-VERSION:7\n") {
		t.Fatalf("fMP4 version: %q", w.Body.String())
	}
	session.public.Protocol = "plex"
	w = httptest.NewRecorder()
	broker.serveMaster(w, httptest.NewRequest(http.MethodGet, "/", nil), session, &provenLadder{})
	if strings.Contains(w.Body.String(), "#EXT-X-VERSION") || strings.Contains(w.Body.String(), "#EXT-X-INDEPENDENT-SEGMENTS") {
		t.Fatalf("Plex master changed: %q", w.Body.String())
	}
}
