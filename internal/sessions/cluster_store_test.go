package sessions

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/clusterbus"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestClusterCleanupPreservesPlexRows(t *testing.T) {
	db := store.OpenTest(t)
	ctx := context.Background()
	for _, protocol := range []any{nil, "plex"} {
		id := "legacy"
		if protocol != nil {
			id = "plex"
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO plex_sessions(id,node_id,user_id,session_key,data,updated_at,protocol) VALUES(?,?,?,?,?,?,?)`, id, "node", 1, 1, `{}`, 0, protocol); err != nil {
			t.Fatal(err)
		}
	}
	c := clusterStore{db: db, node: "node"}
	s := frontend.Session{ID: "broker", Protocol: "jellyfin", ViewerID: 1, Item: core.MovieKey(1), Device: core.Device{ID: "d", Client: "secret-token"}, MediaPath: "/grant-secret", StartedAt: time.Now()}
	if err := c.put(ctx, s, "source", core.Viewer{ID: 1}, time.Now()); err != nil {
		t.Fatal(err)
	}
	var data, device string
	if err := db.QueryRowContext(ctx, `SELECT data,device_data FROM plex_sessions WHERE id=?`, s.ID).Scan(&data, &device); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(data+device, "secret") {
		t.Fatal("credential or grant persisted")
	}
	if err := c.expire(ctx, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM plex_sessions`).Scan(&remaining); err != nil || remaining != 2 {
		t.Fatalf("expiry removed Plex rows: %d %v", remaining, err)
	}
	if err := c.put(ctx, s, "source", core.Viewer{ID: 1}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := c.cleanup(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM plex_sessions`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("plex rows lost: %d, %v", count, err)
	}
}

func TestClusterBrokerListsStopsAndOrdersReportsAcrossNodes(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	hub := clusterbus.NewHub()
	active := func(_ context.Context, id int64) (*core.Viewer, error) {
		return &core.Viewer{ID: id, IsOwner: id == 1}, nil
	}
	mk := func(node string) *Broker {
		busNode := hub.Node(node)
		events := NewEventBus(busNode, active)
		t.Cleanup(events.Close)
		b, err := NewBroker(BrokerDeps{DB: db, Cluster: busNode, Events: events, Playback: &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 5}, Transcoder: fakeBrokerTranscoder{}, Progress: &fakeProgress{}, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, key core.RatingKey) (*core.Title, error) {
			return &core.Title{Key: key}, nil
		}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { b.Close(ctx) })
		return b
	}
	a, b := mk("a"), mk("b")
	viewer, device := &core.Viewer{ID: 2}, core.Device{ID: "phone"}
	p, err := a.Plan(ctx, frontend.PlanRequest{Viewer: viewer, Device: device, Protocol: "jellyfin", Item: core.MovieKey(1), Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true}}})
	if err != nil {
		t.Fatal(err)
	}
	s, err := a.Open(ctx, frontend.OpenRequest{Viewer: viewer, Device: device, Protocol: "jellyfin", PlanID: p.ID})
	if err != nil {
		t.Fatal(err)
	}
	owner := frontend.SessionAccess{Viewer: &core.Viewer{ID: 1}, Owner: true}
	list, err := b.Sessions(ctx, owner)
	if err != nil || len(list) != 1 || list[0].ID != s.ID || list[0].MediaPath != "" || list[0].Protocol != "jellyfin" {
		t.Fatalf("remote list: %+v %v", list, err)
	}
	if err := b.Stop(ctx, frontend.SessionAccess{Viewer: &core.Viewer{ID: 3}, Device: device, Protocol: "jellyfin"}, s.ID); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("foreign stop: %v", err)
	}
	target, err := a.deps.Events.Subscribe(ctx, frontend.EventScope{Viewer: viewer, Protocol: "jellyfin", SessionID: s.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Command(ctx, &core.Viewer{ID: 1}, s.ID, frontend.Command{Kind: frontend.CommandPause}); err != nil {
		t.Fatal(err)
	}
	access := frontend.SessionAccess{Viewer: viewer, Device: device, Protocol: "jellyfin"}
	progress := a.deps.Progress.(*fakeProgress)
	sources := a.deps.Playback.Sessions()
	if len(sources) != 1 {
		t.Fatal("missing playback source")
	}
	sources[0].Info.Duration = time.Hour
	for _, report := range []struct {
		broker   *Broker
		sequence uint64
		position time.Duration
	}{{a, 2, 2 * time.Minute}, {b, 1, time.Minute}, {b, 2, time.Minute}, {b, 3, 3 * time.Minute}, {a, 0, 4 * time.Minute}} {
		if err := report.broker.Report(ctx, access, s.ID, frontend.Timeline{Sequence: report.sequence, Position: report.position, State: "playing"}); err != nil {
			t.Fatal(err)
		}
	}
	if progress.calls != 3 || progress.position != 4*time.Minute || progress.duration != time.Hour {
		t.Fatalf("out-of-order progress: %+v", progress)
	}
	var last string
	if err := db.QueryRowContext(ctx, `SELECT report_sequence FROM plex_sessions WHERE id=?`, s.ID).Scan(&last); err != nil || last != "00000000000000000003" {
		t.Fatalf("stored sequence=%q err=%v", last, err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, report := range []struct {
		broker   *Broker
		sequence uint64
	}{{a, 4}, {b, 5}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- report.broker.Report(ctx, access, s.ID, frontend.Timeline{Sequence: report.sequence, Position: time.Duration(report.sequence) * time.Minute, State: "playing"})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if progress.position != 5*time.Minute {
		t.Fatalf("concurrent reports ended at %v", progress.position)
	}
	if err := db.QueryRowContext(ctx, `SELECT report_sequence FROM plex_sessions WHERE id=?`, s.ID).Scan(&last); err != nil || last != "00000000000000000005" {
		t.Fatalf("concurrent sequence=%q err=%v", last, err)
	}
	priorCalls := progress.calls
	if err := b.Report(ctx, access, s.ID, frontend.Timeline{Sequence: ^uint64(0), Position: 5 * time.Minute, Duration: time.Hour, State: "playing"}); err != nil {
		t.Fatal(err)
	}
	if err := a.Report(ctx, access, s.ID, frontend.Timeline{Sequence: 3, Position: time.Minute, Duration: time.Hour, State: "playing"}); err != nil {
		t.Fatal(err)
	}
	if progress.calls != priorCalls+1 || progress.position != 5*time.Minute {
		t.Fatalf("uint64 sequence ordering: %+v", progress)
	}
	select {
	case event := <-target:
		if event.Command == nil || event.Command.Kind != frontend.CommandPause {
			t.Fatalf("remote command %+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("remote command not delivered")
	}
	if err := b.Stop(ctx, owner, s.ID); err != nil {
		t.Fatal(err)
	}
	list, err = a.Sessions(ctx, owner)
	if err != nil || len(list) != 0 {
		t.Fatalf("remote stop: %+v %v", list, err)
	}
	p, err = a.Plan(ctx, frontend.PlanRequest{Viewer: viewer, Device: device, Protocol: "jellyfin", Item: core.MovieKey(1), Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true}}})
	if err != nil {
		t.Fatal(err)
	}
	superseded, err := a.Open(ctx, frontend.OpenRequest{Viewer: viewer, Device: device, Protocol: "jellyfin", PlanID: p.ID})
	if err != nil {
		t.Fatal(err)
	}
	p, err = a.Plan(ctx, frontend.PlanRequest{Viewer: viewer, Device: device, Protocol: "jellyfin", Item: core.MovieKey(1), Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true}}})
	if err != nil {
		t.Fatal(err)
	}
	current, err := a.Open(ctx, frontend.OpenRequest{Viewer: viewer, Device: device, Protocol: "jellyfin", PlanID: p.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Report(ctx, access, superseded.ID, frontend.Timeline{State: "stopped"}); err != nil {
		t.Fatalf("late report on other node: %v", err)
	}
	list, err = b.Sessions(ctx, owner)
	if err != nil || len(list) != 1 || list[0].ID != current.ID {
		t.Fatalf("supersession on other node: %+v %v", list, err)
	}
	playing := a.deps.Playback.Sessions()
	if len(playing) != 1 || !b.StopPlayback(ctx, playing[0].ID) {
		t.Fatal("owner Activity could not stop remote source")
	}
	list, err = a.Sessions(ctx, owner)
	if err != nil || len(list) != 0 {
		t.Fatalf("owner remote stop: %+v %v", list, err)
	}
}
