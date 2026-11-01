package sessions

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

type revisionSource struct {
	*bytes.Reader
	id, name string
	size     int64
}

func (s *revisionSource) Close() error      { return nil }
func (s *revisionSource) ReleaseID() string { return s.id }
func (s *revisionSource) Name() string      { return s.name }
func (s *revisionSource) Size() int64       { return s.size }

type revisionPlayback struct {
	*fakePlayback
	source *revisionSource
}

func (p *revisionPlayback) PrepareSurface(ctx context.Context, v *core.User, k core.RatingKey, c core.ClientCaps, release string, exact bool) (*core.PlaySession, error) {
	if release != "" || exact {
		return nil, core.ErrNotAvailable
	}
	return p.Prepare(ctx, v, k, c)
}

func (p *revisionPlayback) Prepare(ctx context.Context, v *core.User, k core.RatingKey, c core.ClientCaps) (*core.PlaySession, error) {
	s, err := p.fakePlayback.Prepare(ctx, v, k, c)
	if s != nil {
		s.Source = p.source
	}
	return s, err
}

func TestSourceRevisionIsStableOpaqueAndChangesWithSource(t *testing.T) {
	source := &revisionSource{Reader: bytes.NewReader(nil), id: "release-secret", name: "episode.mkv", size: 100}
	s := &core.PlaySession{Key: core.EpisodeKey(1, 1, 1), Source: source}
	first := sourceRevision(s)
	if first == "" || first != sourceRevision(s) || strings.Contains(first, "secret") || strings.Contains(first, "episode") {
		t.Fatalf("unstable or identifying revision %q", first)
	}
	source.id = "other-release"
	if sourceRevision(s) == first {
		t.Fatal("release change retained revision")
	}
	source.id = "release-secret"
	source.size = 101
	if sourceRevision(s) == first {
		t.Fatal("source size change retained revision")
	}
	source.id = "local:mutable"
	if sourceRevision(s) != "" {
		t.Fatal("mutable local file was assigned an authoritative revision")
	}
	if sourceRevision(&core.PlaySession{}) != "" {
		t.Fatal("unknown source assigned a revision")
	}
}

func TestOpenRejectsChangedPlanSourceRevision(t *testing.T) {
	ctx := context.Background()
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	events := NewEventBus(nil, active)
	defer events.Close()
	source := &revisionSource{Reader: bytes.NewReader(nil), id: "release-one", name: "movie.mkv", size: 100}
	pb := &revisionPlayback{fakePlayback: &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 2}, source: source}
	b, err := NewBroker(BrokerDeps{Events: events, Playback: pb, Transcoder: fakeBrokerTranscoder{}, Progress: &fakeProgress{}, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, k core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: k}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(ctx)
	v, d := &core.Viewer{ID: 1}, core.Device{ID: "tv"}
	plan, err := b.Plan(ctx, frontend.PlanRequest{Viewer: v, Device: d, Protocol: "jellyfin", Item: core.MovieKey(1), Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true}}})
	if err != nil || plan.ETag == "" {
		t.Fatalf("plan %+v %v", plan, err)
	}
	source.id = "release-two"
	if _, err = b.Open(ctx, frontend.OpenRequest{Viewer: v, Device: d, Protocol: "jellyfin", PlanID: plan.ID}); !errors.Is(err, core.ErrNotAvailable) {
		t.Fatalf("stale plan opened: %v", err)
	}
}
