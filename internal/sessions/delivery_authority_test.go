package sessions

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/store"
)

type rememberingPlayback struct {
	*fakePlayback
	release string
	exact   bool
}

func (p *rememberingPlayback) PrepareSurface(ctx context.Context, u *core.User, key core.RatingKey, c core.ClientCaps, release string, exact bool) (*core.PlaySession, error) {
	p.release, p.exact = release, exact
	ps, err := p.fakePlayback.Prepare(ctx, u, key, c)
	if err != nil {
		return nil, err
	}
	if release != "" {
		ps.Resolution = &core.Resolution{Release: core.Release{ID: release}}
	}
	return ps, nil
}

func TestDeliveryAuthoritySurvivesRestartAndRejectsStaleSession(t *testing.T) {
	testDeliveryAuthority(t, store.OpenTest(t))
}

func TestDeliveryAuthorityPostgres(t *testing.T) {
	raw := os.Getenv(store.TestPostgresEnv)
	if raw == "" {
		t.Skip("set " + store.TestPostgresEnv + " to run PostgreSQL tests")
	}
	db, err := store.OpenTestPostgres(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	testDeliveryAuthority(t, db)
}

func testDeliveryAuthority(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	viewer := &core.Viewer{ID: 8}
	device := core.Device{ID: "living-room"}
	item := core.MovieKey(42)
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	newBroker := func(playback *rememberingPlayback) *Broker {
		t.Helper()
		bus := NewEventBus(nil, active)
		t.Cleanup(bus.Close)
		b, err := NewBroker(BrokerDeps{
			DB: db, Events: bus, Playback: playback, Transcoder: fakeBrokerTranscoder{}, Progress: &fakeProgress{},
			ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true },
			BitrateLimit: func(*core.Viewer) int { return 0 },
			OnTimeline:   func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {},
			Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, key core.RatingKey) (*core.Title, error) {
				return &core.Title{Key: key}, nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { b.Close(context.Background()) })
		return b
	}
	openStream := func(b *Broker, client string) string {
		t.Helper()
		plan, err := b.Plan(ctx, frontend.PlanRequest{Viewer: viewer, Device: device, Protocol: "jellyfin", Item: item, Intent: frontend.PlayIntent{ClientSessionID: client, Options: core.DecideOptions{DirectPlay: true}}})
		if err != nil {
			t.Fatal(err)
		}
		session, err := b.Open(ctx, frontend.OpenRequest{Viewer: viewer, Device: device, Protocol: "jellyfin", PlanID: plan.ID})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = b.StartStream(ctx, frontend.SessionAccess{Viewer: viewer, Device: device, Protocol: "jellyfin"}, session.ID, frontend.StreamRequest{Format: frontend.StreamDirect}); err != nil {
			t.Fatal(err)
		}
		return session.ID
	}
	firstPlayback := &rememberingPlayback{fakePlayback: &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 4}}
	first := newBroker(firstPlayback)
	originalSession := openStream(first, "client-a")
	original, ok, err := lookupDeliveryAuthority(ctx, db, viewer.ID, int64(item), "jellyfin", device.ID)
	if err != nil || !ok || original.SessionID != "client-a" || original.ReleaseID == "" || original.Token <= 0 {
		t.Fatalf("published authority %+v ok=%v err=%v", original, ok, err)
	}
	restartedPlayback := &rememberingPlayback{fakePlayback: &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 4}}
	restarted := newBroker(restartedPlayback)
	if _, err = restarted.Plan(ctx, frontend.PlanRequest{Viewer: viewer, Device: device, Protocol: "jellyfin", Item: item, Intent: frontend.PlayIntent{ClientSessionID: "client-a", Options: core.DecideOptions{DirectPlay: true}}}); err != nil {
		t.Fatal(err)
	}
	if !restartedPlayback.exact || restartedPlayback.release != original.ReleaseID {
		t.Fatalf("restart prepared release %q exact %v, authority %q", restartedPlayback.release, restartedPlayback.exact, original.ReleaseID)
	}
	openStream(first, "client-b")
	newer, ok, err := lookupDeliveryAuthority(ctx, db, viewer.ID, int64(item), "jellyfin", device.ID)
	if err != nil || !ok || newer.SessionID != "client-b" || newer.Token <= original.Token {
		t.Fatalf("newer authority %+v ok=%v err=%v", newer, ok, err)
	}
	_, err = first.StartStream(ctx, frontend.SessionAccess{Viewer: viewer, Device: device, Protocol: "jellyfin"}, originalSession, frontend.StreamRequest{Format: frontend.StreamDirect})
	if err == nil {
		t.Fatal("stale session started over the newer authority")
	}
	stale := original
	stale.Generation++
	published, err := publishDeliveryAuthority(ctx, db, stale, time.Now())
	if err != nil || published {
		t.Fatalf("stale token publish = %v err=%v", published, err)
	}
	kept, ok, err := lookupDeliveryAuthority(ctx, db, viewer.ID, int64(item), "jellyfin", device.ID)
	if err != nil || !ok || kept.SessionID != "client-b" || kept.Token != newer.Token || kept.ReleaseID != newer.ReleaseID {
		t.Fatalf("authority after stale publish %+v ok=%v err=%v", kept, ok, err)
	}
}
