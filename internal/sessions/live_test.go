package sessions

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
)

type fakeLive struct{ tunes, stops int }

func (*fakeLive) Allowed(_ context.Context, _ *core.Viewer, id string) error {
	if id != "channel" {
		return core.ErrNotFound
	}
	return nil
}
func (l *fakeLive) Tune(context.Context, *core.Viewer, string, core.ClientCaps, string) (string, error) {
	l.tunes++
	return "live-source", nil
}
func (*fakeLive) Serve(w http.ResponseWriter, _ *http.Request, _ *core.Viewer, _, tail, base string) {
	_, _ = w.Write([]byte(base + tail))
}
func (l *fakeLive) Stop(context.Context, *core.Viewer, string) error { l.stops++; return nil }

func TestLiveChannelEstimateOpenAndScopedMedia(t *testing.T) {
	ctx := context.Background()
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	events := NewEventBus(nil, active)
	defer events.Close()
	live := &fakeLive{}
	b, err := NewBroker(BrokerDeps{Live: live, Events: events, Playback: &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 2}, Progress: &fakeProgress{}, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, k core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: k}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(ctx)
	viewer, device := &core.Viewer{ID: 2}, core.Device{ID: "phone", Client: "account-secret"}
	req := frontend.PlanRequest{Viewer: viewer, Device: device, Protocol: "jellyfin", Channel: "channel", Intent: frontend.PlayIntent{Estimate: true}}
	plan, err := b.Plan(ctx, req)
	if err != nil || !plan.Estimated || live.tunes != 0 {
		t.Fatalf("estimate %+v tunes=%d err=%v", plan, live.tunes, err)
	}
	req.Item = core.MovieKey(1)
	if _, err = b.Plan(ctx, req); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("ambiguous target: %v", err)
	}
	req.Item = 0
	req.Intent.Estimate = false
	plan, err = b.Plan(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	s, err := b.Open(ctx, frontend.OpenRequest{Viewer: viewer, Device: device, Protocol: "jellyfin", PlanID: plan.ID})
	if err != nil || s.Channel != "channel" || live.tunes != 1 {
		t.Fatalf("open %+v %v", s, err)
	}
	a := frontend.SessionAccess{Viewer: viewer, Device: device, Protocol: "jellyfin"}
	path, err := b.StartStream(ctx, a, s.ID, frontend.StreamRequest{Format: frontend.StreamHLS})
	if err != nil || strings.Contains(path, "account-secret") {
		t.Fatalf("path %q %v", path, err)
	}
	out := httptest.NewRecorder()
	b.ServeMedia(out, httptest.NewRequest(http.MethodGet, path, nil))
	if out.Code != 200 || !strings.Contains(out.Body.String(), "index.m3u8") {
		t.Fatalf("media %d %s", out.Code, out.Body.String())
	}
	if !b.StopPlayback(ctx, "live-source") || live.stops != 1 {
		t.Fatal("owner stop did not close live stream")
	}
}

func TestSiloLiveMasterRewritesToThePublicPlaylist(t *testing.T) {
	ctx := context.Background()
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	events := NewEventBus(nil, active)
	defer events.Close()
	b, err := NewBroker(BrokerDeps{Live: &fakeLive{}, Events: events, Playback: &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 2}, Progress: &fakeProgress{}, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, k core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: k}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(ctx)
	viewer, device := &core.Viewer{ID: 2}, core.Device{ID: "tv"}
	plan, err := b.Plan(ctx, frontend.PlanRequest{Viewer: viewer, Device: device, Protocol: "silo", Channel: "channel", Intent: frontend.PlayIntent{ClientSessionID: "11111111-1111-4111-8111-111111111111"}})
	if err != nil {
		t.Fatal(err)
	}
	s, err := b.Open(ctx, frontend.OpenRequest{Viewer: viewer, Device: device, Protocol: "silo", PlanID: plan.ID})
	if err != nil {
		t.Fatal(err)
	}
	access := frontend.SessionAccess{Viewer: viewer, Device: device, Protocol: "silo"}
	if _, err = b.StartStream(ctx, access, s.ID, frontend.StreamRequest{Format: frontend.StreamHLS}); err != nil {
		t.Fatal(err)
	}
	out := httptest.NewRecorder()
	b.ServeSessionMedia(out, httptest.NewRequest(http.MethodGet, "/api/v2/playback/transcode/"+s.ID+"/master.m3u8", nil), access, s.ID, "master.m3u8")
	want := "/api/v2/playback/transcode/" + s.ID + "/live/index.m3u8"
	if out.Code != 200 || !strings.Contains(out.Body.String(), want) || strings.Contains(out.Body.String(), "/sessions/media/") {
		t.Fatalf("master %d %s", out.Code, out.Body.String())
	}
}
