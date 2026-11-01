package sessions

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontends/m3u"
	"github.com/braedonsaunders/ether/internal/store"
)

type m3uGrant struct {
	mu      sync.Mutex
	revoked bool
}

func (g *m3uGrant) GrantReference(_ context.Context, surface, credential string) (string, *core.Viewer, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.revoked || surface != "m3u" || credential != "grant" {
		return "", nil, core.ErrUnauthorized
	}
	return "reference", &core.Viewer{ID: 1}, nil
}
func (g *m3uGrant) ValidateGrantReference(_ context.Context, surface, reference string, viewer int64) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.revoked || surface != "m3u" || reference != "reference" || viewer != 1 {
		return core.ErrUnauthorized
	}
	return nil
}

func TestM3UChannelUsesScopedBrokerAndRevalidates(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	events := NewEventBus(nil, active)
	defer events.Close()
	live := &lazyChannelBackend{}
	live.allowed.Store(true)
	pb := &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 2}
	broker, err := NewBroker(BrokerDeps{Live: live, Events: events, Playback: pb, Progress: &fakeProgress{}, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Lookup: func(context.Context, *core.Viewer, core.Device, core.RatingKey) (*core.Title, error) {
		return nil, errors.New("unexpected metadata lookup")
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close(ctx)
	grants := &m3uGrant{}
	lazy, err := NewLazyStreams(db, broker, "m3u", grants, time.Hour, func(*http.Request) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	host := frontend.Host{
		Identity: frontendtest.Identity{ResolveFunc: func(_ context.Context, token string) (*frontend.Viewer, error) {
			_, v, err := grants.GrantReference(ctx, "m3u", token)
			return v, err
		}},
		LiveTV: frontendtest.LiveTV{ChannelsFunc: func(context.Context, *frontend.Viewer) ([]frontend.LiveChannel, error) {
			return []frontend.LiveChannel{{ID: "channel", Name: "News"}}, nil
		}},
		Sessions: broker, LazyStreams: lazy, Capabilities: frontendtest.Capabilities{}, ClientIP: func(*http.Request) (netip.Addr, bool) { return netip.MustParseAddr("192.168.1.8"), true },
		SurfaceURL: func(context.Context, *http.Request) (string, error) { return "http://ether.local/m3u", nil },
	}
	inst, err := (m3u.Frontend{}).Mount(ctx, host)
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, target string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		inst.Handler().ServeHTTP(w, httptest.NewRequest(method, target, nil))
		return w
	}
	playlist := call(http.MethodGet, "/m3u/grant/playlist.m3u")
	if playlist.Code != http.StatusOK || strings.Contains(playlist.Body.String(), "/grant/") || live.tunes.Load() != 0 {
		t.Fatalf("playlist %d %q tunes=%d", playlist.Code, playlist.Body.String(), live.tunes.Load())
	}
	var mediaURLs []string
	for _, line := range strings.Split(playlist.Body.String(), "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			mediaURLs = append(mediaURLs, line)
		}
	}
	if len(mediaURLs) != 1 {
		t.Fatalf("playlist media URLs %v", mediaURLs)
	}
	u, err := url.Parse(mediaURLs[0])
	if err != nil {
		t.Fatal(err)
	}
	prefix := "/m3u/sessions/lineup/"
	parts := strings.Split(strings.TrimPrefix(u.Path, prefix), "/")
	if u.Scheme != "http" || u.Host != "ether.local" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !strings.HasPrefix(u.Path, prefix) || len(parts) != 4 || parts[0] == "" || parts[0] == "grant" || parts[1] != base64.RawURLEncoding.EncodeToString([]byte("channel")) || parts[2] != "live" || parts[3] != "index.m3u8" {
		t.Fatalf("unscoped lineup media URL %q", mediaURLs[0])
	}
	if head := call(http.MethodHead, u.RequestURI()); head.Code != http.StatusOK || head.Body.Len() != 0 || live.tunes.Load() != 0 {
		t.Fatalf("lineup HEAD %d tunes=%d", head.Code, live.tunes.Load())
	}
	access := frontend.SessionAccess{Viewer: &core.Viewer{ID: 1}, Protocol: "m3u"}
	if sessions, err := broker.Sessions(ctx, access); err != nil || len(sessions) != 0 {
		t.Fatalf("HEAD opened broker session: %v %v", sessions, err)
	}
	manifest := call(http.MethodGet, u.RequestURI())
	if manifest.Code != http.StatusOK || !strings.Contains(manifest.Body.String(), "./relay/0.ts") || strings.Contains(manifest.Body.String(), "reference") || strings.Contains(manifest.Body.String(), "/grant/") || live.tunes.Load() != 1 {
		t.Fatalf("manifest %d %q tunes=%d", manifest.Code, manifest.Body.String(), live.tunes.Load())
	}
	sessions, err := broker.Sessions(ctx, access)
	if err != nil || len(sessions) != 1 {
		t.Fatalf("broker sessions %v %v", sessions, err)
	}
	session := sessions[0]
	if session.Protocol != "m3u" || session.ViewerID != 1 || session.Channel != "channel" || !strings.HasPrefix(session.Device.ID, "m3u-") {
		t.Fatalf("wrong broker scope: %+v", session)
	}
	if _, _, err := broker.access(ctx, frontend.SessionAccess{Viewer: access.Viewer, Protocol: "jellyfin", Device: session.Device}, session.ID); err == nil {
		t.Fatal("lineup session accepted cross-surface access")
	}
	segment := strings.TrimSuffix(u.Path, "index.m3u8") + "relay/0.ts"
	if w := call(http.MethodGet, segment); w.Code != 200 || w.Body.String() != "segment" || live.tunes.Load() != 1 {
		t.Fatalf("segment %d %q", w.Code, w.Body.String())
	}
	live.allowed.Store(false)
	for _, request := range []struct{ method, path string }{{http.MethodHead, u.Path}, {http.MethodGet, u.Path}, {http.MethodGet, segment}} {
		if w := call(request.method, request.path); w.Code != http.StatusNotFound {
			t.Fatalf("programme restriction %s %s: %d", request.method, request.path, w.Code)
		}
	}
	live.allowed.Store(true)
	grants.mu.Lock()
	grants.revoked = true
	grants.mu.Unlock()
	for _, request := range []struct{ method, path string }{{http.MethodHead, u.Path}, {http.MethodGet, u.Path}, {http.MethodGet, segment}} {
		if w := call(request.method, request.path); w.Code != http.StatusNotFound {
			t.Fatalf("revoked grant %s %s: %d", request.method, request.path, w.Code)
		}
	}
	if live.tunes.Load() != 1 {
		t.Fatalf("denied request tuned again: %d", live.tunes.Load())
	}
}
