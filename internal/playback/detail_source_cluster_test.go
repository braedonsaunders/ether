package playback

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/sessions"
)

type detailTestGrants struct{}

func (detailTestGrants) GrantReference(_ context.Context, surface, credential string) (string, *core.Viewer, error) {
	if surface != "jellyfin" || credential != "account-secret" {
		return "", nil, core.ErrUnauthorized
	}
	v := *friend
	return "grant-reference", &v, nil
}
func (detailTestGrants) ValidateGrantReference(_ context.Context, surface, reference string, viewer int64) error {
	if surface != "jellyfin" || reference != "grant-reference" || viewer != friend.ID {
		return core.ErrUnauthorized
	}
	return nil
}

func TestDetailSourceReopensSameRevisionOnAnotherNode(t *testing.T) {
	a, other, _ := twoNodes(t, nil)
	ctx := context.Background()
	active := func(_ context.Context, id int64) (*core.Viewer, error) {
		if id != friend.ID {
			return nil, core.ErrUnauthorized
		}
		v := *friend
		return &v, nil
	}
	makeBroker := func(h *harness) *sessions.Broker {
		events := sessions.NewEventBus(nil, active)
		t.Cleanup(events.Close)
		lookup := func(ctx context.Context, _ *core.Viewer, _ core.Device, key core.RatingKey) (*core.Title, error) {
			return h.catalog.Get(ctx, key)
		}
		b, err := sessions.NewBroker(sessions.BrokerDeps{DB: h.db, Lookup: lookup, MediaLookup: lookup, Playback: h.m, Transcoder: h.tr, Progress: h.state, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Events: events, Now: h.clock.Now})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { b.Close(ctx) })
		return b
	}
	first, second := makeBroker(a), makeBroker(other)
	device := core.Device{ID: "tv"}
	req := frontend.PlanRequest{Viewer: friend, Device: device, Protocol: "jellyfin", Item: movieKey, Caps: defaultCaps(device.ID), Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true, DirectStream: true}}}
	detail, err := first.DetailPlan(ctx, req)
	if err != nil || detail.DetailSource == "" || detail.ETag == "" {
		t.Fatalf("detail: %+v %v", detail, err)
	}
	scope := frontend.VersionScope{Viewer: friend, Device: device, Protocol: req.Protocol, Caps: req.Caps}
	target, err := second.ResolveDetailSource(ctx, scope, detail.DetailSource)
	if err != nil || target.Item != req.Item || target.ETag != detail.ETag {
		t.Fatalf("cross-node resolve: %+v %v", target, err)
	}
	bad := scope
	bad.Device.ID = "other-device"
	if _, err := second.ResolveDetailSource(ctx, bad, detail.DetailSource); err == nil {
		t.Fatal("foreign device resolved handle")
	}
	req.DetailSource = detail.DetailSource
	lazy, err := sessions.NewLazyStreams(other.db, second, req.Protocol, detailTestGrants{}, time.Hour, func(*http.Request) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	path, err := lazy.Issue(ctx, "account-secret", req)
	if err != nil || strings.Contains(path, "account-secret") || strings.Contains(path, "grant-reference") {
		t.Fatalf("lazy issue %q: %v", path, err)
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Header.Set("Range", "bytes=0-2")
	lazy.ServeHTTP(w, r)
	if w.Code != http.StatusPartialContent {
		t.Fatalf("cross-node pinned lazy stream: %d %s", w.Code, w.Body.String())
	}
	plan, err := second.Plan(ctx, req)
	if err != nil || plan.ETag != detail.ETag || plan.Media.Container != detail.Media.Container || plan.Media.Size != detail.Media.Size {
		t.Fatalf("different source on second node: %+v %v", plan, err)
	}
	session, err := second.Open(ctx, frontend.OpenRequest{Viewer: friend, Device: device, Protocol: req.Protocol, PlanID: plan.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := second.MatchDetailSession(ctx, scope, detail.DetailSource, session.ID); err != nil {
		t.Fatalf("matching pinned session: %v", err)
	}
	if err := second.MatchDetailSession(ctx, bad, detail.DetailSource, session.ID); err == nil {
		t.Fatal("foreign device matched session")
	}
	if err := second.MatchDetailSession(ctx, scope, detail.DetailSource, "other-session"); err == nil {
		t.Fatal("unknown session matched handle")
	}
	if err := second.Stop(ctx, frontend.SessionAccess{Viewer: friend, Device: device, Protocol: req.Protocol}, session.ID); err != nil {
		t.Fatal(err)
	}
	// A node may still find the release ID after its underlying file changed.
	// It must reject that revision and never silently fall back to other bytes.
	other.engine.mu.Lock()
	other.engine.releases["A"] = mkv("A", 20000, time.Minute)
	other.engine.mu.Unlock()
	if _, err := second.Plan(ctx, req); !errors.Is(err, frontend.ErrSourceGone) {
		t.Fatalf("changed source accepted: %v", err)
	}
	w = httptest.NewRecorder()
	lazy.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	if w.Code != http.StatusNotFound || w.Body.String() != "Error processing request." || w.Header().Get("Content-Type") != "text/plain" {
		t.Fatalf("changed source stream: %d %q %q", w.Code, w.Body.String(), w.Header().Get("Content-Type"))
	}
	if len(other.m.Sessions()) != 0 {
		t.Fatal("failed revision check leaked a prepared source")
	}
	other.clock.Advance(2 * time.Hour)
	if _, err := second.ResolveDetailSource(ctx, scope, detail.DetailSource); !errors.Is(err, frontend.ErrSourceGone) {
		t.Fatalf("expired handle: %v", err)
	}
}
