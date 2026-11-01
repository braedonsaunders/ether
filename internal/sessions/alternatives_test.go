package sessions

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

func TestPlanAlternativesReuseOneResolvedSource(t *testing.T) {
	ctx := context.Background()
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	events := NewEventBus(nil, active)
	defer events.Close()
	playback := &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 2}
	b, err := NewBroker(BrokerDeps{Events: events, Playback: playback, Transcoder: profileTranscoder{}, Progress: &fakeProgress{}, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, k core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: k}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(ctx)
	allow := false
	plan, err := b.Plan(ctx, frontend.PlanRequest{Viewer: &core.Viewer{ID: 1}, Device: core.Device{ID: "d"}, Protocol: "jellyfin", Item: core.MovieKey(1), Intent: frontend.PlayIntent{AllowTranscoding: &allow, Options: core.DecideOptions{DirectPlay: true, DirectStream: true, AudioStream: -1, SubtitleStream: -1}}})
	if err != nil || !plan.DirectPlay || !plan.DirectStream || plan.Transcoding || playback.prepares != 1 {
		t.Fatalf("alternatives %+v resolves=%d err=%v", plan, playback.prepares, err)
	}
	source := playback.Sessions()[0]
	direct, remux, encode := b.planAlternatives(source, frontend.PlanRequest{Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true, DirectStream: true, AudioStream: -1, SubtitleStream: -1}}})
	if !direct || !remux || !encode {
		t.Fatalf("allowed alternatives direct=%v remux=%v transcode=%v", direct, remux, encode)
	}
}
