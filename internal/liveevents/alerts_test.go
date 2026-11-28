package liveevents

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/events"
	"github.com/braedonsaunders/ether/internal/store"
)

type fakeFavoritesStore struct {
	mu   sync.Mutex
	favs []EntityFavorite
}

func (f *fakeFavoritesStore) ListEntityFavorites(_ context.Context) ([]EntityFavorite, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]EntityFavorite, len(f.favs))
	copy(out, f.favs)
	return out, nil
}

func (f *fakeFavoritesStore) Set(favs []EntityFavorite) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.favs = favs
}

type fakeViewerPolicy struct {
	spoilerFree map[int64]bool
	revealed    map[string]bool // "viewerID:eventID"
}

func (p *fakeViewerPolicy) SpoilerFree(_ context.Context, viewerID int64) (bool, error) {
	return p.spoilerFree[viewerID], nil
}

func (p *fakeViewerPolicy) EventRevealed(_ context.Context, viewerID int64, eventID string) (bool, error) {
	if p.revealed == nil {
		return false, nil
	}
	return p.revealed[eventID], nil
}

type fakePushDeliverer struct {
	mu    sync.Mutex
	items []struct {
		viewerID int64
		alert    core.LiveEventAlert
	}
}

func (p *fakePushDeliverer) DeliverLiveEventAlert(_ context.Context, viewerID int64, alert core.LiveEventAlert) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.items = append(p.items, struct {
		viewerID int64
		alert    core.LiveEventAlert
	}{viewerID: viewerID, alert: alert})
	return nil
}

type fakeFeedPoller struct {
	mu            sync.Mutex
	scheduleCalls int
	pollCalls     int
	lastPollIDs   []string
	schedule      []core.LiveEvent
	pollEvents    []core.LiveEvent
	nextPollSec   int
	pollErr       error
}

func (p *fakeFeedPoller) FetchSchedule(_ context.Context, _, _ string, _, _ time.Time) (FeedPollResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.scheduleCalls++
	return FeedPollResult{Events: p.schedule, NextPollSec: p.nextPollSec}, nil
}

func (p *fakeFeedPoller) PollEvents(_ context.Context, _, _ string, ids []string) (FeedPollResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pollCalls++
	p.lastPollIDs = append([]string(nil), ids...)
	return FeedPollResult{Events: p.pollEvents, NextPollSec: p.nextPollSec}, p.pollErr
}

