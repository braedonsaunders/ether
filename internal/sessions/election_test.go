package sessions

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/clusterbus"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestConcurrentOpenElectsOneDeviceSessionAcrossNodes(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	hub := clusterbus.NewHub()
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	mk := func(id string) (*Broker, *fakePlayback) {
		node := hub.Node(id)
		events := NewEventBus(node, active)
		t.Cleanup(events.Close)
		playback := &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 3}
		b, err := NewBroker(BrokerDeps{DB: db, Cluster: node, Events: events, Playback: playback, Transcoder: fakeBrokerTranscoder{}, Progress: &fakeProgress{}, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, k core.RatingKey) (*core.Title, error) {
			return &core.Title{Key: k}, nil
		}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { b.Close(ctx) })
		return b, playback
	}
	a, pa := mk("a")
	b, pb := mk("b")
	viewer, device := &core.Viewer{ID: 2}, core.Device{ID: "same-device"}
	var plans [2]frontend.PlayPlan
	for i, broker := range []*Broker{a, b} {
		p, err := broker.Plan(ctx, frontend.PlanRequest{Viewer: viewer, Device: device, Protocol: "jellyfin", Item: core.MovieKey(1), Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true}}})
		if err != nil {
			t.Fatal(err)
		}
		plans[i] = p
	}
	type outcome struct {
		s   frontend.Session
		err error
	}
	var results [2]outcome
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, broker := range []*Broker{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i].s, results[i].err = broker.Open(ctx, frontend.OpenRequest{Viewer: viewer, Device: device, Protocol: "jellyfin", PlanID: plans[i].ID})
		}()
	}
	close(start)
	wg.Wait()
	for _, r := range results {
		if r.err != nil || r.s.ID == "" || r.s.MediaPath == "" {
			t.Fatalf("open: %+v %v", r.s, r.err)
		}
	}
	var winner string
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(MAX(id),'') FROM plex_sessions WHERE user_id=? AND surface=? AND device_id=? AND active=1`, viewer.ID, "jellyfin", device.ID).Scan(&count, &winner); err != nil || count != 1 {
		t.Fatalf("active rows=%d err=%v", count, err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO plex_sessions(id,node_id,user_id,session_key,data,updated_at,protocol,surface,device_id) VALUES(?,?,?,?,?,?,?,?,?)`, "duplicate", "other", viewer.ID, 999, `{}`, time.Now().UnixMilli(), "jellyfin", "jellyfin", device.ID); err == nil {
		t.Fatal("unique active-device index accepted a duplicate")
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO plex_sessions(id,node_id,user_id,session_key,data,updated_at) VALUES(?,?,?,?,?,?)`, "legacy", "old-node", viewer.ID, 1000, `{}`, time.Now().UnixMilli()); err != nil {
		t.Fatalf("old-shape writer rejected: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		a.mu.Lock()
		na := len(a.sessions)
		a.mu.Unlock()
		b.mu.Lock()
		nb := len(b.sessions)
		b.mu.Unlock()
		if na+nb == 1 && len(pa.Sessions())+len(pb.Sessions()) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("loser not torn down: broker sessions=%d/%d sources=%d/%d", na, nb, len(pa.Sessions()), len(pb.Sessions()))
		}
		time.Sleep(10 * time.Millisecond)
	}
	for i, broker := range []*Broker{a, b} {
		r := results[i]
		if r.s.ID != winner {
			rec := httptest.NewRecorder()
			broker.ServeMedia(rec, httptest.NewRequest(http.MethodGet, r.s.MediaPath, nil))
			if rec.Code != http.StatusNotFound {
				t.Fatalf("loser grant still served: %d", rec.Code)
			}
			continue
		}
		path, err := broker.StartStream(ctx, frontend.SessionAccess{Viewer: viewer, Device: device, Protocol: "jellyfin"}, r.s.ID, frontend.StreamRequest{Format: frontend.StreamDirect})
		if err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		broker.ServeMedia(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("winner media: %d", rec.Code)
		}
	}
}

func TestOpenReplacesPreviousItemOnSameDevice(t *testing.T) {
	ctx := context.Background()
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	events := NewEventBus(nil, active)
	defer events.Close()
	playback := &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 3}
	b, err := NewBroker(BrokerDeps{Events: events, Playback: playback, Transcoder: fakeBrokerTranscoder{}, Progress: &fakeProgress{}, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, k core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: k}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(ctx)
	v, d := &core.Viewer{ID: 2}, core.Device{ID: "device"}
	open := func(key core.RatingKey) frontend.Session {
		p, err := b.Plan(ctx, frontend.PlanRequest{Viewer: v, Device: d, Protocol: "silo", Item: key, Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true}}})
		if err != nil {
			t.Fatal(err)
		}
		s, err := b.Open(ctx, frontend.OpenRequest{Viewer: v, Device: d, Protocol: "silo", PlanID: p.ID})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	old, newer := open(core.MovieKey(1)), open(core.MovieKey(2))
	list, err := b.Sessions(ctx, frontend.SessionAccess{Viewer: v})
	if err != nil || len(list) != 1 || list[0].ID != newer.ID || len(playback.Sessions()) != 1 {
		t.Fatalf("replacement %+v %v", list, err)
	}
	if err := b.Report(ctx, frontend.SessionAccess{Viewer: v, Device: d, Protocol: "silo"}, old.ID, frontend.Timeline{State: "stopped"}); err != nil || len(playback.Sessions()) != 1 {
		t.Fatalf("late stop affected replacement: %v", err)
	}
}
