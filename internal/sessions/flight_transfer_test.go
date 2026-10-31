package sessions

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/handles/capability"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestFlightTransferBlocksLegacyCleanupUntilRetirement(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	hash := lazyHash("flight-source")
	if err := insertLazyRow(db, hash, 7); err != nil {
		t.Fatal(err)
	}
	var expires int64
	if err := db.QueryRowContext(ctx, `SELECT expires FROM sessions_lazy_streams WHERE hash=?`, hash).Scan(&expires); err != nil {
		t.Fatal(err)
	}
	source := capability.Header{HeaderScope: capability.HeaderScope{Namespace: "lazy-stream", ViewerID: 7, Surface: "stremio"}, Digest: hash, ExpiresMillis: expires}
	target := capability.Header{HeaderScope: capability.HeaderScope{Namespace: brokerTransferNamespace, ViewerID: 7, Surface: "stremio", DeviceID: "tv"}, Digest: lazyHash("target-side"), ExpiresMillis: time.Now().Add(time.Hour).UnixMilli()}
	if _, err := prepareFlightTransfer(ctx, db, source, target, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM sessions_lazy_streams WHERE expires<=?`, expires); err == nil || !strings.Contains(err.Error(), "lazy capability retirement required") {
		t.Fatalf("legacy bulk delete = %v", err)
	}
	var streams int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions_lazy_streams`).Scan(&streams); err != nil || streams != 1 {
		t.Fatalf("streams=%d err=%v", streams, err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE capability_headers SET expires=1 WHERE namespace='lazy-stream' AND digest=?`, hash); err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	pruned, err := capability.PruneHeadersInTx(ctx, tx, "lazy-stream", time.Now().UnixMilli(), capability.HeaderBatchMax)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if pruned != 0 {
		t.Fatalf("prune removed a claimed header: %d", pruned)
	}
	if err = dropTransferHeader(ctx, db, target); err != nil {
		t.Fatal(err)
	}
	var targets int
	if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM capability_headers WHERE namespace=?`, brokerTransferNamespace).Scan(&targets); err != nil || targets != 0 {
		t.Fatalf("target header survived rollback: %d %v", targets, err)
	}
	if _, err = db.ExecContext(ctx, `DELETE FROM sessions_lazy_streams WHERE hash=?`, hash); err == nil {
		t.Fatal("source stream deleted while retirement claim remained")
	}
	if err = releaseRetirementClaim(ctx, db, source); err != nil {
		t.Fatal(err)
	}
	tx, err = db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = capability.PruneHeadersInTx(ctx, tx, "lazy-stream", time.Now().UnixMilli(), capability.HeaderBatchMax); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `DELETE FROM sessions_lazy_streams WHERE hash=?`, hash); err != nil {
		t.Fatal(err)
	}
}