func TestAlertEngineTransitionsSpoilerFreeAndThreeChannelDelivery(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 22, 18, 0, 0, 0, time.UTC)

	reg := events.NewRegistry()
	events.RegisterBuiltins(reg)
	bus := events.NewBus(events.BusOptions{Registry: reg})
	busSub := bus.Subscribe(events.SubscriptionOptions{Name: "test-alerts", Buffer: 32})
	defer busSub.Unsubscribe()

	hub := NewAlertHub()
	// Viewer 1 has the app open (/live/stream?alerts=1); Viewer 2 does not (uses ViewerPush).
	// Viewer 3 is spoiler-free and also has the app open.
	subV1 := hub.SubscribeAlerts(1)
	defer subV1.Close()
	subV3 := hub.SubscribeAlerts(3)
	defer subV3.Close()

	push := &fakePushDeliverer{}
	favs := &fakeFavoritesStore{
		favs: []EntityFavorite{
			{ViewerID: 1, Key: "demo:main:team:wolves"},
			{ViewerID: 2, Key: "demo:main:team:wolves"},
			{ViewerID: 3, Key: "demo:main:team:wolves"},
		},
	}
	policy := &fakeViewerPolicy{
		spoilerFree: map[int64]bool{3: true},
	}

	engine := NewAlertEngine(AlertEngineOptions{
		Favorites: favs,
		Policy:    policy,
		Bus:       bus,
		Push:      push,
		InApp:     hub,
		Now:       func() time.Time { return now },
	})

	baseEv := core.LiveEvent{
		ID:       "demo:main:e1",
		Feed:     "main",
		Category: "fieldball",
		League:   core.LiveEventLeague{ID: "l1", Name: "Premier", Abbr: "PL"},
		Start:    now.UTC().Format(time.RFC3339),
		Status:   core.LiveEventStatus{State: core.LiveEventStatePre},
		Competitors: []core.LiveEventCompetitor{
			{ID: "wolves", Name: "North Wolves", Short: "Wolves", Abbr: "NW", EntityID: "wolves"},
			{ID: "hawks", Name: "South Hawks", Short: "Hawks", Abbr: "SH", Home: true, EntityID: "hawks"},
		},
	}
	// Seed pre-game state.
	engine.SeedScheduleEvent(baseEv)

	// Transition 1: pre -> in (started).
	evIn := baseEv.Clone()
	evIn.Status = core.LiveEventStatus{State: core.LiveEventStateIn, Period: 1, Clock: "15:00"}
	if err := engine.ProcessEvents(ctx, []core.LiveEvent{evIn}); err != nil {
		t.Fatalf("ProcessEvents started: %v", err)
	}

	// Transition 2: close == true.
	evClose := evIn.Clone()
	evClose.Competitors[0].Score = "24"
	evClose.Competitors[1].Score = "21"
	evClose.Status = core.LiveEventStatus{State: core.LiveEventStateIn, Detail: "P4 · 2:10", Period: 4, Clock: "2:10", Close: true}
	if err := engine.ProcessEvents(ctx, []core.LiveEvent{evClose}); err != nil {
		t.Fatalf("ProcessEvents close: %v", err)
	}

	// Repeated close tick (flicker false -> true) must NOT fire a duplicate close alert.
	evNotClose := evClose.Clone()
	evNotClose.Status.Close = false
	_ = engine.ProcessEvents(ctx, []core.LiveEvent{evNotClose})
	_ = engine.ProcessEvents(ctx, []core.LiveEvent{evClose})

	// Transition 3: in -> post (final).
	evFinal := evClose.Clone()
	evFinal.Competitors[0].Score = "28"
	evFinal.Competitors[1].Score = "21"
	evFinal.Status = core.LiveEventStatus{State: core.LiveEventStatePost, Detail: "Final"}
	if err := engine.ProcessEvents(ctx, []core.LiveEvent{evFinal}); err != nil {
		t.Fatalf("ProcessEvents final: %v", err)
	}

	// Drain in-app alerts for viewer 1 (non-spoiler-free, app open): started + close + final.
	drainInApp := func(sub *AlertSubscription) []core.LiveEventAlert {
		var out []core.LiveEventAlert
		for {
			select {
			case a := <-sub.C:
				out = append(out, a)
			default:
				return out
			}
		}
	}
	v1Alerts := drainInApp(subV1)
	if len(v1Alerts) != 3 || v1Alerts[0].Kind != "started" || v1Alerts[1].Kind != "close" || v1Alerts[2].Kind != "final" {
		t.Fatalf("viewer 1 in-app alerts = %+v, want [started, close, final]", v1Alerts)
	}
	if v1Alerts[0].EntityKey != "demo:main:team:wolves" || v1Alerts[0].EventID != "demo:main:e1" {
		t.Fatalf("unexpected alert metadata: %+v", v1Alerts[0])
	}

	// Viewer 3 (spoiler-free): MUST receive started ONLY!
	v3Alerts := drainInApp(subV3)
	if len(v3Alerts) != 1 || v3Alerts[0].Kind != "started" {
		t.Fatalf("spoiler-free viewer 3 received %+v, want [started] only", v3Alerts)
	}
	if strings.Contains(v3Alerts[0].Body, "24") || strings.Contains(v3Alerts[0].Body, "28") {
		t.Fatalf("start alert leaked score in body: %q", v3Alerts[0].Body)
	}

	// ViewerPush: ONLY viewer 2 (app closed) should receive pushes (started + close + final).
	// Viewers 1 and 3 had open in-app AlertHub streams, so push was skipped for them.
	if len(push.items) != 3 {
		t.Fatalf("push deliveries = %d (%+v), want 3 for viewer 2 only", len(push.items), push.items)
	}
	for _, p := range push.items {
		if p.viewerID != 2 {
			t.Fatalf("push delivered to viewer %d who had app open", p.viewerID)
		}
	}

	// Flows bus received events for all delivered alerts (v1:3 + v2:3 + v3:1 = 7).
	var busEvents []events.Envelope
	for {
		select {
		case ev := <-busSub.C():
			busEvents = append(busEvents, ev)
		default:
			goto doneBus
		}
	}
