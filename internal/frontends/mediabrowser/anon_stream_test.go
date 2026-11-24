package mediabrowser

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

// anonState is the mutable state behind a fake session broker: the live
// session set the test can close, and the paths served through it.
type anonState struct {
	live      []frontend.Session
	served    []string
	startedTo []string
}

func newAnonBroker(state *anonState, device frontend.Device) frontendtest.Sessions {
	return frontendtest.Sessions{
		PlanFunc: func(context.Context, frontend.PlanRequest) (frontend.PlayPlan, error) {
			return frontend.PlayPlan{ID: "plan", Decision: frontend.Decision{Kind: "directplay"}, DirectPlay: true}, nil
		},
		OpenFunc: func(context.Context, frontend.OpenRequest) (frontend.Session, error) {
			return frontend.Session{ID: "session-1", Item: 5, Device: device}, nil
		},
		SessionsFunc: func(context.Context, frontend.SessionAccess) ([]frontend.Session, error) {
			return state.live, nil
		},
		StartStreamFunc: func(_ context.Context, _ frontend.SessionAccess, id string, _ frontend.StreamRequest) (string, error) {
			state.startedTo = append(state.startedTo, id)
			return "/sessions/media/" + id + "/scoped/index.m3u8", nil
		},
		ServeMediaFunc: func(w http.ResponseWriter, r *http.Request) {
			state.served = append(state.served, r.URL.Path)
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write([]byte("bytes"))
		},
	}
}

func anonClientIP(r *http.Request) (netip.Addr, bool) {
	if v := r.Header.Get("X-Test-IP"); v != "" {
		ip, err := netip.ParseAddr(v)
		return ip, err == nil
	}
	return netip.MustParseAddr("192.0.2.1"), true
}

func mountAnonTest(t *testing.T, protocol string, broker frontendtest.Sessions) frontend.Instance {
	t.Helper()
	viewer := &frontend.Viewer{ID: 1}
	ident := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
	var inst frontend.Instance
	var err error
	host := frontend.Host{Identity: ident, Sessions: broker, Capabilities: frontendtest.Capabilities{}, ClientIP: anonClientIP}
	if protocol == "emby" {
		inst, err = (Emby{MachineID: "test"}).Mount(context.Background(), host)
	} else {
		inst, err = (Jellyfin{MachineID: "test"}).Mount(context.Background(), host)
	}
	if err != nil {
		t.Fatal(err)
	}
	return inst
}

// openSession opens a playback session through the authenticated PlaybackInfo,
// which is what records the anonymous window.
func openSession(t *testing.T, inst frontend.Instance, protocol string, key frontend.RatingKey, ip string) {
	t.Helper()
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/"+protocol+"/Items/"+ids.RatingKeyGUID(key)+"/PlaybackInfo", strings.NewReader(`{"IsPlayback":true}`))
	r.Header.Set("X-Emby-Token", "grant")
	r.Header.Set("X-Emby-Device-Id", "tv")
	r.Header.Set("X-Test-IP", ip)
	inst.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("PlaybackInfo = %d: %s", w.Code, w.Body.String())
	}
}

func anonymousStream(inst frontend.Instance, protocol string, key frontend.RatingKey, ip, query string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/"+protocol+"/Videos/"+ids.RatingKeyGUID(key)+"/stream?static=true"+query, nil)
	if ip != "" {
		r.Header.Set("X-Test-IP", ip)
	}
	inst.Handler().ServeHTTP(w, r)
	return w
}

func TestAnonymousStreamRidesFreshSameIPSession(t *testing.T) {
	state := &anonState{live: []frontend.Session{{ID: "session-1", Item: 5, Protocol: "jellyfin", Device: frontend.Device{ID: "tv"}, MediaPath: "/sessions/media/session-1/scoped/file"}}}
	inst := mountAnonTest(t, "jellyfin", newAnonBroker(state, frontend.Device{ID: "tv"}))
	openSession(t, inst, "jellyfin", 5, "192.0.2.1")

	w := anonymousStream(inst, "jellyfin", 5, "192.0.2.1", "&mediaSourceId="+ids.RatingKeyGUID(5))
	if w.Code != http.StatusPartialContent {
		t.Fatalf("credential-less stream = %d, want 206: %s", w.Code, w.Body.String())
	}
	if len(state.served) != 1 || state.served[0] != "/sessions/media/session-1/scoped/file" {
		t.Fatalf("served paths = %v, want the opened session's file", state.served)
	}
}

