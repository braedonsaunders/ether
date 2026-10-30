package sessions

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

type variantPlayback struct {
	*fakePlayback
	selected string
}

func (p *variantPlayback) PrepareSurface(ctx context.Context, v *core.User, k core.RatingKey, c core.ClientCaps, id string, exact bool) (*core.PlaySession, error) {
	if exact {
		return nil, core.ErrNotAvailable
	}
	return p.PrepareVariant(ctx, v, k, c, id)
}

func (p *variantPlayback) PrepareVariant(ctx context.Context, v *core.User, k core.RatingKey, c core.ClientCaps, id string) (*core.PlaySession, error) {
	p.selected = id
	return p.Prepare(ctx, v, k, c)
}
func TestBrokerVersionSelection(t *testing.T) {
	ctx := context.Background()
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	events := NewEventBus(nil, active)
	defer events.Close()
	pb := &variantPlayback{fakePlayback: &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 3}}
	b, err := NewBroker(BrokerDeps{Events: events, Playback: pb, Progress: &fakeProgress{}, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, k core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: k}, nil
	}, Variants: func(context.Context, *core.Viewer, *core.Title, core.ClientCaps) ([]core.ReleaseVariant, error) {
		return []core.ReleaseVariant{{ID: "cut", Info: core.MediaInfo{Container: "mkv", Estimated: true}}}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(ctx)
	req := frontend.PlanRequest{Viewer: &core.Viewer{ID: 1}, Device: core.Device{ID: "tv"}, Protocol: "silo", Item: core.MovieKey(2), Version: "cut", Intent: frontend.PlayIntent{Estimate: true, Options: core.DecideOptions{DirectPlay: true}}}
	p, err := b.Plan(ctx, req)
	if err != nil || !p.Estimated || p.Media.Container != "mkv" || pb.prepares != 0 {
		t.Fatalf("estimate %v %v", p, err)
	}
	req.Intent.Estimate = false
	_, err = b.Plan(ctx, req)
	if err != nil || pb.selected != "cut" {
		t.Fatalf("selection %q %v", pb.selected, err)
	}
	req.Version = "unoffered"
	if _, err = b.Plan(ctx, req); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("unoffered %v", err)
	}
}