doneBus:
	if len(busEvents) != 7 {
		t.Fatalf("flows bus events = %d, want 7", len(busEvents))
	}

	// Verify exact SSE frame contract for /vela/v0/live/stream?alerts=1.
	frame, err := FormatAlertSSE(v1Alerts[1])
	if err != nil {
		t.Fatalf("FormatAlertSSE: %v", err)
	}
	sFrame := string(frame)
	if !strings.HasPrefix(sFrame, "event: alert\ndata: ") || !strings.HasSuffix(sFrame, "\n\n") {
		t.Fatalf("unexpected SSE frame:\n%s", sFrame)
	}
	rawJSON := strings.TrimSuffix(strings.TrimPrefix(sFrame, "event: alert\ndata: "), "\n\n")
	var decoded map[string]any
	if err := json.Unmarshal([]byte(rawJSON), &decoded); err != nil {
		t.Fatalf("unmarshal SSE payload: %v", err)
	}
	for _, field := range []string{"id", "kind", "eventId", "entityKey", "title", "body", "at"} {
		if _, ok := decoded[field]; !ok {
			t.Fatalf("SSE alert payload missing required field %q: %s", field, rawJSON)
		}
	}
}

func TestAlertSchedulerZeroIdleHourlyScheduleAndBoundedWindowPolling(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 22, 16, 0, 0, 0, time.UTC)
	kickoff := now.Add(2 * time.Hour) // 18:00 UTC -> alert window opens at 17:45 UTC

	favs := &fakeFavoritesStore{}
	poller := &fakeFeedPoller{nextPollSec: 30}
	hub := NewAlertHub()
	sub := hub.SubscribeAlerts(10)
	defer sub.Close()

	engine := NewAlertEngine(AlertEngineOptions{
		Favorites: favs,
		InApp:     hub,
		Now:       func() time.Time { return now },
	})
	sched := NewAlertScheduler(AlertSchedulerOptions{
		Favorites: favs,
		Poller:    poller,
		Engine:    engine,
		Now:       func() time.Time { return now },
	})

	// 1. Zero favourites -> zero schedule fetches, zero polls, idle (hasWake == false).
	_, hasWake, err := sched.Step(ctx)
	if err != nil || hasWake || poller.scheduleCalls != 0 || poller.pollCalls != 0 {
		t.Fatalf("idle step: hasWake=%v sched=%d poll=%d err=%v, want all zero/idle", hasWake, poller.scheduleCalls, poller.pollCalls, err)
	}

	// 2. Viewer adds a favourite 2 hours before kickoff.
	game := core.LiveEvent{
		ID:     "demo:main:g1",
		Feed:   "main",
		Start:  kickoff.UTC().Format(time.RFC3339),
		Status: core.LiveEventStatus{State: core.LiveEventStatePre},
		Competitors: []core.LiveEventCompetitor{
			{ID: "wolves", Name: "North Wolves", EntityID: "wolves"},
			{ID: "hawks", Name: "South Hawks", EntityID: "hawks", Home: true},
		},
	}
	alreadyFinished := core.LiveEvent{
		ID:     "demo:main:g-old",
		Feed:   "main",
		Start:  now.Add(-3 * time.Hour).UTC().Format(time.RFC3339),
		Status: core.LiveEventStatus{State: core.LiveEventStatePost},
		Competitors: []core.LiveEventCompetitor{
			{ID: "wolves", Name: "North Wolves", EntityID: "wolves"},
			{ID: "bears", Name: "West Bears", EntityID: "bears", Home: true},
		},
	}
	poller.schedule = []core.LiveEvent{game, alreadyFinished}
	favs.Set([]EntityFavorite{{ViewerID: 10, Key: "demo:main:team:wolves"}})

	nextWake, hasWake, err := sched.Step(ctx)
	if err != nil || !hasWake {
		t.Fatalf("first fav step: hasWake=%v err=%v", hasWake, err)
	}
	if poller.scheduleCalls != 1 || poller.pollCalls != 0 {
		t.Fatalf("expected 1 schedule call and 0 live polls before window, got sched=%d poll=%d", poller.scheduleCalls, poller.pollCalls)
	}
	// Historical post-game event must NOT open a window or trigger an alert.
	if sched.ActiveWindowCount() != 1 {
		t.Fatalf("ActiveWindowCount = %d, want 1 (only upcoming game)", sched.ActiveWindowCount())
	}
	if !nextWake.Equal(now.Add(time.Hour)) {
		t.Fatalf("nextWake = %v, want +1h (%v)", nextWake, now.Add(time.Hour))
	}

	// 3. 30 minutes later (16:30): still within 1h schedule throttle and outside alert window -> 0 calls!
	now = now.Add(30 * time.Minute)
	_, _, _ = sched.Step(ctx)
	if poller.scheduleCalls != 1 || poller.pollCalls != 0 {
		t.Fatalf("at 16:30 expected no new schedule or poll calls, got sched=%d poll=%d", poller.scheduleCalls, poller.pollCalls)
	}

	// 4. Advance to 17:45 (start - 15m): alert window opens -> live poll fires for g1!
	now = kickoff.Add(-15 * time.Minute)
	evLive := game.Clone()
	evLive.Status.State = core.LiveEventStateIn
	poller.pollEvents = []core.LiveEvent{evLive}
	_, _, _ = sched.Step(ctx)
	if poller.pollCalls != 1 || len(poller.lastPollIDs) != 1 || poller.lastPollIDs[0] != "demo:main:g1" {
		t.Fatalf("at window open expected 1 poll for g1, got calls=%d ids=%v", poller.pollCalls, poller.lastPollIDs)
	}
	select {
	case a := <-sub.C:
		if a.Kind != core.LiveEventAlertStarted {
			t.Fatalf("got alert kind %q, want started", a.Kind)
		}
	default:
		t.Fatalf("expected started alert on in-app hub")
	}

	// 5. Advance 30s -> game hits final whistle -> alert fires, window closes, polling stops!
	now = now.Add(30 * time.Second)
	evFinal := evLive.Clone()
	evFinal.Status.State = core.LiveEventStatePost
	poller.pollEvents = []core.LiveEvent{evFinal}
	_, _, _ = sched.Step(ctx)
	if sched.ActiveWindowCount() != 0 {
		t.Fatalf("ActiveWindowCount after final = %d, want 0", sched.ActiveWindowCount())
	}
	pollsAfterFinal := poller.pollCalls

	// 6. Another 1 minute later -> zero additional live polls!
	now = now.Add(time.Minute)
	_, _, _ = sched.Step(ctx)
	if poller.pollCalls != pollsAfterFinal {
		t.Fatalf("polled after final whistle: %d -> %d", pollsAfterFinal, poller.pollCalls)
	}

	// 7. Removing last favourite returns scheduler to idle (hasWake == false).
	favs.Set(nil)
	_, hasWake, _ = sched.Step(ctx)
	if hasWake {
		t.Fatalf("expected idle (hasWake == false) after last favourite removed")
	}
}