func TestAnonymousStreamFromAnotherIPIsRejected(t *testing.T) {
	state := &anonState{live: []frontend.Session{{ID: "session-1", Item: 5, Protocol: "jellyfin", Device: frontend.Device{ID: "tv"}, MediaPath: "/sessions/media/session-1/scoped/file"}}}
	inst := mountAnonTest(t, "jellyfin", newAnonBroker(state, frontend.Device{ID: "tv"}))
	openSession(t, inst, "jellyfin", 5, "192.0.2.1")

	if w := anonymousStream(inst, "jellyfin", 5, "198.51.100.9", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("other-IP stream = %d, want 401", w.Code)
	}
	if len(state.served) != 0 {
		t.Fatalf("served paths = %v, want none", state.served)
	}
}

func TestAnonymousStreamForAnotherItemIsRejected(t *testing.T) {
	state := &anonState{live: []frontend.Session{{ID: "session-1", Item: 5, Protocol: "jellyfin", Device: frontend.Device{ID: "tv"}, MediaPath: "/sessions/media/session-1/scoped/file"}}}
	inst := mountAnonTest(t, "jellyfin", newAnonBroker(state, frontend.Device{ID: "tv"}))
	openSession(t, inst, "jellyfin", 5, "192.0.2.1")

	if w := anonymousStream(inst, "jellyfin", 6, "192.0.2.1", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("other-item stream = %d, want 401", w.Code)
	}
	if len(state.served) != 0 {
		t.Fatalf("served paths = %v, want none", state.served)
	}
}

func TestAnonymousStreamAfterSessionClosesIsRejected(t *testing.T) {
	state := &anonState{live: []frontend.Session{{ID: "session-1", Item: 5, Protocol: "jellyfin", Device: frontend.Device{ID: "tv"}, MediaPath: "/sessions/media/session-1/scoped/file"}}}
	inst := mountAnonTest(t, "jellyfin", newAnonBroker(state, frontend.Device{ID: "tv"}))
	openSession(t, inst, "jellyfin", 5, "192.0.2.1")

	state.live = nil
	if w := anonymousStream(inst, "jellyfin", 5, "192.0.2.1", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("closed-session stream = %d, want 401", w.Code)
	}
	if len(state.served) != 0 {
		t.Fatalf("served paths = %v, want none", state.served)
	}
}

func TestAnonymousStreamDoesNotBypassBadCredential(t *testing.T) {
	state := &anonState{live: []frontend.Session{{ID: "session-1", Item: 5, Protocol: "jellyfin", Device: frontend.Device{ID: "tv"}, MediaPath: "/sessions/media/session-1/scoped/file"}}}
	viewer := &frontend.Viewer{ID: 1}
	ident := frontendtest.Identity{ResolveFunc: func(_ context.Context, token string) (*frontend.Viewer, error) {
		if token == "grant" {
			return viewer, nil
		}
		return nil, frontend.ErrUnauthorized
	}}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: ident, Sessions: newAnonBroker(state, frontend.Device{ID: "tv"}), Capabilities: frontendtest.Capabilities{}, ClientIP: anonClientIP})
	if err != nil {
		t.Fatal(err)
	}
	openSession(t, inst, "jellyfin", 5, "192.0.2.1")

	// An invalid token must never fall back to the anonymous path.
	for _, bad := range []string{"X-Emby-Token", "api_key"} {
		url := "/jellyfin/Videos/" + ids.RatingKeyGUID(5) + "/stream?static=true"
		if bad == "api_key" {
			url += "&api_key=bogus"
		}
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", url, nil)
		r.Header.Set("X-Test-IP", "192.0.2.1")
		if bad == "X-Emby-Token" {
			r.Header.Set(bad, "bogus")
		}
		inst.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("bad-credential stream (%s) = %d, want 401", bad, w.Code)
		}
	}
	if len(state.served) != 0 {
		t.Fatalf("served paths = %v, want none", state.served)
	}
}

