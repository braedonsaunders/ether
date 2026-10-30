package sessions

import (
	"context"
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
	if playlist.Code != 200 || !strings.Contains(playlist.Body.String(), "/m3u/grant/channel?id=channel") {
		t.Fatalf("playlist %d %q", playlist.Code, playlist.Body.String())
	}
	if head := call(http.MethodHead, "/m3u/grant/channel?id=channel"); head.Code != 200 || live.tunes.Load() != 0 {
		t.Fatalf("channel HEAD %d", head.Code)
	}
	redirect := call(http.MethodGet, "/m3u/grant/channel?id=channel")
	if redirect.Code != http.StatusFound || strings.Contains(redirect.Header().Get("Location"), "grant") || live.tunes.Load() != 0 {
		t.Fatalf("redirect %d %q", redirect.Code, redirect.Header().Get("Location"))
	}
	u, err := url.Parse(redirect.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	manifest := call(http.MethodGet, u.Path)
	if manifest.Code != 200 || !strings.Contains(manifest.Body.String(), "./relay/0.ts") || strings.Contains(manifest.Body.String(), "reference") {
		t.Fatalf("manifest %d %q", manifest.Code, manifest.Body.String())
	}
	segment := strings.TrimSuffix(u.Path, "index.m3u8") + "relay/0.ts"
	if w := call(http.MethodGet, segment); w.Code != 200 || w.Body.String() != "segment" || live.tunes.Load() != 1 {
		t.Fatalf("segment %d %q", w.Code, w.Body.String())
	}
	live.allowed.Store(false)
	if w := call(http.MethodGet, segment); w.Code != 404 {
		t.Fatalf("programme restriction %d", w.Code)
	}
	live.allowed.Store(true)
	grants.mu.Lock()
	grants.revoked = true
	grants.mu.Unlock()
	if w := call(http.MethodGet, segment); w.Code != 404 {
		t.Fatalf("revoked grant %d", w.Code)
	}
}