func TestSQLFavoriteEntitiesReaderReadsFavoritesItemsTable(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)

	// Simulate favorites_items table created by internal/favorites (A4).
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS favorites_items (viewer_id INTEGER NOT NULL, kind TEXT NOT NULL, subject_id TEXT NOT NULL, added_at INTEGER NOT NULL, sort_order INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(viewer_id,kind,subject_id))`); err != nil {
		t.Fatalf("create favorites_items: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO favorites_items(viewer_id, kind, subject_id, added_at, sort_order) VALUES (1,'entity','demo:main:team:wolves',100,0),(1,'title','movie:1',101,1),(2,'entity','demo:main:team:hawks',102,0)`); err != nil {
		t.Fatalf("insert favorites_items: %v", err)
	}

	reader := NewSQLFavoriteEntitiesReader(db)
	got, err := reader.ListEntityFavorites(ctx)
	if err != nil {
		t.Fatalf("ListEntityFavorites: %v", err)
	}
	if len(got) != 2 || got[0].ViewerID != 1 || got[0].Key != "demo:main:team:wolves" || got[1].ViewerID != 2 || got[1].Key != "demo:main:team:hawks" {
		t.Fatalf("ListEntityFavorites = %+v, want 2 entity favorites only", got)
	}
}

func TestAlertEngineSeedsColdEventsSilently(t *testing.T) {
	now := time.Date(2026, 11, 22, 18, 0, 0, 0, time.UTC)
	hub := NewAlertHub()
	sub := hub.SubscribeAlerts(10)
	defer sub.Close()
	favs := []EntityFavorite{{ViewerID: 10, Key: "demo:main:team:wolves"}}
	engine := NewAlertEngine(AlertEngineOptions{InApp: hub, Now: func() time.Time { return now }})
	alreadyLive := core.LiveEvent{
		ID: "demo:main:live", Start: now.Add(-time.Hour).Format(time.RFC3339),
		Status:      core.LiveEventStatus{State: core.LiveEventStateIn, Close: true},
		Competitors: []core.LiveEventCompetitor{{EntityID: "wolves"}},
	}
	upcoming := alreadyLive.Clone()
	upcoming.ID = "demo:main:upcoming"
	upcoming.Status = core.LiveEventStatus{State: core.LiveEventStatePre}
	if err := engine.ProcessEventsWithFavorites(t.Context(), []core.LiveEvent{alreadyLive, upcoming}, favs); err != nil {
		t.Fatal(err)
	}
	select {
	case alert := <-sub.C:
		t.Fatalf("cold snapshot sent an alert: %+v", alert)
	default:
	}
	upcoming.Status.State = core.LiveEventStateIn
	alreadyLive.Status.State = core.LiveEventStatePost
	if err := engine.ProcessEventsWithFavorites(t.Context(), []core.LiveEvent{upcoming, alreadyLive}, favs); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{core.LiveEventAlertStarted, core.LiveEventAlertFinal} {
		select {
		case alert := <-sub.C:
			if alert.Kind != kind {
				t.Fatalf("alert kind = %q, want %q", alert.Kind, kind)
			}
		default:
			t.Fatalf("missing %s alert for observed transition", kind)
		}
	}
}