func TestFlightTransferUnknownCommitRetainsSource(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	hash := lazyHash("unknown-source")
	if err := insertLazyRow(db, hash, 4); err != nil {
		t.Fatal(err)
	}
	var expires int64
	if err := db.QueryRowContext(ctx, `SELECT expires FROM sessions_lazy_streams WHERE hash=?`, hash).Scan(&expires); err != nil {
		t.Fatal(err)
	}
	source := capability.Header{HeaderScope: capability.HeaderScope{Namespace: "lazy-stream", ViewerID: 4, Surface: "stremio"}, Digest: hash, ExpiresMillis: expires}
	target := capability.Header{HeaderScope: capability.HeaderScope{Namespace: brokerTransferNamespace, ViewerID: 4, Surface: "stremio", DeviceID: "tv"}, Digest: lazyHash("unknown-target"), ExpiresMillis: time.Now().Add(time.Hour).UnixMilli()}
	prev := beforeTransferCommit
	beforeTransferCommit = func(tx *sql.Tx) error { return tx.Rollback() }
	t.Cleanup(func() { beforeTransferCommit = prev })
	_, err := prepareFlightTransfer(ctx, db, source, target, time.Now().UnixMilli())
	if !errors.Is(err, errTransferCommitUnknown) {
		t.Fatalf("commit error = %v", err)
	}
	var streams int
	if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions_lazy_streams WHERE hash=?`, hash).Scan(&streams); err != nil || streams != 1 {
		t.Fatalf("unknown commit changed source rows: %d %v", streams, err)
	}
}

type scriptOutcome struct {
	done chan struct{}
	err  error
}

func (s scriptOutcome) Done() <-chan struct{} { return s.done }
func (s scriptOutcome) Err() error {
	select {
	case <-s.done:
		return s.err
	default:
		return errors.New("pending")
	}
}

type ownedPlayback struct {
	*fakePlayback
	outcome scriptOutcome
	retired int
}

func (o *ownedPlayback) SourceRetirement(string) (core.SourceRetirement, bool) {
	return o.outcome, true
}
func (o *ownedPlayback) PrepareSurfaceSource(context.Context, *core.User, core.RatingKey, core.ClientCaps, string, bool) (*core.PlaySession, core.SourceRetirement, error) {
	return nil, nil, core.ErrNotAvailable
}
func (o *ownedPlayback) RetireSource(id string, _ core.SourceRetirement) error {
	o.retired++
	o.Stop(id)
	close(o.outcome.done)
	return nil
}

type ownedSession struct {
	stubHLS
	done chan struct{}
	err  error
}

func (s ownedSession) EncoderRetirement() core.EncoderRetirement { return s }
func (s ownedSession) RetireEncoder() error                      { return nil }
func (s ownedSession) EncoderDone() <-chan struct{}              { return s.done }
func (s ownedSession) EncoderErr() error {
	select {
	case <-s.done:
		return s.err
	default:
		return core.ErrEncoderPending
	}
}

type stubHLS struct{}

func (stubHLS) ID() string                                           { return "hls" }
func (stubHLS) ServePlaylist(http.ResponseWriter, *http.Request)     {}
func (stubHLS) ServeSegment(http.ResponseWriter, *http.Request, int) {}
func (stubHLS) SwapInput(context.Context, string, core.MediaInfo, time.Duration) error {
	return nil
}
func (stubHLS) LastAccess() time.Time { return time.Time{} }
func (stubHLS) Close() error          { return nil }

type ownedTranscoder struct {
	fakeBrokerTranscoder
	sess ownedSession
}

func (o ownedTranscoder) Session(string) (core.HLSSession, bool) { return o.sess, true }
func (o ownedTranscoder) Stop(string)                            {}

func TestBrokerTransferSurvivesUnverifiedInputAndFailedSource(t *testing.T) {
	for _, tc := range []struct {
		name    string
		encErr  error
		srcErr  error
		dropped bool
	}{
		{name: "unverified-input", encErr: core.ErrEncoderInputUnverified},
		{name: "source-close", srcErr: errors.New("close failed")},
		{name: "drained", dropped: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			db := store.OpenTest(t)
			playback := &ownedPlayback{fakePlayback: &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 4}, outcome: scriptOutcome{done: make(chan struct{}), err: tc.srcErr}}
			enc := ownedTranscoder{sess: ownedSession{done: make(chan struct{}), err: tc.encErr}}
			close(enc.sess.done)
			viewer := &core.Viewer{ID: 3}
			device := core.Device{ID: "tv"}
			active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
			bus := NewEventBus(nil, active)
			t.Cleanup(bus.Close)
			b, err := NewBroker(BrokerDeps{DB: db, Events: bus, Playback: playback, Transcoder: enc, Progress: &fakeProgress{}, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, k core.RatingKey) (*core.Title, error) {
				return &core.Title{Key: k}, nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { b.Close(context.Background()) })
			plan, err := b.Plan(ctx, frontend.PlanRequest{Viewer: viewer, Device: device, Protocol: "jellyfin", Item: core.MovieKey(1), Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true, DirectStream: true}}})
			if err != nil {
				t.Fatal(err)
			}
			opened, err := b.Open(ctx, frontend.OpenRequest{Viewer: viewer, Device: device, Protocol: "jellyfin", PlanID: plan.ID})
			if err != nil {
				t.Fatal(err)
			}
			var claims int
			if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM capability_claims WHERE purpose=?`, capability.RetirementOwnerClaim).Scan(&claims); err != nil || claims != 1 {
				t.Fatalf("claims after open=%d err=%v", claims, err)
			}
			if err = b.Stop(ctx, frontend.SessionAccess{Viewer: viewer, Device: device, Protocol: "jellyfin"}, opened.ID); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(2 * time.Second)
			for {
				if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM capability_claims WHERE purpose=?`, capability.RetirementOwnerClaim).Scan(&claims); err != nil {
					t.Fatal(err)
				}
				if tc.dropped && claims == 0 || !tc.dropped && claims == 1 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("claims=%d dropped=%v", claims, tc.dropped)
				}
				time.Sleep(10 * time.Millisecond)
			}
			if playback.retired != 1 {
				t.Fatalf("retire calls=%d", playback.retired)
			}
		})
	}
}

func TestBrokerUnknownTransferCommitDoesNotStopSource(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	playback := &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 2}
	viewer := &core.Viewer{ID: 5}
	device := core.Device{ID: "tv"}
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	bus := NewEventBus(nil, active)
	t.Cleanup(bus.Close)
	b, err := NewBroker(BrokerDeps{DB: db, Events: bus, Playback: playback, Progress: &fakeProgress{}, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, k core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: k}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close(context.Background()) })
	plan, err := b.Plan(ctx, frontend.PlanRequest{Viewer: viewer, Device: device, Protocol: "jellyfin", Item: core.MovieKey(9), Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true, DirectStream: true}}})
	if err != nil {
		t.Fatal(err)
	}
	prev := beforeTransferCommit
	beforeTransferCommit = func(tx *sql.Tx) error { return tx.Rollback() }
	defer func() { beforeTransferCommit = prev }()
	_, err = b.Open(ctx, frontend.OpenRequest{Viewer: viewer, Device: device, Protocol: "jellyfin", PlanID: plan.ID})
	if !errors.Is(err, errTransferCommitUnknown) {
		t.Fatalf("open err=%v", err)
	}
	playback.mu.Lock()
	left := len(playback.sessions)
	playback.mu.Unlock()
	if left != 1 {
		t.Fatalf("source sessions=%d", left)
	}
}
