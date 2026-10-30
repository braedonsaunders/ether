package sessions

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/store"
)

type lazyChannelBackend struct {
	allowed atomic.Bool
	tunes   atomic.Int32
	stops   atomic.Int32
}

func (l *lazyChannelBackend) Allowed(_ context.Context, _ *core.Viewer, id string) error {
	if id != "channel" || !l.allowed.Load() {
		return core.ErrNotFound
	}
	return nil
}
func (l *lazyChannelBackend) Tune(context.Context, *core.Viewer, string, core.ClientCaps, string) (string, error) {
	l.tunes.Add(1)
	return "live-source", nil
}
func (l *lazyChannelBackend) Stop(context.Context, *core.Viewer, string) error {
	l.stops.Add(1)
	return nil
}
func (*lazyChannelBackend) Serve(w http.ResponseWriter, _ *http.Request, _ *core.Viewer, _, tail, base string) {
	if strings.HasSuffix(tail, ".m3u8") {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		_, _ = w.Write([]byte("#EXTM3U\n" + base + "relay/0.ts\n" + base + "resource/variant.m3u8\n"))
		return
	}
	_, _ = w.Write([]byte("segment"))
}

func TestLazyChannelRechecksGrantAndProgrammeForEveryResource(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	now := time.Now()
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	events := NewEventBus(nil, active)
	defer events.Close()
	live := &lazyChannelBackend{}
	live.allowed.Store(true)
	pb := &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 2}
	var network atomic.Int32
	broker, err := NewBroker(BrokerDeps{Live: live, Events: events, Playback: pb, Progress: &fakeProgress{}, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Now: func() time.Time { return now }, Lookup: func(context.Context, *core.Viewer, core.Device, core.RatingKey) (*core.Title, error) {
		network.Add(1)
		return nil, errors.New("network metadata forbidden")
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close(ctx)
	grants := &fakeLazyGrants{}
	lazy, err := NewLazyStreams(db, broker, "stremio", grants, time.Hour, func(*http.Request) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	req := frontend.PlanRequest{Viewer: &core.Viewer{ID: 1}, Device: core.Device{ID: "viewer-device"}, Protocol: "stremio", Channel: "channel"}
	path, err := lazy.Issue(ctx, "account-credential-secret", req)
	if err != nil || !strings.HasSuffix(path, "/live/index.m3u8") || strings.Contains(path, "account-credential-secret") {
		t.Fatalf("issue %q: %v", path, err)
	}
	serve := func(method, path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		lazy.ServeHTTP(w, httptest.NewRequest(method, path, nil))
		return w
	}
	segment := strings.TrimSuffix(path, "index.m3u8") + "relay/0.ts"
	if w := serve(http.MethodGet, segment); w.Code != 404 || live.tunes.Load() != 0 {
		t.Fatalf("orphan segment %d", w.Code)
	}
	if w := serve(http.MethodHead, path); w.Code != 200 || live.tunes.Load() != 0 {
		t.Fatalf("HEAD %d tuned", w.Code)
	}
	if w := serve(http.MethodHead, segment); w.Code != 404 {
		t.Fatalf("orphan segment HEAD %d", w.Code)
	}
	if w := serve(http.MethodGet, strings.TrimSuffix(path, "index.m3u8")+"../file"); w.Code != 404 {
		t.Fatalf("traversal %d", w.Code)
	}
	grants.mu.Lock()
	grants.checks, grants.revokeAt = 0, 3
	grants.mu.Unlock()
	if w := serve(http.MethodGet, path); w.Code != 404 || live.stops.Load() != 1 {
		t.Fatalf("revoked during tune: status=%d stops=%d", w.Code, live.stops.Load())
	}
	grants.mu.Lock()
	grants.revoked, grants.revokeAt = false, 0
	grants.mu.Unlock()
	var wg sync.WaitGroup
	status := make(chan int, 5)
	for n := 0; n < 5; n++ {
		wg.Add(1)
		go func() { defer wg.Done(); status <- serve(http.MethodGet, path).Code }()
	}
	wg.Wait()
	close(status)
	for code := range status {
		if code != 200 {
			t.Fatalf("manifest %d", code)
		}
	}
	if live.tunes.Load() != 2 {
		t.Fatalf("concurrent tunes %d", live.tunes.Load())
	}
	w := serve(http.MethodGet, path)
	if !strings.Contains(w.Body.String(), "./relay/0.ts") || strings.Contains(w.Body.String(), "/sessions/media/") {
		t.Fatalf("manifest %q", w.Body.String())
	}
	variant := strings.TrimSuffix(path, "index.m3u8") + "resource/variant.m3u8"
	if w = serve(http.MethodGet, variant); w.Code != 200 || !strings.Contains(w.Body.String(), "../relay/0.ts") {
		t.Fatalf("variant %d %q", w.Code, w.Body.String())
	}
	for n := 0; n < cap(broker.startSlots); n++ {
		broker.startSlots <- struct{}{}
	}
	if w = serve(http.MethodGet, segment); w.Code != 200 {
		t.Fatalf("segment %d", w.Code)
	}
	for n := 0; n < cap(broker.startSlots); n++ {
		<-broker.startSlots
	}
	// A live HLS session must survive beyond the original one-hour URL TTL.
	for n := 0; n < 7; n++ {
		now = now.Add(30 * time.Minute)
		if w = serve(http.MethodGet, segment); w.Code != http.StatusOK || live.tunes.Load() != 2 {
			t.Fatalf("segment after %s: status=%d tunes=%d", time.Duration(n+1)*30*time.Minute, w.Code, live.tunes.Load())
		}
	}
	live.allowed.Store(false)
	if w = serve(http.MethodGet, segment); w.Code != 404 {
		t.Fatalf("programme policy bypass %d", w.Code)
	}
	if w = serve(http.MethodHead, path); w.Code != 404 {
		t.Fatalf("programme HEAD bypass %d", w.Code)
	}
	live.allowed.Store(true)
	grants.mu.Lock()
	grants.revoked = true
	grants.mu.Unlock()
	if w = serve(http.MethodGet, segment); w.Code != 404 {
		t.Fatalf("revoked grant %d", w.Code)
	}
	grants.mu.Lock()
	grants.revoked = false
	grants.mu.Unlock()
	now = now.Add(time.Hour)
	if w = serve(http.MethodGet, path); w.Code != 404 {
		t.Fatalf("expired capability %d", w.Code)
	}
	if network.Load() != 0 {
		t.Fatal("live resource fetched network metadata")
	}
}

type lineupGrants struct{ fakeLazyGrants }

func (g *lineupGrants) GrantReference(ctx context.Context, surface, credential string) (string, *core.Viewer, error) {
	if surface != "m3u" {
		return "", nil, core.ErrUnauthorized
	}
	return g.fakeLazyGrants.GrantReference(ctx, "stremio", credential)
}
func (g *lineupGrants) ValidateGrantReference(ctx context.Context, surface, reference string, viewer int64) error {
	if surface != "m3u" {
		return core.ErrUnauthorized
	}
	return g.fakeLazyGrants.ValidateGrantReference(ctx, "stremio", reference, viewer)
}
func TestLazyLineupReusesOneBoundCapability(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	events := NewEventBus(nil, active)
	defer events.Close()
	live := &lazyChannelBackend{}
	live.allowed.Store(true)
	pb := &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 2}
	b, err := NewBroker(BrokerDeps{Live: live, Events: events, Playback: pb, Progress: &fakeProgress{}, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(ctx)
	grants := &lineupGrants{}
	streams, err := NewLazyStreams(db, b, "m3u", grants, time.Hour, func(*http.Request) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	lazy := streams.(*lazyStreams)
	req := frontend.PlanRequest{Viewer: &core.Viewer{ID: 1}, Device: core.Device{ID: "m3u-device"}, Protocol: "m3u"}
	prefix, err := lazy.IssueLineup(ctx, "account-credential-secret", req)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 300; i++ {
		got, err := lazy.IssueLineup(ctx, "account-credential-secret", req)
		if err != nil || got != prefix {
			t.Fatalf("retry %q %v", got, err)
		}
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sessions_lazy_streams`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("lineup charged %d %v", n, err)
	}
	serve := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		lazy.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		return w
	}
	path := prefix + "Y2hhbm5lbA/live/index.m3u8"
	if w := serve(path); w.Code != 200 || !strings.Contains(w.Body.String(), "./relay/0.ts") {
		t.Fatalf("manifest %d %q", w.Code, w.Body.String())
	}
	if w := serve(prefix + "Y2hhbm5lbA/live/relay/0.ts"); w.Code != 200 {
		t.Fatalf("segment %d", w.Code)
	}
	if w := serve(prefix + "ZGVuaWVk/live/index.m3u8"); w.Code != 404 {
		t.Fatalf("unauthorized channel %d", w.Code)
	}
	// An ordinary lazy path must never turn a lineup scope into an item capability.
	token := strings.TrimSuffix(strings.TrimPrefix(prefix, "/sessions/lineup/"), "/")
	if w := serve("/sessions/lazy/" + token + "/live/index.m3u8"); w.Code != 404 {
		t.Fatal("lineup namespace confused")
	}
	live.allowed.Store(false)
	if w := serve(path); w.Code != 404 {
		t.Fatal("current channel policy bypassed")
	}
	live.allowed.Store(true)
	grants.mu.Lock()
	grants.revoked = true
	grants.mu.Unlock()
	if w := serve(path); w.Code != 404 {
		t.Fatal("source revocation bypassed")
	}
	checkLazyCounts(t, db)
}
