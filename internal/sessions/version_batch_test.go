package sessions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/handles/capability"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestVersionHandleBatchValidatesOnceAndIsAtomic(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	bus := NewEventBus(nil, active)
	defer bus.Close()
	lookups, variants := 0, 0
	b, err := NewBroker(BrokerDeps{Events: bus, Playback: &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 1}, Progress: &fakeProgress{}, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, k core.RatingKey) (*core.Title, error) {
		lookups++
		return &core.Title{Key: k}, nil
	}, Variants: func(context.Context, *core.Viewer, *core.Title, core.ClientCaps) ([]core.ReleaseVariant, error) {
		variants++
		return []core.ReleaseVariant{{ID: "one"}, {ID: "two"}, {ID: "three"}}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(ctx)
	service, err := NewVersionHandles(db, b, "jellyfin")
	if err != nil {
		t.Fatal(err)
	}
	service.(*versionHandles).limits = capability.Limits{PerScope: 2, PerShard: 2}
	batch, ok := service.(frontend.VersionHandleBatch)
	if !ok {
		t.Fatal("batch capability missing")
	}
	scope := frontend.VersionScope{Viewer: &core.Viewer{ID: 1}, Device: core.Device{ID: "tv"}, Protocol: "jellyfin"}
	item := core.MovieKey(7)
	if _, err = batch.IssueBatch(ctx, scope, item, []string{"one", "missing"}); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("invalid batch: %v", err)
	}
	var count int
	if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions_version_handles`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial batch: %d %v", count, err)
	}
	lookups, variants = 0, 0
	got, err := batch.IssueBatch(ctx, scope, item, []string{"two", "one", "one"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["one"] == "" || got["two"] == "" || got["one"] == got["two"] || lookups != 1 || variants != 1 {
		t.Fatalf("batch=%v lookups=%d variants=%d", got, lookups, variants)
	}
	again, err := batch.IssueBatch(ctx, scope, item, []string{"one", "two"})
	if err != nil || got["one"] != again["one"] || got["two"] != again["two"] {
		t.Fatalf("unstable batch: %v %v", again, err)
	}
	if _, err = batch.IssueBatch(ctx, scope, item, make([]string, 129)); !errors.Is(err, core.ErrNotAvailable) {
		t.Fatalf("unbounded batch: %v", err)
	}
	if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM capability_headers WHERE namespace='sessions.version'`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("headers %d %v", count, err)
	}
	var capacity *capability.CapacityError
	if _, err = batch.IssueBatch(ctx, scope, item, []string{"three"}); !errors.As(err, &capacity) {
		t.Fatalf("quota admission %v", err)
	}
	if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions_version_handles`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("mapping survived admission failure %d %v", count, err)
	}

}

func TestVersionHeaderRequirementIsSiloOnly(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	b := &Broker{deps: BrokerDeps{DB: db, Now: time.Now, ActiveViewer: func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }, Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, key core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: key}, nil
	}, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, Variants: func(context.Context, *core.Viewer, *core.Title, core.ClientCaps) ([]core.ReleaseVariant, error) {
		return []core.ReleaseVariant{{ID: "offered"}}, nil
	}}}
	for _, surface := range []frontend.Protocol{"silo", "jellyfin", "emby", "plex"} {
		api, err := NewVersionHandles(db, b, surface)
		if err != nil {
			t.Fatal(err)
		}
		scope := frontend.VersionScope{Viewer: &core.Viewer{ID: 1}, Device: core.Device{ID: "tv"}, Protocol: surface}
		handle, err := api.Issue(ctx, scope, core.MovieKey(1), "offered")
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256([]byte(handle))
		if _, err = db.Exec(`DELETE FROM capability_headers WHERE namespace='sessions.version' AND digest=?`, hex.EncodeToString(digest[:])); err != nil {
			t.Fatal(err)
		}
		_, err = api.Resolve(ctx, scope, handle)
		if surface == "silo" && err == nil {
			t.Fatal("Silo accepted a headerless mapping")
		}
		if surface != "silo" && err != nil {
			t.Fatalf("%s changed pre-header resolution: %v", surface, err)
		}
	}
}
