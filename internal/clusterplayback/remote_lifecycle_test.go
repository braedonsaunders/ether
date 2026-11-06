package clusterplayback

import (
	"context"
	"crypto/sha256"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/store"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/nodewire"
)

func lifecycleFixture(now *time.Time) (*RemotePlayback, *remotePreparation) {
	e := &remotePreparation{operation: "original-operation", owner: nodewire.Identity{Cluster: "cluster", NodeID: "source", Boot: "original-boot"}, activity: *now, done: make(chan struct{}), snapshot: &core.PlaySession{ID: "remote", UserID: 9, ClientID: "client", Key: core.MovieKey(1), PreparedOnly: true}}
	p := &RemotePlayback{o: RemotePlaybackOptions{IdleTimeout: 2 * time.Minute, Now: func() time.Time { return *now }}, entries: map[string]*remotePreparation{"remote": e}}
	return p, e
}

func TestRemoteIdleSealsWithoutProvingRetirement(t *testing.T) {
	now := time.Unix(1000, 0)
	p, e := lifecycleFixture(&now)
	life := remoteLifetime{p: p, e: e}
	now = now.Add(2 * time.Minute)
	p.expireLocked(e, now)
	if e.retiring {
		t.Fatal("expired at boundary, unlike Manager")
	}
	now = now.Add(time.Nanosecond)
	p.expireLocked(e, now)
	if !e.retiring || p.pending() != 1 {
		t.Fatal("idle source must seal while retaining responsibility")
	}
	select {
	case <-life.Done():
		t.Fatal("elapsed time proved retirement")
	default:
	}
	if life.Err() != ErrPending {
		t.Fatal("idle lifetime lost uncertain responsibility")
	}
	if p.MarkSurfaceOpened("remote") || p.MarkSurfacePrepared("remote") {
		t.Fatal("sealed entry revived")
	}
	p.Touch("remote")
	if e.owner.Boot != "original-boot" || e.operation != "original-operation" {
		t.Fatal("cleanup responsibility changed")
	}
}

func TestRemoteIdleFreshnessPinsAndDrain(t *testing.T) {
	for _, kind := range []string{"control", "media", "paused", "pin", "in-flight", "preparing"} {
		t.Run(kind, func(t *testing.T) {
			now := time.Unix(1000, 0)
			p, e := lifecycleFixture(&now)
			now = now.Add(3 * time.Minute)
			switch kind {
			case "control":
				p.PlaybackProgress(9, "client", core.MovieKey(1), time.Second, "playing")
			case "media":
				p.Touch("remote")
			case "paused":
				e.state = "paused"
			case "pin":
				e.pins = 1
			case "in-flight":
				e.uses = 1
			case "preparing":
				e.preparing = true
			}
			p.SetDraining(true)
			p.expireLocked(e, now)
			if e.retiring {
				t.Fatal("drain/idle cut protected source")
			}
			if _, _, err := p.PrepareSurfaceSource(context.Background(), nil, core.MovieKey(1), core.ClientCaps{}, "", false); err != ErrDenied {
				t.Fatal("drain admitted preparation")
			}
			if kind == "paused" {
				now = e.activity.Add(core.PausedSessionRetention + time.Nanosecond)
			} else {
				e.pins = 0
				e.uses = 0
				e.preparing = false
				now = e.activity.Add(p.o.IdleTimeout + time.Nanosecond)
			}
			p.expireLocked(e, now)
			if !e.retiring {
				t.Fatal("source never became idle after protection ended")
			}
		})
	}
}

func TestRemoteStopWaitsForAdmittedUse(t *testing.T) {
	now := time.Unix(1000, 0)
	p, e := lifecycleFixture(&now)
	e.uses = 1
	p.Stop("remote")
	if !e.retiring || e.retired || p.pending() != 1 {
		t.Fatal("stop dropped responsibility")
	}
	if err := p.OpenRemoteSource(context.Background(), "remote"); err != ErrDenied {
		t.Fatal("open admitted after stop")
	}
	p.endUse(e)
	if e.uses != 0 || p.pending() != 1 {
		t.Fatal("ending use proved retirement")
	}
}

func TestRemoteDelayedOpenCannotReviveStopOrCancellation(t *testing.T) {
	for _, cancelOpen := range []bool{false, true} {
		name := "stop"
		if cancelOpen {
			name = "cancel"
		}
		t.Run(name, func(t *testing.T) {
			now := time.Unix(1000, 0)
			p, e := lifecycleFixture(&now)
			entered, release := make(chan struct{}), make(chan struct{})
			p.r = &Runtime{o: Options{Endpoint: func(_ context.Context, owner nodewire.Identity) (string, error) {
				if owner != e.owner {
					t.Error("open switched exact owner")
				}
				close(entered)
				<-release
				return "", ErrDenied
			}}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- p.OpenRemoteSource(ctx, "remote") }()
			<-entered
			// Endpoint is blocked outside the broker mutex: Stop and Touch must finish.
			if cancelOpen {
				cancel()
			} else {
				p.Stop("remote")
			}
			p.Touch("remote")
			close(release)
			if err := <-done; err == nil {
				t.Fatal("delayed open succeeded")
			}
			if !e.retiring || e.retired || e.uses != 0 || p.pending() != 1 {
				t.Fatal("uncertain open lost exact responsibility")
			}
			select {
			case <-e.done:
				t.Fatal("cancel/stop proved completion")
			default:
			}
		})
	}
}