func TestAnonymousHLSStreamRidesSession(t *testing.T) {
	state := &anonState{live: []frontend.Session{{ID: "session-1", Item: 5, Protocol: "jellyfin", Device: frontend.Device{ID: "tv"}, MediaPath: "/sessions/media/session-1/scoped/file"}}}
	inst := mountAnonTest(t, "jellyfin", newAnonBroker(state, frontend.Device{ID: "tv"}))
	openSession(t, inst, "jellyfin", 5, "192.0.2.1")
	state.startedTo = nil

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/jellyfin/Videos/"+ids.RatingKeyGUID(5)+"/master.m3u8", nil)
	r.Header.Set("X-Test-IP", "192.0.2.1")
	inst.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusTemporaryRedirect || w.Header().Get("Location") != "/jellyfin/sessions/media/session-1/scoped/index.m3u8" {
		t.Fatalf("anonymous HLS = %d %q", w.Code, w.Header().Get("Location"))
	}
	if len(state.startedTo) != 1 || state.startedTo[0] != "session-1" {
		t.Fatalf("started = %v, want session-1", state.startedTo)
	}
}

func TestAnonymousStreamWorksForEmbyAudioAlias(t *testing.T) {
	state := &anonState{live: []frontend.Session{{ID: "session-1", Item: 5, Protocol: "emby", Device: frontend.Device{ID: "tv"}, MediaPath: "/sessions/media/session-1/scoped/file"}}}
	inst := mountAnonTest(t, "emby", newAnonBroker(state, frontend.Device{ID: "tv"}))
	openSession(t, inst, "emby", 5, "192.0.2.1")

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/emby/Audio/"+ids.RatingKeyGUID(5)+"/stream?static=true", nil)
	r.Header.Set("X-Test-IP", "192.0.2.1")
	inst.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusPartialContent {
		t.Fatalf("emby anonymous audio stream = %d, want 206: %s", w.Code, w.Body.String())
	}
	if len(state.served) != 1 || state.served[0] != "/sessions/media/session-1/scoped/file" {
		t.Fatalf("served paths = %v", state.served)
	}
}

// Findroid posts PlaybackInfo for every queued episode before it reads the
// one it plays. The broker keeps one session per device, so those opens
// retire the playing episode's session; its credential-less stream reopens
// it. A stopped title is never reopened.
func TestAnonymousStreamReopensSessionSupersededOnDevice(t *testing.T) {
	state := &anonState{}
	device := frontend.Device{ID: "tv"}
	broker := newAnonBroker(state, device)
	opened := 0
	var planned []frontend.RatingKey
	broker.PlanFunc = func(_ context.Context, req frontend.PlanRequest) (frontend.PlayPlan, error) {
		planned = append(planned, req.Item)
		return frontend.PlayPlan{ID: "plan-" + ids.RatingKeyGUID(req.Item), Decision: frontend.Decision{Kind: "directplay"}, DirectPlay: true}, nil
	}
	broker.OpenFunc = func(_ context.Context, req frontend.OpenRequest) (frontend.Session, error) {
		opened++
		item := frontend.RatingKey(5)
		if req.PlanID == "plan-"+ids.RatingKeyGUID(6) {
			item = 6
		}
		id := "session-" + string(rune('0'+opened))
		// One session per device: every open supersedes the previous one.
		state.live = []frontend.Session{{ID: id, Item: item, Protocol: "jellyfin", Device: device, MediaPath: "/sessions/media/" + id + "/scoped/file"}}
		return state.live[0], nil
	}
	inst := mountAnonTest(t, "jellyfin", broker)
	openSession(t, inst, "jellyfin", 5, "192.0.2.1")
	openSession(t, inst, "jellyfin", 6, "192.0.2.1") // the queued next episode

	if w := anonymousStream(inst, "jellyfin", 5, "192.0.2.1", ""); w.Code != http.StatusPartialContent {
		t.Fatalf("superseded stream = %d, want 206: %s", w.Code, w.Body.String())
	}
	if len(state.served) != 1 || state.served[0] != "/sessions/media/session-3/scoped/file" || planned[len(planned)-1] != 5 {
		t.Fatalf("served %v after planning %v, want a reopened session for item 5", state.served, planned)
	}
	// A second ranged GET rides the reopened session.
	if w := anonymousStream(inst, "jellyfin", 5, "192.0.2.1", ""); w.Code != http.StatusPartialContent || opened != 3 {
		t.Fatalf("second GET = %d with %d opens, want 206 without another open", w.Code, opened)
	}

	stop := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/jellyfin/Sessions/Playing/Stopped", strings.NewReader(`{"ItemId":"`+ids.RatingKeyGUID(5)+`","PlaySessionId":"session-3"}`))
	r.Header.Set("X-Emby-Token", "grant")
	r.Header.Set("X-Emby-Device-Id", "tv")
	r.Header.Set("X-Test-IP", "192.0.2.1")
	inst.Handler().ServeHTTP(stop, r)
	state.live = nil
	if w := anonymousStream(inst, "jellyfin", 5, "192.0.2.1", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("stopped stream = %d, want 401", w.Code)
	}
}