func TestAlertSchedulerBacksOffFailedPolls(t *testing.T) {
	now := time.Date(2026, 11, 22, 18, 0, 0, 0, time.UTC)
	ev := core.LiveEvent{
		ID: "demo:main:g1", Start: now.Format(time.RFC3339),
		Status:      core.LiveEventStatus{State: core.LiveEventStatePre},
		Competitors: []core.LiveEventCompetitor{{EntityID: "wolves"}},
	}
	favs := &fakeFavoritesStore{favs: []EntityFavorite{{ViewerID: 10, Key: "demo:main:team:wolves"}}}
	poller := &fakeFeedPoller{schedule: []core.LiveEvent{ev}, pollErr: errors.New("poll failed")}
	sched := NewAlertScheduler(AlertSchedulerOptions{
		Favorites: favs, Poller: poller, Now: func() time.Time { return now },
	})
	step := func(delay time.Duration, calls int) {
		t.Helper()
		next, awake, err := sched.Step(t.Context())
		if err != nil || !awake || !next.Equal(now.Add(delay)) || poller.pollCalls != calls {
			t.Fatalf("next = %v, awake = %v, calls = %d, err = %v; want +%v and %d calls", next, awake, poller.pollCalls, err, delay, calls)
		}
	}
	step(15*time.Second, 1)
	now = now.Add(15 * time.Second)
	step(30*time.Second, 2)
	now = now.Add(15 * time.Second)
	step(15*time.Second, 2)
	now = now.Add(15 * time.Second)
	poller.pollErr, poller.nextPollSec = nil, 20
	step(20*time.Second, 3)
	now = now.Add(20 * time.Second)
	poller.pollErr, poller.nextPollSec = errors.New("poll failed again"), 0
	step(15*time.Second, 4)
}
