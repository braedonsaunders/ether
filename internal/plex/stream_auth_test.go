package plex

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plex/model"
)

func TestStreamGrantIsolation(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.trans.decision = core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioCopy: true, Container: "fmp4"}
	startPath := "/video/:/transcode/universal/start.m3u8?path=%2Flibrary%2Fmetadata%2F1000000000000550&session=private-stream"
	start := f.get(t, startPath)
	if start.Code != 200 {
		t.Fatal(start.Code, start.Body.String())
	}
	e, _ := f.srv.transcodes.get("private-stream")
	if e.streamToken == "" || !strings.Contains(start.Body.String(), e.streamToken) {
		t.Fatal("master missing grant")
	}
	path := "/video/:/transcode/universal/session/private-stream/base/init.mp4"
	for _, tc := range []struct {
		path, token string
		want        int
	}{
		{path, "", 200}, {streamGrantURL(path, "wrong"), "", 200},
		{streamGrantURL(path, e.streamToken), "", 200}, {path, ownerToken, 200},
		{streamGrantURL(path, e.streamToken), friendToken, 403},
	} {
		rec := f.request(t, http.MethodGet, tc.path, map[string]string{"X-Plex-Token": tc.token})
		if rec.Code != tc.want {
			t.Errorf("credential %q: %d want %d", tc.token, rec.Code, tc.want)
		}
	}
	// A viewer cannot replace another viewer's client-chosen session ID.
	before := f.playback.prepared
	rec := f.get(t, startPath, "X-Plex-Token", friendToken)
	if rec.Code != 403 || f.playback.prepared != before {
		t.Fatalf("session collision: %d prepares=%d", rec.Code, f.playback.prepared-before)
	}
	rec = f.get(t, "/video/:/transcode/universal/stop?session=private-stream", "X-Plex-Token", friendToken)
	if _, ok := f.trans.Session(e.id); !ok {
		t.Fatal("viewer stopped someone else's stream")
	}
	// A grant stops working immediately when its viewer is no longer active.
	a := f.srv.d.Auth.(*fakeAuth)
	for tok, u := range a.users {
		if u.ID == e.userID {
			delete(a.users, tok)
		}
	}
	rec = f.request(t, http.MethodGet, streamGrantURL(path, e.streamToken), map[string]string{"X-Plex-Token": ""})
	if rec.Code != 401 {
		t.Fatalf("revoked grant: %d", rec.Code)
	}
}

func encodedSessionDecision(id string) string {
	return "/video%2F:%2Ftranscode%2Funiversal%2Fsession%2F" + id + "%2Fx/:/transcode/universal/decision?path=%2Flibrary%2Fmetadata%2F1000000000000550"
}

func TestEncodedSlashCapabilityCannotReachDecision(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.trans.decision = core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioCopy: true, Container: "fmp4"}
	start := f.get(t, "/video/:/transcode/universal/start.m3u8?path=%2Flibrary%2Fmetadata%2F1000000000000550&session=path-escape")
	if start.Code != 200 {
		t.Fatal(start.Code, start.Body.String())
	}
	e, _ := f.srv.transcodes.get("path-escape")
	encoded := encodedSessionDecision("path-escape")
	probe := httptest.NewRequest(http.MethodGet, encoded, nil)
	if probe.URL.RawPath == "" && !strings.Contains(probe.URL.EscapedPath(), "%") {
		t.Fatal("encoded probe lost RawPath; the request no longer reproduces the escape")
	}
	if transcodeSessionID(probe.URL.Path) != "path-escape" {
		t.Fatalf("decoded path session %q", transcodeSessionID(probe.URL.Path))
	}

	for _, tc := range []struct{ name, target string }{
		{"stream grant", streamGrantURL(encoded, e.streamToken)},
		{"session id", encoded},
	} {
		before := f.playback.prepared
		rec := f.request(t, http.MethodGet, tc.target, map[string]string{"X-Plex-Token": ""})
		if rec.Code < 400 || rec.Code > 404 {
			t.Fatalf("%s: %d %s", tc.name, rec.Code, rec.Body.String())
		}
		if f.playback.prepared != before {
			t.Fatalf("%s: decision handler ran", tc.name)
		}
	}

	decision := "/video/:/transcode/universal/decision?path=%2Flibrary%2Fmetadata%2F1000000000000550&session=path-escape"
	before := f.playback.prepared
	rec := f.request(t, http.MethodGet, streamGrantURL(decision, e.streamToken), map[string]string{"X-Plex-Token": ""})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("grant on decision: %d %s", rec.Code, rec.Body.String())
	}
	if f.playback.prepared != before {
		t.Fatal("grant on decision reached the handler")
	}

	path := "/video/:/transcode/universal/session/path-escape/base/init.mp4"
	if rec := f.request(t, http.MethodGet, streamGrantURL(path, e.streamToken), map[string]string{"X-Plex-Token": ""}); rec.Code != 200 {
		t.Fatalf("unencoded grant: %d", rec.Code)
	}
	if rec := f.get(t, path); rec.Code != 200 {
		t.Fatalf("unencoded token: %d", rec.Code)
	}
}

