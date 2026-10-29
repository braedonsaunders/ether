package sessions

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/transcode"
)

type capacityTranscoder struct {
	fakeBrokerTranscoder
	mu     sync.Mutex
	active map[string]core.HLSSession
}

func (*capacityTranscoder) Decide(core.MediaInfo, core.ClientCaps, core.DecideOptions) core.Decision {
	return core.Decision{Kind: core.DecisionTranscode, VideoCodec: "h264", AudioCodec: "aac", Container: "mpegts"}
}
func (t *capacityTranscoder) StartHLS(_ context.Context, r core.HLSRequest) (core.HLSSession, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.active) > 0 {
		return nil, transcode.ErrTooManyTranscodes
	}
	s := &fakeMediaSession{id: r.SessionID}
	t.active[r.SessionID] = s
	return s, nil
}
func (t *capacityTranscoder) Session(id string) (core.HLSSession, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.active[id]
	return s, ok
}
func (t *capacityTranscoder) Stop(id string) { t.mu.Lock(); defer t.mu.Unlock(); delete(t.active, id) }

func TestCrossProtocolTranscodeCapacity(t *testing.T) {
	ctx := context.Background()
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	events := NewEventBus(nil, active)
	defer events.Close()
	tc := &capacityTranscoder{active: map[string]core.HLSSession{}}
	b, err := NewBroker(BrokerDeps{Events: events, Playback: &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 2}, Transcoder: tc, Progress: &fakeProgress{}, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, k core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: k}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(ctx)
	if _, err = tc.StartHLS(ctx, core.HLSRequest{SessionID: "plex"}); err != nil {
		t.Fatal(err)
	}
	viewer, device := &core.Viewer{ID: 2}, core.Device{ID: "tv"}
	p, err := b.Plan(ctx, frontend.PlanRequest{Viewer: viewer, Device: device, Protocol: "jellyfin", Item: core.MovieKey(1)})
	if err != nil {
		t.Fatal(err)
	}
	s, err := b.Open(ctx, frontend.OpenRequest{Viewer: viewer, Device: device, Protocol: "jellyfin", PlanID: p.ID})
	if err != nil {
		t.Fatal(err)
	}
	access := frontend.SessionAccess{Viewer: viewer, Device: device, Protocol: "jellyfin"}
	_, err = b.StartStream(ctx, access, s.ID, frontend.StreamRequest{Format: frontend.StreamHLS})
	var capacity *frontend.TranscodeCapacityError
	if !errors.Is(err, frontend.ErrTranscodeCapacity) || !errors.As(err, &capacity) || capacity.RetryAfter <= 0 || !errors.Is(err, transcode.ErrTooManyTranscodes) {
		t.Fatalf("neutral capacity error: %v", err)
	}
	tc.Stop("plex")
	if _, err = b.StartStream(ctx, access, s.ID, frontend.StreamRequest{Format: frontend.StreamHLS}); err != nil {
		t.Fatal(err)
	}
	if _, err = tc.StartHLS(ctx, core.HLSRequest{SessionID: "plex"}); !errors.Is(err, transcode.ErrTooManyTranscodes) {
		t.Fatalf("Plex did not share broker capacity: %v", err)
	}
}