// While a credential-less stream is playing, a PlaybackInfo for another item
// from the same device is a queue prefetch: nothing opens, the playing
// session stays, and the queued item opens when its stream is requested.
func TestAnonymousQueuePrefetchDefersOpen(t *testing.T) {
	state := &anonState{}
	device := frontend.Device{ID: "tv"}
	broker := newAnonBroker(state, device)
	opened := 0
	broker.PlanFunc = func(_ context.Context, req frontend.PlanRequest) (frontend.PlayPlan, error) {
		return frontend.PlayPlan{ID: "plan-" + ids.RatingKeyGUID(req.Item), Decision: frontend.Decision{Kind: "directplay"}, DirectPlay: true, Estimated: req.Intent.Estimate}, nil
	}
	broker.OpenFunc = func(_ context.Context, req frontend.OpenRequest) (frontend.Session, error) {
		opened++
		item := frontend.RatingKey(5)
		if req.PlanID == "plan-"+ids.RatingKeyGUID(6) {
			item = 6
		}
		id := "session-" + string(rune('0'+opened))
		state.live = []frontend.Session{{ID: id, Item: item, Protocol: "jellyfin", Device: device, MediaPath: "/sessions/media/" + id + "/scoped/file"}}
		return state.live[0], nil
	}
	inst := mountAnonTest(t, "jellyfin", broker)
	openSession(t, inst, "jellyfin", 5, "192.0.2.1")
	if w := anonymousStream(inst, "jellyfin", 5, "192.0.2.1", ""); w.Code != http.StatusPartialContent {
		t.Fatalf("playing stream = %d", w.Code)
	}
	openSession(t, inst, "jellyfin", 6, "192.0.2.1") // the queued next episode
	if opened != 1 || state.live[0].ID != "session-1" {
		t.Fatalf("queue prefetch opened a session: opens=%d live=%v", opened, state.live)
	}
	if w := anonymousStream(inst, "jellyfin", 5, "192.0.2.1", ""); w.Code != http.StatusPartialContent || state.served[len(state.served)-1] != "/sessions/media/session-1/scoped/file" {
		t.Fatalf("playing stream after prefetch = %d %v", w.Code, state.served)
	}
	if w := anonymousStream(inst, "jellyfin", 6, "192.0.2.1", ""); w.Code != http.StatusPartialContent || opened != 2 || state.served[len(state.served)-1] != "/sessions/media/session-2/scoped/file" {
		t.Fatalf("queued stream = %d opens=%d served=%v", w.Code, opened, state.served)
	}
}