func TestSessionReadAuth(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.trans.decision = core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioCopy: true, Container: "fmp4"}
	start := f.get(t, "/video/:/transcode/universal/start.m3u8?path=%2Flibrary%2Fmetadata%2F1000000000000550&session=live-session")
	if start.Code != 200 {
		t.Fatal(start.Code, start.Body.String())
	}
	path := "/video/:/transcode/universal/session/live-session/base/init.mp4"
	// Live session id on playlist/segment follow-up fetches, matching device.
	for _, p := range []string{
		path,
		"/video/:/transcode/universal/session/live-session/base/index.m3u8",
		"/video/:/transcode/universal/session/live-session/base/00000.ts",
	} {
		if rec := f.request(t, http.MethodGet, p, map[string]string{"X-Plex-Token": ""}); rec.Code != 200 {
			t.Fatalf("session id %s: %d", p, rec.Code)
		}
	}
	f.srv.transcodes.put(&transcodeEntry{id: "dash-session", userID: ownerUser.ID, clientID: "client-1", started: true})
	if rec := f.request(t, http.MethodGet, "/video/:/transcode/universal/session/dash-session/0/header", map[string]string{"X-Plex-Token": ""}); rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
		t.Fatalf("dash session id: %d", rec.Code)
	}
	// Stale token next to the live session still passes via the session.
	if rec := f.request(t, http.MethodGet, path, map[string]string{"X-Plex-Token": "expired-token"}); rec.Code != 200 {
		t.Fatalf("stale token + session: %d", rec.Code)
	}
	e, _ := f.srv.transcodes.get("live-session")
	if rec := f.request(t, http.MethodGet, streamGrantURL(path, e.streamToken), map[string]string{"X-Plex-Token": "expired-token"}); rec.Code != 200 {
		t.Fatalf("stale token + grant: %d", rec.Code)
	}
	// Guessing a session that does not exist.
	if rec := f.request(t, http.MethodGet, "/video/:/transcode/universal/session/not-a-session/base/init.mp4", map[string]string{"X-Plex-Token": ""}); rec.Code == 200 {
		t.Fatal("guessed session id was accepted")
	}
	// Wrong device.
	if rec := f.request(t, http.MethodGet, path, map[string]string{"X-Plex-Token": "", "X-Plex-Client-Identifier": "other-device"}); rec.Code == 200 {
		t.Fatal("cross-device session read was accepted")
	}
	// Control routes stay gated: start, ping, stop.
	for _, p := range []string{
		"/video/:/transcode/universal/start.m3u8?path=%2Flibrary%2Fmetadata%2F1000000000000550&session=live-session",
		"/video/:/transcode/universal/start.mpd?path=%2Flibrary%2Fmetadata%2F1000000000000550&session=live-session",
		"/video/:/transcode/universal/start?path=%2Flibrary%2Fmetadata%2F1000000000000550&session=live-session&protocol=http",
		"/video/:/transcode/universal/ping?session=live-session",
		"/video/:/transcode/universal/stop?session=live-session",
	} {
		if rec := f.request(t, http.MethodGet, p, map[string]string{"X-Plex-Token": ""}); rec.Code == 200 {
			t.Fatalf("%s: session id authorized a non-segment route", p)
		}
	}
	// Expired session: stop it, then the segment is refused.
	if rec := f.get(t, "/video/:/transcode/universal/stop?session=live-session"); rec.Code != 200 {
		t.Fatalf("stop: %d", rec.Code)
	}
	if rec := f.request(t, http.MethodGet, path, map[string]string{"X-Plex-Token": ""}); rec.Code == 200 {
		t.Fatal("expired session was accepted")
	}
}

func TestPrivateNotifications(t *testing.T) {
	n := newNotifier()
	defer n.close()
	alice, _ := n.subscribe(nil, &core.User{ID: 2})
	bob, _ := n.subscribe(nil, &core.User{ID: 3})
	owner, _ := n.subscribe(nil, &core.User{ID: 1, IsOwner: true})
	n.publish(model.NotificationContainer{Type: "playing", Size: 1}, 2)
	for _, sub := range []*subscriber{alice, owner} {
		select {
		case <-sub.ch:
		default:
			t.Fatal("intended viewer missed notification")
		}
	}
	select {
	case <-bob.ch:
		t.Fatal("viewer received another viewer's activity")
	default:
	}
}

func TestManifestGrantIncludesInitAndSegments(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.srv.transcodes.put(&transcodeEntry{id: "x", userID: 1, clientID: "c"})
	for _, tc := range []struct{ ct, body string }{
		{"application/vnd.apple.mpegurl", "#EXTM3U\n#EXT-X-MAP:URI=\"init.mp4\"\n0.m4s\n"},
		{"application/dash+xml", `<SegmentTemplate initialization="session/x/$RepresentationID$/header" media="session/x/$RepresentationID$/$Number$.m4s"/>`},
	} {
		rec := httptest.NewRecorder()
		f.srv.serveStreamManifest(rec, httptest.NewRequest("GET", "/", nil), "x", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", tc.ct)
			w.Write([]byte(tc.body))
		})
		if strings.Count(rec.Body.String(), streamGrantParam+"=") != 2 {
			t.Fatalf("unprotected media URL: %s", rec.Body.String())
		}
	}

}

// An escaped session path segment must not let a viewer's own binding
// authorize a different (unescaped) transcode session.
func TestTranscodeSessionPathEscapingCannotCrossSessions(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.trans.decision = core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioCopy: true, Container: "mpegts"}
	// The owner's running stream.
	if rec := f.get(t, "/video/:/transcode/universal/start.m3u8?path=%2Flibrary%2Fmetadata%2F1000000000000550&session=a2Fb"); rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	// The friend's own binding "a".
	f.srv.transcodes.put(&transcodeEntry{id: "a", userID: friendUser.ID, clientID: "client-1", started: true})
	for _, path := range []string{
		"/video/:/transcode/universal/session/a%2Fb/base/index.m3u8",
		"/video/:/transcode/universal/session/a%2Fb/base/00000.ts",
		"/video/:/transcode/universal/session/a%2Fb/0/header",
		"/video/:/transcode/universal/session/a%2Fb/0/1.m4s",
	} {
		rec := f.get(t, path, "X-Plex-Token", friendToken)
		if rec.Code == 200 {
			t.Errorf("%s: friend reached the owner's session: %s", path, rec.Body.String())
		}
	}
}