func TestRemoteMediaWriteRefreshesBeyondRequestAdmission(t *testing.T) {
	now := time.Unix(1000, 0)
	p, e := lifecycleFixture(&now)
	e.uses = 1
	now = now.Add(3 * time.Minute)
	w := remoteActivityWriter{ResponseWriter: httptest.NewRecorder(), playback: p, id: "remote"}
	if _, err := w.Write([]byte("media")); err != nil {
		t.Fatal(err)
	}
	p.endUse(e)
	p.expireLocked(e, now)
	if e.retiring {
		t.Fatal("successful long response expired at completion")
	}
	now = now.Add(p.o.IdleTimeout + time.Nanosecond)
	p.expireLocked(e, now)
	if !e.retiring {
		t.Fatal("disconnected media never expired")
	}
}

func TestRemoteCancelledOpenPreservesExistingClient(t *testing.T) {
	now := time.Unix(1000, 0)
	p, e := lifecycleFixture(&now)
	e.snapshot.PreparedOnly = false
	e.state = "paused"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.r = &Runtime{o: Options{Endpoint: func(context.Context, nodewire.Identity) (string, error) { cancel(); return "", ErrDenied }}}
	if p.OpenRemoteSource(ctx, "remote") == nil {
		t.Fatal("cancelled open accepted")
	}
	if e.retiring || e.retired || e.state != "paused" || p.pending() != 1 {
		t.Fatal("cancelled second open terminated existing paused client")
	}
}

func TestRemoteTickRequiresExactReceiptEvenWithSourceGone(t *testing.T) {
	for _, pinned := range []bool{false, true} {
		name := "idle"
		if pinned {
			name = "pinned"
		}
		t.Run(name, func(t *testing.T) {
			now := time.Unix(1000, 0)
			p, e := lifecycleFixture(&now)
			now = now.Add(3 * time.Minute)
			if pinned {
				e.pins = 1
			}
			// No activity is queued: this test isolates lifecycle reconciliation.
			e.sentActivity = e.activity
			db := store.OpenTest(t)
			self := nodewire.Identity{Cluster: "cluster", NodeID: "controller", Boot: "controller-boot"}
			calls := 0
			p.r = &Runtime{o: Options{DB: db, Self: self, MemberRole: func(context.Context, nodewire.Identity, config.NodeRole) error { return nil }, Endpoint: func(_ context.Context, owner nodewire.Identity) (string, error) {
				calls++
				if owner != e.owner {
					t.Error("retirement switched original owner")
				}
				return "", ErrDenied
			}}}
			_ = p.tick(context.Background())
			if e.retired || p.pending() != 1 {
				t.Fatal("missing receipt/owner absence settled responsibility")
			}
			select {
			case <-e.done:
				t.Fatal("idle expiry proved completion")
			default:
			}
			// Fixture a source-published immutable terminal receipt. Receipt creation
			// and publication proof are independently covered by the source tests.
			receipt := PreparationReceipt{Version: 1, Source: e.owner, Controller: self, OperationHash: preparationHash(e.operation), RequestHash: sha256.Sum256([]byte("request")), LifetimeID: "exact-lifetime", Kind: "source-retired", PlayID: "source-play", Revision: "revision"}
			insert := func(r PreparationReceipt) {
				raw, err := r.encode()
				if err != nil {
					t.Fatal(err)
				}
				args := append(preparationArgs(r.Source, r.Controller, r.OperationHash), raw)
				if _, err := db.Exec(`INSERT INTO clusterplayback_preparation_receipts(cluster,source,source_boot,controller,controller_boot,operation_hash,receipt) VALUES(?,?,?,?,?,?,?)`, args...); err != nil {
					t.Fatal(err)
				}
			}
			wrong := receipt
			wrong.Source.Boot = "replacement-boot"
			insert(wrong)
			_ = p.tick(context.Background())
			if e.retired {
				t.Fatal("replacement boot receipt cleared original responsibility")
			}
			insert(receipt)
			before := calls
			if err := p.tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			if !e.retired || p.pending() != 0 || calls != before {
				t.Fatal("exact durable receipt did not settle without old boot RPC")
			}
			select {
			case <-e.done:
			default:
				t.Fatal("receipt did not close lifetime")
			}
		})
	}
}
