package sessions

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestSiloInstallationBindingSurvivesSharedSessionRow(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	const installation = "11111111-1111-4111-8111-111111111111"
	writer := clusterStore{db: db, node: "a"}
	reader := clusterStore{db: db, node: "b"}
	session := frontend.Session{ID: "session", ViewerID: 1, Protocol: "silo", Device: core.Device{ID: "phone"}, Item: core.MovieKey(1), ClientSessionID: installation, StartedAt: time.Now()}
	if err := writer.put(ctx, session, "source", core.Viewer{ID: 1}, time.Now()); err != nil {
		t.Fatal(err)
	}
	rows, err := reader.list(ctx, time.Now().Add(-time.Minute), map[string]bool{"a": true})
	if err != nil || len(rows) != 1 || rows[0].session.ClientSessionID != installation {
		t.Fatalf("shared binding %+v %v", rows, err)
	}
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	events := NewEventBus(nil, active)
	defer events.Close()
	b, err := NewBroker(BrokerDeps{Events: events, Playback: &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 2}, Progress: &fakeProgress{}, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, k core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: k}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(ctx)
	req := frontend.PlanRequest{Viewer: &core.Viewer{ID: 1}, Device: core.Device{ID: "phone"}, Protocol: "silo", Item: core.MovieKey(1), Intent: frontend.PlayIntent{Estimate: true, ClientSessionID: "account-credential-secret"}}
	if _, err = b.Plan(ctx, req); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("malformed identifier accepted: %v", err)
	}
	req.Intent.ClientSessionID = installation
	if _, err = b.Plan(ctx, req); err != nil {
		t.Fatalf("canonical installation rejected: %v", err)
	}
}
