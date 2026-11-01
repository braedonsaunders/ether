package sessions

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

type policyLive struct {
	allowed bool
	served  int
}

func (l *policyLive) Allowed(context.Context, *core.Viewer, string) error {
	if !l.allowed {
		return core.ErrUnauthorized
	}
	return nil
}
func (*policyLive) Tune(context.Context, *core.Viewer, string, core.ClientCaps, string) (string, error) {
	return "live", nil
}
func (l *policyLive) Serve(w http.ResponseWriter, _ *http.Request, _ *core.Viewer, _, _, _ string) {
	l.served++
	_, _ = w.Write([]byte("live"))
}
func (*policyLive) Stop(context.Context, *core.Viewer, string) error { return nil }

func TestMediaRechecksCurrentPolicyAndScopedDevice(t *testing.T) {
	ctx := context.Background()
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	events := NewEventBus(nil, active)
	defer events.Close()
	allowed := true
	canPlay := true
	live := &policyLive{allowed: true}
	playback := &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 4}
	transcoder := &fakeMediaTranscoder{sessions: map[string]*fakeMediaSession{}}
	key := core.MovieKey(42)
	lookupCalls := 0
	lookup := func(_ context.Context, v *core.Viewer, d core.Device, k core.RatingKey) (*core.Title, error) {
		if !allowed && v.ID == 2 {
			return nil, core.ErrUnauthorized
		}
		if d.ID == "" {
			return nil, errors.New("missing device")
		}
		return &core.Title{Key: k}, nil
	}
	broker, err := NewBroker(BrokerDeps{Events: events, Playback: playback, Transcoder: transcoder, Live: live, Progress: &fakeProgress{}, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return canPlay }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Lookup: func(ctx context.Context, v *core.Viewer, d core.Device, k core.RatingKey) (*core.Title, error) {
		lookupCalls++
		return lookup(ctx, v, d, k)
	}, MediaLookup: lookup, ExtractSubtitle: func(_ context.Context, _ string, _ int, _ string, w io.Writer) error {
		_, e := io.WriteString(w, "WEBVTT\n")
		return e
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close(ctx)
	viewer, device := &core.Viewer{ID: 2}, core.Device{ID: "device"}
	req := frontend.PlanRequest{Viewer: viewer, Device: device, Protocol: "silo", Item: key, Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true, DirectStream: true}}}
	plan, err := broker.Plan(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	session, err := broker.Open(ctx, frontend.OpenRequest{Viewer: viewer, Device: device, Protocol: "silo", PlanID: plan.ID})
	if err != nil {
		t.Fatal(err)
	}
	access := frontend.SessionAccess{Viewer: viewer, Device: device, Protocol: "silo"}
	lookupAfterOpen := lookupCalls
	direct, err := broker.StartStream(ctx, access, session.ID, frontend.StreamRequest{Format: frontend.StreamDirect})
	if err != nil {
		t.Fatal(err)
	}
	playback.mu.Lock()
	for _, ps := range playback.sessions {
		ps.Info.Streams = []core.StreamInfo{{Index: 3, Type: "subtitle", Codec: "srt"}}
	}
	playback.mu.Unlock()
	subtitle, err := broker.SubtitleURL(ctx, access, session.ID, 3, "vtt")
	if err != nil {
		t.Fatal(err)
	}
	check := func(method, path string, a frontend.SessionAccess, want int) {
		t.Helper()
		w := httptest.NewRecorder()
		r := httptest.NewRequest(method, path, nil)
		r.Header.Set("Range", "bytes=0-3")
		BindSurface(broker, "silo").(frontend.ScopedMedia).ServeMediaAccess(w, r, a)
		if w.Code != want {
			t.Fatalf("%s %s: got %d want %d", method, path, w.Code, want)
		}
	}
	check("GET", direct, access, 200)
	check("HEAD", direct, access, 200)
	check("GET", subtitle, access, 200)
	bad := access
	bad.Device.ID = "other"
	check("GET", direct, bad, 404)
	bad = access
	bad.Protocol = "jellyfin"
	check("GET", direct, bad, 404)
	bad = access
	bad.Viewer = &core.Viewer{ID: 3}
	check("GET", direct, bad, 404)
	bad = access
	bad.Owner = true
	check("GET", direct, bad, 404)
	allowed = false
	check("GET", direct, access, 404)
	check("HEAD", direct, access, 404)
	check("GET", subtitle, access, 404)
	if lookupCalls != lookupAfterOpen {
		t.Fatalf("media operations used network-capable lookup %d times", lookupCalls-lookupAfterOpen)
	}
	otherViewer := &core.Viewer{ID: 3}
	otherDevice := core.Device{ID: "other-viewer-device"}
	otherPlan, err := broker.Plan(ctx, frontend.PlanRequest{Viewer: otherViewer, Device: otherDevice, Protocol: "silo", Item: key, Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true}}})
	if err != nil {
		t.Fatal(err)
	}
	otherSession, err := broker.Open(ctx, frontend.OpenRequest{Viewer: otherViewer, Device: otherDevice, Protocol: "silo", PlanID: otherPlan.ID})
	if err != nil {
		t.Fatal(err)
	}
	otherAccess := frontend.SessionAccess{Viewer: otherViewer, Device: otherDevice, Protocol: "silo"}
	otherPath, err := broker.StartStream(ctx, otherAccess, otherSession.ID, frontend.StreamRequest{Format: frontend.StreamDirect})
	if err != nil {
		t.Fatal(err)
	}
	check("GET", otherPath, otherAccess, 200)
	if _, err := broker.StartStream(ctx, access, session.ID, frontend.StreamRequest{Format: frontend.StreamHLS}); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("revoked start: %v", err)
	}
	if _, err := broker.SubtitleURL(ctx, access, session.ID, 3, "vtt"); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("revoked subtitle issuance: %v", err)
	}
	if err := broker.Report(ctx, access, session.ID, frontend.Timeline{State: "playing"}); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("revoked progress: %v", err)
	}
	allowed = true
	hls, err := broker.StartStream(ctx, access, session.ID, frontend.StreamRequest{Format: frontend.StreamHLS})
	if err != nil {
		t.Fatal(err)
	}
	base := strings.TrimSuffix(hls, "master.m3u8")
	check("GET", base+"index.m3u8", access, 200)
	check("GET", base+"00000.ts", access, 200)
	allowed = false
	check("GET", base+"index.m3u8", access, 404)
	check("GET", base+"00000.ts", access, 404)
	// Capability-only mounts also enforce the current policy.
	w := httptest.NewRecorder()
	broker.ServeMedia(w, httptest.NewRequest("GET", base+"00000.ts", nil))
	if w.Code != 404 {
		t.Fatalf("unscoped revoked grant: %d", w.Code)
	}
	allowed = true
	dash, err := broker.StartStream(ctx, access, session.ID, frontend.StreamRequest{Format: frontend.StreamDASH})
	if err != nil {
		t.Fatal(err)
	}
	check("GET", dash, access, 200)
	dashBase := strings.TrimSuffix(dash, "manifest.mpd") + "session/" + session.ID + "/0/"
	check("GET", dashBase+"header", access, 200)
	check("GET", dashBase+"00000.ts", access, 200)
	allowed = false
	check("GET", dash, access, 404)
	check("GET", dashBase+"header", access, 404)
	check("GET", dashBase+"00000.ts", access, 404)
	canPlay = false
	if err := broker.Stop(ctx, access, session.ID); err != nil {
		t.Fatalf("stop after revocation: %v", err)
	}
	canPlay = true
	allowed = true
	liveReq := frontend.PlanRequest{Viewer: viewer, Device: core.Device{ID: "live-device"}, Protocol: "silo", Channel: "channel"}
	lp, err := broker.Plan(ctx, liveReq)
	if err != nil {
		t.Fatal(err)
	}
	ls, err := broker.Open(ctx, frontend.OpenRequest{Viewer: viewer, Device: liveReq.Device, Protocol: "silo", PlanID: lp.ID})
	if err != nil {
		t.Fatal(err)
	}
	la := frontend.SessionAccess{Viewer: viewer, Device: liveReq.Device, Protocol: "silo"}
	path, err := broker.StartStream(ctx, la, ls.ID, frontend.StreamRequest{Format: frontend.StreamHLS})
	if err != nil {
		t.Fatal(err)
	}
	check("GET", path, la, 200)
	live.allowed = false
	check("GET", path, la, 404)
	check("HEAD", path, la, 404)
	if err := broker.Report(ctx, la, ls.ID, frontend.Timeline{State: "stopped"}); err != nil {
		t.Fatalf("stopped report after channel revocation: %v", err)
	}
	if live.served != 1 {
		t.Fatalf("live backend served after revocation: %d", live.served)
	}
}