// A stream alias for an already-started HLS session must redirect straight to
// its active master playlist instead of calling StartStream again at position 0.
func TestServeAliasSessionReusesActiveHLSMaster(t *testing.T) {
	device := frontend.Device{ID: "tablet"}
	startCalls := 0
	broker := frontendtest.Sessions{
		SessionsFunc: func(context.Context, frontend.SessionAccess) ([]frontend.Session, error) {
			return []frontend.Session{{ID: "hls-session", Item: 5, Protocol: "jellyfin", Device: device, MediaPath: "/sessions/media/hls-session/scoped/master.m3u8"}}, nil
		},
		StartStreamFunc: func(context.Context, frontend.SessionAccess, string, frontend.StreamRequest) (string, error) {
			startCalls++
			return "/sessions/media/hls-session/scoped/master.m3u8", nil
		},
	}
	inst := mountAnonTest(t, "jellyfin", broker)
	r := httptest.NewRequest("GET", "/jellyfin/Videos/"+ids.RatingKeyGUID(5)+"/master.m3u8?PlaySessionId=hls-session", nil)
	r.Header.Set("X-Emby-Token", "grant")
	r.Header.Set("X-Emby-Device-Id", "tablet")
	w := httptest.NewRecorder()
	inst.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusTemporaryRedirect || w.Header().Get("Location") != "/jellyfin/sessions/media/hls-session/scoped/master.m3u8" || startCalls != 0 {
		t.Fatalf("HLS alias = %d loc=%q startCalls=%d, want 307 without restarting stream", w.Code, w.Header().Get("Location"), startCalls)
	}
}

// deviceVersionHandles resolves one version handle only for the device it
// was issued to, as the session store does: any other or an empty device
// finds nothing.
type deviceVersionHandles struct {
	item   frontend.RatingKey
	handle string
	device string
}

func (h *deviceVersionHandles) Issue(context.Context, frontend.VersionScope, frontend.RatingKey, string) (string, error) {
	return "", errors.New("not offered")
}
func (h *deviceVersionHandles) Resolve(_ context.Context, scope frontend.VersionScope, id string) (frontend.VersionTarget, error) {
	if scope.Device.ID != h.device || id != h.handle {
		return frontend.VersionTarget{}, errors.New("unknown handle")
	}
	return frontend.VersionTarget{Item: h.item}, nil
}
func (h *deviceVersionHandles) MatchSession(_ context.Context, scope frontend.VersionScope, id, session string) error {
	if scope.Device.ID != h.device || id != h.handle || session != "session-1" {
		return errors.New("wrong source")
	}
	return nil
}

// A credential-less player that streams a picked version sends no device id;
// the version is checked against the device that opened the session.
func TestAnonymousStreamOfPickedVersionUsesSessionDevice(t *testing.T) {
	handle := "4e0f0a56-014b-4883-89b7-e9465f03462e"
	state := &anonState{live: []frontend.Session{{ID: "session-1", Item: 5, Protocol: "jellyfin", Device: frontend.Device{ID: "tv"}, MediaPath: "/sessions/media/session-1/scoped/file"}}}
	viewer := &frontend.Viewer{ID: 1}
	ident := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
	host := frontend.Host{Identity: ident, Sessions: newAnonBroker(state, frontend.Device{ID: "tv"}), Capabilities: frontendtest.Capabilities{}, ClientIP: anonClientIP, VersionHandles: &deviceVersionHandles{item: 5, handle: handle, device: "tv"}}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), host)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/jellyfin/Items/"+ids.RatingKeyGUID(5)+"/PlaybackInfo", strings.NewReader(`{"IsPlayback":true,"MediaSourceId":"`+handle+`"}`))
	r.Header.Set("X-Emby-Token", "grant")
	r.Header.Set("X-Emby-Device-Id", "tv")
	r.Header.Set("X-Test-IP", "192.0.2.1")
	inst.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("PlaybackInfo = %d: %s", w.Code, w.Body.String())
	}

	if w := anonymousStream(inst, "jellyfin", 5, "192.0.2.1", "&mediaSourceId="+handle); w.Code != http.StatusPartialContent {
		t.Fatalf("credential-less version stream = %d, want 206: %s", w.Code, w.Body.String())
	}
	if len(state.served) != 1 || state.served[0] != "/sessions/media/session-1/scoped/file" {
		t.Fatalf("served paths = %v, want the opened session's file", state.served)
	}
}
