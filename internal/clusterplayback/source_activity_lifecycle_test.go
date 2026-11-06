package clusterplayback

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/nodewire"
	"github.com/braedonsaunders/ether/internal/playback"
)

func TestSourceActivityNeverOverwritesNewerOrSealedState(t *testing.T) {
	manager, err := playback.New(playback.Deps{Catalog: fixtureCatalog{}, Resolver: fixtureResolver{}, Engine: fixtureEngine{}, Transcoder: &fixtureEncoder{}, SelectProfile: func(core.ClientCaps, *core.User) core.QualityProfile { return core.QualityProfile{} }}, config.Defaults(t.TempDir()), playback.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	ps, life, err := manager.PrepareSurfaceSource(context.Background(), &core.User{ID: 9}, core.MovieKey(1), core.ClientCaps{ClientID: "activity"}, "", false)
	if err != nil {
		t.Fatal(err)
	}
	peer := nodewire.Identity{Cluster: "cluster", NodeID: "controller", Boot: "boot"}
	prepared := PreparedSource{Owner: nodewire.Identity{Cluster: "cluster", NodeID: "source", Boot: "source-boot"}, PlayID: ps.ID, Revision: core.SourceRevision(ps)}
	entry := &preparation{activityMu: &sync.Mutex{}, activityVersion: new(uint64), peer: peer, viewer: 9, finished: true, life: life, result: prepared}
	r := &Runtime{preparations: map[string]*preparation{"controller/boot/op": entry}, o: Options{Playback: manager, MemberRole: func(context.Context, nodewire.Identity, config.NodeRole) error { return nil }, ActiveViewer: func(context.Context, int64) (*core.Viewer, error) { return &core.Viewer{ID: 9}, nil }, SourcePolicy: func(context.Context, *core.Viewer, *core.PlaySession) error { return nil }}}
	activity := func(ctx context.Context, version uint64, state string) error {
		_, err := r.sourceActivity(ctx, peer, sourceRequest{Op: "prepare-activity", Prepare: &PrepareRequest{Operation: "op"}, ActivityVersion: version, Position: time.Second, State: state})
		return err
	}
	if err := activity(context.Background(), 2, "paused"); err != nil {
		t.Fatal(err)
	}
	for _, version := range []uint64{1, 0, 2} {
		if err := activity(context.Background(), version, "playing"); err != nil {
			t.Fatal(err)
		}
	}
	snap, _ := manager.Session(ps.ID)
	if snap.ViewerState != "paused" {
		t.Fatal("stale/duplicate/legacy activity overwrote paused state")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if activity(ctx, 3, "playing") == nil {
		t.Fatal("cancelled activity accepted")
	}
	r.mu.Lock()
	entry.retiring = true
	r.mu.Unlock()
	if activity(context.Background(), 4, "playing") == nil {
		t.Fatal("activity admitted after retirement seal")
	}
	snap, _ = manager.Session(ps.ID)
	if snap.ViewerState != "paused" {
		t.Fatal("cancelled/sealed mutation changed source")
	}
}
