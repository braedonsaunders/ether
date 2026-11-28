package liveevents

import (
	"context"
	"encoding/json"
	"net/http"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/cluster"
	"github.com/braedonsaunders/ether/internal/clusterbus"
	"github.com/braedonsaunders/ether/internal/core"
	_ "github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/plugins"
	"github.com/braedonsaunders/ether/internal/store"
)

type fakePluginSource struct {
	mu      sync.Mutex
	feeds   []FeedInfo
	handler func(ctx context.Context, pluginID string, req plugins.Request) (plugins.Response, error)
	calls   atomic.Int64
	called  chan plugins.Request
}

func newFakePluginSource(feeds ...FeedInfo) *fakePluginSource {
	return &fakePluginSource{
		feeds:  feeds,
		called: make(chan plugins.Request, 128),
	}
}

func (f *fakePluginSource) ActiveLiveFeeds(context.Context) []FeedInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]FeedInfo(nil), f.feeds...)
}

func (f *fakePluginSource) SetFeeds(feeds ...FeedInfo) {
	f.mu.Lock()
	f.feeds = append([]FeedInfo(nil), feeds...)
	f.mu.Unlock()
}

func (f *fakePluginSource) Call(ctx context.Context, pluginID string, req plugins.Request) (plugins.Response, error) {
	f.calls.Add(1)
	select {
	case f.called <- req:
	default:
	}
	f.mu.Lock()
	h := f.handler
	f.mu.Unlock()
	if h == nil {
		return plugins.Response{Status: 200, Body: json.RawMessage(`{"events":[]}`)}, nil
	}
	return h(ctx, pluginID, req)
}

func waitCall(t *testing.T, ch <-chan plugins.Request) plugins.Request {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for plugin call")
		return plugins.Request{}
	}
}

func waitBatch(t *testing.T, ch <-chan []core.LiveEvent) []core.LiveEvent {
	t.Helper()
	select {
	case b := <-ch:
		return b
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for event batch")
		return nil
	}
}

func waitAlert(t *testing.T, ch <-chan core.LiveEventAlert) core.LiveEventAlert {
	t.Helper()
	select {
	case a := <-ch:
		return a
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for alert")
		return core.LiveEventAlert{}
	}
}

func sampleEvent(id, state, scoreA, scoreB, updatedAt string) core.LiveEvent {
	return core.LiveEvent{
		ID:       id,
		Category: "ball",
		League:   core.LiveEventLeague{ID: "top", Name: "Top League", Abbr: "TL"},
		Title:    "Alpha vs Beta",
		Start:    "2026-11-22T18:00:00Z",
		Status:   core.LiveEventStatus{State: state, Detail: "P4 · 2:14", Period: 4, Clock: "2:14", Close: state == core.LiveEventStateIn},
		Competitors: []core.LiveEventCompetitor{
			{ID: "a", Name: "Alpha", Abbr: "ALP", Score: scoreA, Home: false},
			{ID: "b", Name: "Beta", Abbr: "BET", Score: scoreB, Home: true, Winner: state == core.LiveEventStatePost},
		},
		Situation:   map[string]string{"Possession": "BET"},
		Probability: &core.LiveEventProbability{Label: "Win", Value: 0.72, CompetitorID: "b"},
		UpdatedAt:   updatedAt,
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestHubNoIdlePolling(t *testing.T) {
	clk := newFakeClock(time.Date(2026, 11, 22, 18, 0, 0, 0, time.UTC))
	src := newFakePluginSource(FeedInfo{PluginID: "plug.a", FeedID: "scores", MinIntervalSec: 15})
	hub := NewHub(Options{Source: src, Clock: clk})
	defer hub.Close()

	if !hub.HasLiveFeeds(context.Background()) {
		t.Fatal("expected HasLiveFeeds = true")
	}
	clk.Advance(3 * time.Hour)
	if got := src.calls.Load(); got != 0 {
		t.Fatalf("idle hub made %d plugin calls, want 0", got)
	}
	if got := clk.ActiveTimerCount(); got != 0 {
		t.Fatalf("idle hub armed %d timers, want 0", got)
	}
}

func TestHubSubscribePollsAndStopsOnUnsubscribe(t *testing.T) {
	clk := newFakeClock(time.Date(2026, 11, 22, 18, 0, 0, 0, time.UTC))
	src := newFakePluginSource(FeedInfo{PluginID: "plug.a", FeedID: "scores", MinIntervalSec: 15})

	var seq atomic.Int64
	src.handler = func(_ context.Context, _ string, _ plugins.Request) (plugins.Response, error) {
		n := seq.Add(1)
		ev := sampleEvent("g1", core.LiveEventStateIn, "14", "10", "2026-11-22T18:00:00Z")
		nextPoll := 30
		if n >= 2 {
			ev.Competitors[0].Score = "21"
			ev.UpdatedAt = "2026-11-22T18:00:30Z"
			nextPoll = 5 // below minIntervalSec=15 -> clamped to 15s
		}
		return plugins.Response{
			Status: 200,
			Body:   mustJSON(t, core.LiveEventsResponse{Events: []core.LiveEvent{ev}, NextPollSec: nextPoll}),
		}, nil
	}

	hub := NewHub(Options{Source: src, Clock: clk})
	defer hub.Close()

	sub, err := hub.Subscribe(context.Background(), SubscribeRequest{Feeds: []string{"plug.a:scores"}})
	if err != nil {
		t.Fatal(err)
	}

	// First poll fires immediately on subscribe.
	waitCall(t, src.called)
	b1 := waitBatch(t, sub.Updates())
	if len(b1) != 1 || b1[0].ID != "plug.a:scores:g1" || b1[0].Competitors[0].Score != "14" {
		t.Fatalf("unexpected first batch: %+v", b1)
	}

	// Poller respected nextPollSec=30 (> minIntervalSec=15).
	clk.WaitForActiveTimers(t, 1)
	clk.Advance(29 * time.Second)
	if got := src.calls.Load(); got != 1 {
		t.Fatalf("polled too early at 29s: calls=%d", got)
	}
	clk.Advance(1 * time.Second)
	waitCall(t, src.called)
	b2 := waitBatch(t, sub.Updates())
	if len(b2) != 1 || b2[0].Competitors[0].Score != "21" {
		t.Fatalf("unexpected second batch: %+v", b2)
	}

	// Second response asked for nextPollSec=5 -> clamped to minIntervalSec=15s.
	clk.WaitForActiveTimers(t, 1)
	clk.Advance(14 * time.Second)
	if got := src.calls.Load(); got != 2 {
		t.Fatalf("polled below minIntervalSec at 14s: calls=%d", got)
	}
	clk.Advance(1 * time.Second)
	waitCall(t, src.called)
	clk.WaitForActiveTimers(t, 1)

	// Unsubscribing stops polling immediately.
	sub.Close()
	hub.mu.Lock()
	activePollers := len(hub.pollers)
	hub.mu.Unlock()
	if activePollers != 0 {
		t.Fatalf("expected 0 active pollers after unsubscribe, got %d", activePollers)
	}
	before := src.calls.Load()
	clk.Advance(2 * time.Hour)
	if after := src.calls.Load(); after != before {
		t.Fatalf("poller continued after unsubscribe: before=%d after=%d", before, after)
	}
}

func TestHubRateLimitBackoffAndRetryAfter(t *testing.T) {
	clk := newFakeClock(time.Date(2026, 11, 22, 18, 0, 0, 0, time.UTC))
	src := newFakePluginSource(FeedInfo{PluginID: "plug.a", FeedID: "scores", MinIntervalSec: 10})

	var attempt atomic.Int64
	src.handler = func(_ context.Context, _ string, _ plugins.Request) (plugins.Response, error) {
		n := attempt.Add(1)
		switch n {
		case 1:
			return plugins.Response{Status: http.StatusTooManyRequests, Headers: map[string]string{"Retry-After": "45"}}, nil
		case 2:
			return plugins.Response{}, plugins.ErrRateLimited
		default:
			ev := sampleEvent("g1", core.LiveEventStateIn, "7", "3", "2026-11-22T18:02:00Z")
			return plugins.Response{Status: 200, Body: mustJSON(t, core.LiveEventsResponse{Events: []core.LiveEvent{ev}, NextPollSec: 10})}, nil
		}
	}

	hub := NewHub(Options{Source: src, Clock: clk})
	defer hub.Close()

	sub, err := hub.Subscribe(context.Background(), SubscribeRequest{Feeds: []string{"plug.a:scores"}})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()

	// Attempt 1 -> HTTP 429 with Retry-After: 45.
	waitCall(t, src.called)
	clk.WaitForActiveTimers(t, 1)
	clk.Advance(44 * time.Second)
	if src.calls.Load() != 1 {
		t.Fatalf("did not honor Retry-After 45s")
	}
	clk.Advance(1 * time.Second)

	// Attempt 2 -> ErrRateLimited; 2nd consecutive error -> 10s << 1 = 20s backoff.
	waitCall(t, src.called)
	clk.WaitForActiveTimers(t, 1)
	clk.Advance(19 * time.Second)
	if src.calls.Load() != 2 {
		t.Fatalf("did not honor 20s exponential backoff")
	}
	clk.Advance(1 * time.Second)

	// Attempt 3 -> recovers cleanly and emits event.
	waitCall(t, src.called)
	b := waitBatch(t, sub.Updates())
	if len(b) != 1 || b[0].Competitors[0].Score != "7" {
		t.Fatalf("unexpected recovery batch: %+v", b)
	}
}

func TestHubSingleFlightDeduplication(t *testing.T) {
	clk := newFakeClock(time.Date(2026, 11, 22, 18, 0, 0, 0, time.UTC))
	src := newFakePluginSource(FeedInfo{PluginID: "plug.a", FeedID: "scores", MinIntervalSec: 15})

	gate := make(chan struct{})
	src.handler = func(_ context.Context, _ string, req plugins.Request) (plugins.Response, error) {
		<-gate
		if req.Path == "/ether/v1/live/scores/events/g1" {
			d := core.LiveEventDetail{LiveEvent: sampleEvent("g1", core.LiveEventStateIn, "10", "7", "2026-11-22T18:00:00Z")}
			return plugins.Response{Status: 200, Body: mustJSON(t, d)}, nil
		}
		ev := sampleEvent("g1", core.LiveEventStateIn, "10", "7", "2026-11-22T18:00:00Z")
		return plugins.Response{Status: 200, Body: mustJSON(t, core.LiveEventsResponse{Events: []core.LiveEvent{ev}, NextPollSec: 15})}, nil
	}

	hub := NewHub(Options{Source: src, Clock: clk})
	defer hub.Close()

	const workers = 8
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := hub.FetchEvents(context.Background(), "plug.a", "scores", EventQuery{League: "top"})
			errs <- err
		}()
	}
	waitCall(t, src.called)
	close(gate)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("FetchEvents error: %v", err)
		}
	}
	if got := src.calls.Load(); got != 1 {
		t.Fatalf("single-flight made %d plugin calls for %d concurrent callers, want 1", got, workers)
	}

	// Subsequent call within TTL hits query cache without calling plugin.
	if _, err := hub.FetchEvents(context.Background(), "plug.a", "scores", EventQuery{League: "top"}); err != nil {
		t.Fatal(err)
	}
	if got := src.calls.Load(); got != 1 {
		t.Fatalf("cached query hit plugin: calls=%d, want 1", got)
	}
}

func TestHubDiffingUpdatedAtAndContentHash(t *testing.T) {
	clk := newFakeClock(time.Date(2026, 11, 22, 18, 0, 0, 0, time.UTC))
	src := newFakePluginSource(FeedInfo{PluginID: "plug.a", FeedID: "scores", MinIntervalSec: 10})
	hub := NewHub(Options{Source: src, Clock: clk})
	defer hub.Close()

	var transitions []string
	unsubHook := hub.OnEventChange(func(prev *core.LiveEvent, next core.LiveEvent) {
		from := "<nil>"
		if prev != nil {
			from = prev.Competitors[0].Score
		}
		transitions = append(transitions, from+"->"+next.Competitors[0].Score)
	})
	defer unsubHook()

	sub, err := hub.Subscribe(context.Background(), SubscribeRequest{EventIDs: []string{"plug.a:scores:g1"}})
	if err != nil {
		t.Fatal(err)
	}
	// Drain initial empty poll triggered by subscribe.
	waitCall(t, src.called)

	// 1. Initial event arrives.
	ev1 := sampleEvent("g1", core.LiveEventStateIn, "14", "10", "2026-11-22T18:05:00Z")
	if changed := hub.ingestEvents("plug.a", "scores", []core.LiveEvent{ev1}, false); len(changed) != 1 {
		t.Fatalf("initial ingest changed=%d, want 1", len(changed))
	}
	b1 := waitBatch(t, sub.Updates())
	if b1[0].UpdatedAt != "2026-11-22T18:05:00Z" {
		t.Fatalf("unexpected updatedAt: %q", b1[0].UpdatedAt)
	}

	// 2. Identical content with newer updatedAt -> suppressed (no noise).
	evSame := sampleEvent("g1", core.LiveEventStateIn, "14", "10", "2026-11-22T18:05:30Z")
	if changed := hub.ingestEvents("plug.a", "scores", []core.LiveEvent{evSame}, false); len(changed) != 0 {
		t.Fatalf("unchanged content emitted %d updates, want 0", len(changed))
	}

	// 3. Stale out-of-order older updatedAt even with different score -> ignored.
	evStale := sampleEvent("g1", core.LiveEventStateIn, "7", "10", "2026-11-22T18:01:00Z")
	if changed := hub.ingestEvents("plug.a", "scores", []core.LiveEvent{evStale}, false); len(changed) != 0 {
		t.Fatalf("stale update emitted %d updates, want 0", len(changed))
	}

	// 4. Real score change even when plugin forgot to bump updatedAt -> emitted AND updatedAt advances.
	evChanged := sampleEvent("g1", core.LiveEventStateIn, "21", "10", "2026-11-22T18:05:00Z")
	if changed := hub.ingestEvents("plug.a", "scores", []core.LiveEvent{evChanged}, false); len(changed) != 1 {
		t.Fatalf("score change emitted %d updates, want 1", len(changed))
	}
	b2 := waitBatch(t, sub.Updates())
	if b2[0].Competitors[0].Score != "21" {
		t.Fatalf("got score %q, want 21", b2[0].Competitors[0].Score)
	}
	if b2[0].UpdatedAt <= b1[0].UpdatedAt {
		t.Fatalf("updatedAt did not advance on content change: prev=%q next=%q", b1[0].UpdatedAt, b2[0].UpdatedAt)
	}

	if len(transitions) != 2 || transitions[0] != "<nil>->14" || transitions[1] != "14->21" {
		t.Fatalf("unexpected OnEventChange transitions: %v", transitions)
	}
}

func TestHubBoundedMemoryEviction(t *testing.T) {
	clk := newFakeClock(time.Date(2026, 11, 22, 18, 0, 0, 0, time.UTC))
	src := newFakePluginSource(
		FeedInfo{PluginID: "plug.a", FeedID: "f1", MinIntervalSec: 10},
		FeedInfo{PluginID: "plug.a", FeedID: "f2", MinIntervalSec: 10},
	)
	hub := NewHub(Options{
		Source:           src,
		Clock:            clk,
		MaxEventsPerFeed: 3,
		MaxTotalEvents:   4,
		MaxCachedDetails: 2,
		MaxCachedQueries: 2,
	})
	defer hub.Close()

	// Insert 1 live ("in") event and 4 finished ("post") events on f1.
	evs := []core.LiveEvent{
		sampleEvent("live1", core.LiveEventStateIn, "7", "7", "2026-11-22T18:00:00Z"),
		sampleEvent("post1", core.LiveEventStatePost, "10", "3", "2026-11-22T17:00:00Z"),
		sampleEvent("post2", core.LiveEventStatePost, "14", "7", "2026-11-22T17:10:00Z"),
		sampleEvent("post3", core.LiveEventStatePost, "20", "17", "2026-11-22T17:20:00Z"),
		sampleEvent("post4", core.LiveEventStatePost, "24", "21", "2026-11-22T17:30:00Z"),
	}
	hub.ingestEvents("plug.a", "f1", evs, false)

	hub.mu.Lock()
	f1Count := hub.feedEventCounts["plug.a:f1"]
	_, liveKept := hub.events["plug.a:f1:live1"]
	hub.mu.Unlock()
	if f1Count > 3 {
		t.Fatalf("feed event count = %d, want <= 3", f1Count)
	}
	if !liveKept {
		t.Fatal("expected live (in-progress) event to be prioritized over older post events")
	}

	// Insert 3 events on f2 -> global cap of 4 must hold.
	hub.ingestEvents("plug.a", "f2", []core.LiveEvent{
		sampleEvent("b1", core.LiveEventStatePost, "1", "0", "2026-11-22T18:00:00Z"),
		sampleEvent("b2", core.LiveEventStatePost, "2", "1", "2026-11-22T18:01:00Z"),
		sampleEvent("b3", core.LiveEventStatePost, "3", "2", "2026-11-22T18:02:00Z"),
	}, false)
	hub.mu.Lock()
	total := len(hub.events)
	hub.mu.Unlock()
	if total > 4 {
		t.Fatalf("total cached events = %d, want <= 4", total)
	}
}

func TestHubSpoilerFilteringAndPerViewerReveal(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	// Insert two active viewers for DB-backed preferences & reveals.
	if _, err := db.ExecContext(ctx, `INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(101,101,0,0),(102,102,0,0)`); err != nil {
		t.Fatal(err)
	}

	clk := newFakeClock(time.Date(2026, 11, 22, 18, 0, 0, 0, time.UTC))
	src := newFakePluginSource(FeedInfo{PluginID: "plug.a", FeedID: "scores", MinIntervalSec: 15})
	detailPayload := core.LiveEventDetail{
		LiveEvent: sampleEvent("g1", core.LiveEventStateIn, "24", "21", "2026-11-22T18:10:00Z"),
		Tables:    []core.LiveEventTable{{Title: "Linescore", Columns: []string{"T"}, Rows: [][]string{{"24"}, {"21"}}}},
		Plays:     []core.LiveEventPlay{{Clock: "2:14", Period: 4, Text: "Touchdown", Emphasis: core.LiveEventPlayEmphasisScore}},
		ProbabilitySeries: []core.LiveEventProbabilityPoint{
			{T: core.ProbFraction(0.5), Value: 0.65},
		},
		Links: []core.LiveEventLink{{Label: "Summary", URL: "https://example.invalid/g1"}},
	}
	upcomingEv := core.LiveEvent{
		ID:       "g2",
		Category: "ball",
		League:   core.LiveEventLeague{ID: "top", Name: "Top League"},
		Title:    "Gamma vs Delta",
		Start:    "2026-11-23T19:00:00Z",
		Status:   core.LiveEventStatus{State: core.LiveEventStatePre, Detail: "Tomorrow 7:00 PM"},
		Competitors: []core.LiveEventCompetitor{
			{ID: "c", Name: "Gamma"},
			{ID: "d", Name: "Delta"},
		},
		UpdatedAt: "2026-11-22T18:00:00Z",
	}

	src.handler = func(_ context.Context, _ string, req plugins.Request) (plugins.Response, error) {
		if req.Path == "/ether/v1/live/scores/events/g1" {
			return plugins.Response{Status: 200, Body: mustJSON(t, detailPayload)}, nil
		}
		return plugins.Response{
			Status: 200,
			Body:   mustJSON(t, core.LiveEventsResponse{Events: []core.LiveEvent{detailPayload.LiveEvent, upcomingEv}, NextPollSec: 15}),
		}, nil
	}

	hub := NewHub(Options{Source: src, DB: db, Clock: clk})
	defer hub.Close()

	alice := &core.Viewer{ID: 101}
	bob := &core.Viewer{ID: 102}

	// Default spoilerFree is off.
	if sf, err := hub.SpoilerFree(ctx, alice.ID); err != nil || sf {
		t.Fatalf("default spoilerFree = %v, err=%v; want false", sf, err)
	}
	// Turn spoilerFree ON for Alice and Bob.
	if err := hub.SetSpoilerFree(ctx, alice.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := hub.SetSpoilerFree(ctx, bob.ID, true); err != nil {
		t.Fatal(err)
	}

	// Alice gets masked detail for live event g1.
	det, err := hub.GetDetail(ctx, alice, "plug.a:scores:g1")
	if err != nil {
		t.Fatal(err)
	}
	if !det.Spoiler {
		t.Fatal("expected spoiler=true for spoilerFree viewer")
	}
	if det.Competitors[0].Score != "" || det.Competitors[1].Score != "" || det.Probability != nil || det.Situation != nil || det.Status.Clock != "" || det.Status.Close || len(det.Plays) != 0 || len(det.ProbabilitySeries) != 0 || len(det.Tables) != 0 {
		t.Fatalf("spoiler fields leaked on masked detail: %+v", det)
	}
	if len(det.Links) != 1 || det.Competitors[0].Name != "Alpha" {
		t.Fatalf("non-spoiler metadata stripped unexpectedly: %+v", det)
	}

	// Upcoming event g2 remains spoiler=false with scheduled detail preserved.
	evs, err := hub.GetEvents(ctx, alice, []string{"plug.a:scores:g1", "plug.a:scores:g2"})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 || !evs[0].Spoiler || evs[1].Spoiler || evs[1].Status.Detail != "Tomorrow 7:00 PM" {
		t.Fatalf("unexpected GetEvents spoiler behavior: %+v", evs)
	}

	// Subscribe Alice with FilterSpoilers: hidden score changes do NOT spam her stream.
	subAlice, err := hub.Subscribe(ctx, SubscribeRequest{
		EventIDs:       []string{"plug.a:scores:g1"},
		Viewer:         alice,
		FilterSpoilers: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer subAlice.Close()
	snap, err := hub.SubscriptionSnapshot(ctx, subAlice)
	if err != nil || len(snap) != 1 || !snap[0].Spoiler {
		t.Fatalf("expected masked initial snapshot, got %+v err=%v", snap, err)
	}

	// Score changes while masked -> Alice should NOT get a spoiler-leaking update frame.
	scoreBump := sampleEvent("g1", core.LiveEventStateIn, "31", "21", "2026-11-22T18:12:00Z")
	hub.ingestEvents("plug.a", "scores", []core.LiveEvent{scoreBump}, false)
	select {
	case unexpected := <-subAlice.Updates():
		t.Fatalf("spoiler-free viewer received update for hidden score change: %+v", unexpected)
	default:
	}

	// Alice explicitly reveals g1 -> receives immediate unmasked update on her stream!
	revealedEv, err := hub.RevealEvent(ctx, alice, "plug.a:scores:g1")
	if err != nil {
		t.Fatal(err)
	}
	if revealedEv == nil || revealedEv.Spoiler || revealedEv.Competitors[0].Score != "31" {
		t.Fatalf("RevealEvent returned %+v, want unmasked score 31", revealedEv)
	}
	pushed := waitBatch(t, subAlice.Updates())
	if len(pushed) != 1 || pushed[0].Spoiler || pushed[0].Competitors[0].Score != "31" {
		t.Fatalf("stream did not receive unmasked event on reveal: %+v", pushed)
	}

	// Subsequent detail fetch for Alice is unmasked; Bob is STILL masked!
	detAlice, err := hub.GetDetail(ctx, alice, "plug.a:scores:g1")
	if err != nil || detAlice.Spoiler || len(detAlice.Plays) != 1 || detAlice.Competitors[0].Score != "24" {
		t.Fatalf("Alice detail after reveal = %+v, err=%v", detAlice, err)
	}
	detBob, err := hub.GetDetail(ctx, bob, "plug.a:scores:g1")
	if err != nil || !detBob.Spoiler || len(detBob.Plays) != 0 || detBob.Competitors[0].Score != "" {
		t.Fatalf("Bob detail leaked Alice's reveal: %+v, err=%v", detBob, err)
	}

	// Verify persistence across a fresh SpoilerStore reading the same DB.
	reloaded := NewSpoilerStore(db, clk, nil)
	if sf, err := reloaded.SpoilerFree(ctx, alice.ID); err != nil || !sf {
		t.Fatalf("persisted spoilerFree = %v, err=%v", sf, err)
	}
	if rev, err := reloaded.IsRevealed(ctx, alice.ID, "plug.a:scores:g1"); err != nil || !rev {
		t.Fatalf("persisted reveal = %v, err=%v", rev, err)
	}
}

func TestHubAlertsAndSpoilerFreeFiltering(t *testing.T) {
	ctx := context.Background()
	clk := newFakeClock(time.Date(2026, 11, 22, 18, 0, 0, 0, time.UTC))
	hub := NewHub(Options{Clock: clk})
	defer hub.Close()

	normal := &core.Viewer{ID: 1}
	spoilerFree := &core.Viewer{ID: 2}
	if err := hub.SetSpoilerFree(ctx, spoilerFree.ID, true); err != nil {
		t.Fatal(err)
	}

	subNormal, err := hub.Subscribe(ctx, SubscribeRequest{Viewer: normal, Alerts: true})
	if err != nil {
		t.Fatal(err)
	}
	defer subNormal.Close()

	subSF, err := hub.Subscribe(ctx, SubscribeRequest{Viewer: spoilerFree, Alerts: true})
	if err != nil {
		t.Fatal(err)
	}
	defer subSF.Close()

	alerts := []core.LiveEventAlert{
		{ID: "a1", Kind: core.LiveEventAlertStarted, EventID: "p:f:g1", EntityKey: "p:f:team:a", Title: "Started", Body: "Kickoff"},
		{ID: "a2", Kind: core.LiveEventAlertClose, EventID: "p:f:g1", EntityKey: "p:f:team:a", Title: "Close", Body: "Within 3"},
		{ID: "a3", Kind: core.LiveEventAlertFinal, EventID: "p:f:g1", EntityKey: "p:f:team:a", Title: "Final", Body: "27-24"},
	}
	for _, a := range alerts {
		hub.PublishAlert(ctx, normal.ID, a)
		hub.PublishAlert(ctx, spoilerFree.ID, a)
	}

	// Normal viewer gets all three alerts.
	for _, want := range []string{"a1", "a2", "a3"} {
		got := waitAlert(t, subNormal.Alerts())
		if got.ID != want || got.At == "" {
			t.Fatalf("normal viewer got alert %+v, want ID=%s with timestamp", got, want)
		}
	}

	// Spoiler-free viewer gets ONLY the started alert.
	gotSF := waitAlert(t, subSF.Alerts())
	if gotSF.ID != "a1" || gotSF.Kind != core.LiveEventAlertStarted {
		t.Fatalf("spoiler-free viewer got %+v, want started alert a1", gotSF)
	}
	select {
	case extra := <-subSF.Alerts():
		t.Fatalf("spoiler-free viewer received forbidden alert: %+v", extra)
	default:
	}
}

func TestHubWatchEnricherFeedSubscriptionAndEntitiesStandings(t *testing.T) {
	ctx := context.Background()
	clk := newFakeClock(time.Date(2026, 11, 22, 18, 0, 0, 0, time.UTC))
	src := newFakePluginSource(FeedInfo{PluginID: "plug.a", FeedID: "scores", MinIntervalSec: 15, EntityKinds: []string{"team"}})
	src.handler = func(_ context.Context, _ string, req plugins.Request) (plugins.Response, error) {
		switch req.Path {
		case "/ether/v1/live/scores/entities":
			return plugins.Response{Status: 200, Body: mustJSON(t, core.LiveEventEntitiesResponse{
				Entities: []core.LiveEventEntity{{ID: "t1", Kind: "team", Name: "Alpha", Abbr: "ALP", League: "top"}},
			})}, nil
		case "/ether/v1/live/scores/standings":
			return plugins.Response{Status: 200, Body: mustJSON(t, core.LiveEventStandingsResponse{
				Tables: []core.LiveEventTable{{Title: "Table", Columns: []string{"Team", "W"}, Rows: [][]string{{"Alpha", "9"}}}},
			})}, nil
		default:
			ev := sampleEvent("g1", core.LiveEventStateIn, "14", "10", "2026-11-22T18:00:00Z")
			return plugins.Response{Status: 200, Body: mustJSON(t, core.LiveEventsResponse{Events: []core.LiveEvent{ev}, NextPollSec: 15})}, nil
		}
	}

	hub := NewHub(Options{
		Source: src,
		Clock:  clk,
		WatchEnricher: func(_ context.Context, v *core.Viewer, events []core.LiveEvent) {
			for i := range events {
				events[i].Watch = []core.LiveEventWatchTarget{{Kind: core.LiveEventWatchChannel, ChannelID: "ch-1", Name: "Net 1", Live: true, Confidence: 0.9}}
			}
		},
	})
	defer hub.Close()

	viewer := &core.Viewer{ID: 7}
	sub, err := hub.Subscribe(ctx, SubscribeRequest{Feeds: []string{"plug.a:scores"}, Viewer: viewer, FilterSpoilers: true})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()

	snap, err := hub.SubscriptionSnapshot(ctx, sub)
	if err != nil || len(snap) != 1 || len(snap[0].Watch) != 1 || snap[0].Watch[0].ChannelID != "ch-1" {
		t.Fatalf("unexpected enriched screen snapshot: %+v err=%v", snap, err)
	}

	// Entities populate namespaced key + favorite=false and cache.
	ents, err := hub.GetEntities(ctx, "plug.a", "scores", "team", "top", "")
	if err != nil || len(ents.Entities) != 1 || ents.Entities[0].Key != "plug.a:scores:team:t1" || ents.Entities[0].Favorite {
		t.Fatalf("unexpected entities: %+v err=%v", ents, err)
	}
	standings, err := hub.GetStandings(ctx, "plug.a", "scores", "top")
	if err != nil || len(standings.Tables) != 1 || standings.Tables[0].Title != "Table" {
		t.Fatalf("unexpected standings: %+v err=%v", standings, err)
	}
}

// --- Clustered mode fake cluster + tests ---

type testCluster struct {
	nodeID string
	bus    clusterbus.Node

	mu            sync.Mutex
	leading       bool
	subs          map[int]func(bool)
	nextSub       int
	leaderEntered chan struct{}
}

func newTestCluster(nodeID string, bus clusterbus.Node, leading bool) *testCluster {
	return &testCluster{
		nodeID:        nodeID,
		bus:           bus,
		leading:       leading,
		subs:          make(map[int]func(bool)),
		leaderEntered: make(chan struct{}, 8),
	}
}

func (c *testCluster) SetLeading(v bool) {
	c.mu.Lock()
	if c.leading == v {
		c.mu.Unlock()
		return
	}
	c.leading = v
	fns := make([]func(bool), 0, len(c.subs))
	for _, fn := range c.subs {
		fns = append(fns, fn)
	}
	c.mu.Unlock()
	for _, fn := range fns {
		fn(v)
	}
}

func (c *testCluster) Mode() string                { return cluster.ModePostgres }
func (c *testCluster) Start(context.Context) error { return nil }
func (c *testCluster) Close() error                { return nil }
func (c *testCluster) IsLeader() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.leading
}
func (c *testCluster) Epoch() int64 { return 1 }
func (c *testCluster) OnLeaderChange(fn func(bool)) func() {
	if fn == nil {
		return func() {}
	}
	c.mu.Lock()
	c.nextSub++
	id := c.nextSub
	c.subs[id] = fn
	c.mu.Unlock()
	return func() {
		c.mu.Lock()
		delete(c.subs, id)
		c.mu.Unlock()
	}
}
func (c *testCluster) Self() cluster.Node { return cluster.Node{ID: c.nodeID, Live: true} }
func (c *testCluster) Nodes(context.Context) ([]cluster.Node, error) {
	return []cluster.Node{{ID: c.nodeID, Live: true}}, nil
}
func (c *testCluster) AllNodes(context.Context) ([]cluster.Node, error) {
	return []cluster.Node{{ID: c.nodeID, Live: true}}, nil
}
func (c *testCluster) Publish(ctx context.Context, topic string, payload []byte) error {
	return c.bus.Publish(ctx, topic, payload)
}
func (c *testCluster) Subscribe(topic string, fn func([]byte)) func() {
	return c.bus.Subscribe(topic, fn)
}
func (c *testCluster) RunAsLeader(ctx context.Context, _ string, fn func(context.Context)) {
	wake := make(chan struct{}, 1)
	c.mu.Lock()
	c.nextSub++
	id := c.nextSub
	c.subs[id] = func(bool) {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.subs, id)
		c.mu.Unlock()
	}()

	for {
		if ctx.Err() != nil {
			return
		}
		if !c.IsLeader() {
			select {
			case <-ctx.Done():
				return
			case <-wake:
				continue
			}
		}
		leadCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			select {
			case c.leaderEntered <- struct{}{}:
			default:
			}
			fn(leadCtx)
		}()
		for c.IsLeader() && ctx.Err() == nil {
			select {
			case <-ctx.Done():
				cancel()
			case <-wake:
				if !c.IsLeader() {
					cancel()
				}
			case <-done:
			}
		}
		cancel()
		<-done
	}
}

func TestHubClusterLeaderOnlyPollingAndFanout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	busHub := clusterbus.NewHub()
	clkA := newFakeClock(time.Date(2026, 11, 22, 18, 0, 0, 0, time.UTC))
	clkB := newFakeClock(time.Date(2026, 11, 22, 18, 0, 0, 0, time.UTC))

	feed := FeedInfo{PluginID: "plug.a", FeedID: "scores", MinIntervalSec: 15}
	srcA := newFakePluginSource(feed)
	srcB := newFakePluginSource(feed)

	var score atomic.Int64
	makeHandler := func() func(context.Context, string, plugins.Request) (plugins.Response, error) {
		return func(_ context.Context, _ string, _ plugins.Request) (plugins.Response, error) {
			n := score.Add(1)
			ev := sampleEvent("g1", core.LiveEventStateIn, "14", "10", "2026-11-22T18:00:00Z")
			if n >= 2 {
				ev.Competitors[0].Score = "21"
				ev.UpdatedAt = "2026-11-22T18:00:15Z"
			}
			return plugins.Response{Status: 200, Body: mustJSON(t, core.LiveEventsResponse{Events: []core.LiveEvent{ev}, NextPollSec: 15})}, nil
		}
	}
	srcA.handler = makeHandler()
	srcB.handler = makeHandler()

	clA := newTestCluster("node-a", busHub.Node("node-a"), true)
	clB := newTestCluster("node-b", busHub.Node("node-b"), false)

	hubA := NewHub(Options{Source: srcA, Cluster: clA, Clock: clkA})
	hubB := NewHub(Options{Source: srcB, Cluster: clB, Clock: clkB})
	hubA.SyncFeeds(ctx)
	hubB.SyncFeeds(ctx)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); hubA.Start(ctx) }()
	go func() { defer wg.Done(); hubB.Start(ctx) }()

	// Wait until leader A has entered RunAsLeader.
	select {
	case <-clA.leaderEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("leader A did not enter RunAsLeader")
	}
	// Ensure Start's leading flag is set before subscribing.
	hubA.mu.Lock()
	for !hubA.leading {
		hubA.mu.Unlock()
		runtime.Gosched()
		hubA.mu.Lock()
	}
	hubA.mu.Unlock()
	if !hubA.IsLeading() || hubB.IsLeading() {
		t.Fatalf("expected A leading and B following; A=%v B=%v", hubA.IsLeading(), hubB.IsLeading())
	}

	// Viewer subscribes on FOLLOWER B (with alerts enabled too).
	subB, err := hubB.Subscribe(ctx, SubscribeRequest{
		Feeds:          []string{"plug.a:scores"},
		Viewer:         &core.Viewer{ID: 42},
		FilterSpoilers: true,
		Alerts:         true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Leader A receives follower B's interest and polls; follower B makes 0 polls.
	waitCall(t, srcA.called)
	b1 := waitBatch(t, subB.Updates())
	if len(b1) != 1 || b1[0].ID != "plug.a:scores:g1" || b1[0].Competitors[0].Score != "14" {
		t.Fatalf("follower B did not receive leader update: %+v", b1)
	}
	if got := srcB.calls.Load(); got != 0 {
		t.Fatalf("follower B polled plugin directly (%d calls), want 0", got)
	}

	// Next poll on leader A propagates diffed update to follower B.
	// Two timers are active on leader A: remote interest pruner + feed poller.
	clkA.WaitForActiveTimers(t, 2)
	clkA.Advance(15 * time.Second)
	waitCall(t, srcA.called)
	b2 := waitBatch(t, subB.Updates())
	if len(b2) != 1 || b2[0].Competitors[0].Score != "21" {
		t.Fatalf("follower B did not receive second leader update: %+v", b2)
	}
	if got := srcB.calls.Load(); got != 0 {
		t.Fatalf("follower B polled plugin directly (%d calls), want 0", got)
	}

	// Alerts published on leader A reach follower B's open alert stream!
	hubA.PublishAlert(ctx, 42, core.LiveEventAlert{
		ID: "alert-1", Kind: core.LiveEventAlertStarted, EventID: "plug.a:scores:g1", EntityKey: "plug.a:scores:team:a", Title: "Kickoff", Body: "Live now",
	})
	gotAlert := waitAlert(t, subB.Alerts())
	if gotAlert.ID != "alert-1" {
		t.Fatalf("follower B got alert %+v, want alert-1", gotAlert)
	}

	// Leadership failover: A steps down and B becomes leader -> B starts polling for its subscriber!
	clA.SetLeading(false)
	clB.SetLeading(true)
	select {
	case <-clB.leaderEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("node B did not enter RunAsLeader on failover")
	}
	waitCall(t, srcB.called)
	// A steps down asynchronously once its leader context ends.
	for deadline := time.Now().Add(3 * time.Second); hubA.IsLeading(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("expected A to stop leading after failover")
		}
	}

	// Closing B's subscription stops polling on the new leader B with zero idle timers!
	subB.Close()
	hubB.mu.Lock()
	bPollers := len(hubB.pollers)
	hubB.mu.Unlock()
	if bPollers != 0 {
		t.Fatalf("expected 0 active pollers on B after unsubscribe, got %d", bPollers)
	}
	beforeA := srcA.calls.Load()
	beforeB := srcB.calls.Load()
	clkA.Advance(1 * time.Hour)
	clkB.Advance(1 * time.Hour)
	if srcA.calls.Load() != beforeA || srcB.calls.Load() != beforeB {
		t.Fatalf("polling continued after unsubscribe: A %d->%d B %d->%d", beforeA, srcA.calls.Load(), beforeB, srcB.calls.Load())
	}

	cancel()
	wg.Wait()
}

func TestHubOverflowOnlyPublishesChanges(t *testing.T) {
	hub := NewHub(Options{MaxEventsPerFeed: 2, MaxTotalEvents: 3})
	defer hub.Close()
	const before = "2026-11-22T18:00:00Z"
	events := []core.LiveEvent{
		sampleEvent("a", core.LiveEventStatePre, "0", "0", before),
		sampleEvent("b", core.LiveEventStatePre, "0", "0", before),
		sampleEvent("c", core.LiveEventStatePre, "0", "0", before),
	}
	if got := hub.ingestEvents("plug", "feed", events, false); len(got) != len(events) {
		t.Fatalf("initial changes = %d, want %d", len(got), len(events))
	}
	if got := hub.ingestEvents("plug", "feed", events, false); len(got) != 0 {
		t.Fatalf("unchanged overflow re-emitted %d events", len(got))
	}
	events[0].Status.State = core.LiveEventStateIn
	got := hub.ingestEvents("plug", "feed", events, false)
	if len(got) != 1 || got[0].ID != "plug:feed:a" {
		t.Fatalf("updated overflow = %+v", got)
	}
	if len(hub.events) != 2 {
		t.Fatalf("cached events = %d, want 2", len(hub.events))
	}
}

func TestHubAlertsObserveOneBatchAfterFanout(t *testing.T) {
	src := newFakePluginSource(FeedInfo{PluginID: "plug", FeedID: "feed", MinIntervalSec: 10})
	hub := NewHub(Options{Source: src})
	defer hub.Close()
	sub, err := hub.Subscribe(t.Context(), SubscribeRequest{EventIDs: []string{"plug:feed:a", "plug:feed:b"}})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	waitCall(t, src.called)

	calls := 0
	unsubscribe := hub.OnEventBatch(func(changed []core.LiveEvent) {
		calls++
		select {
		case updates := <-sub.Updates():
			if len(updates) != 2 || len(changed) != 2 || updates[0].ID != changed[0].ID || updates[1].ID != changed[1].ID {
				t.Fatalf("subscriber got %+v, hook got %+v", updates, changed)
			}
		default:
			t.Fatal("batch hook ran before subscriber fan-out")
		}
	})
	const at = "2026-11-22T18:00:00Z"
	batch := []core.LiveEvent{
		sampleEvent("a", core.LiveEventStatePre, "0", "0", at),
		sampleEvent("b", core.LiveEventStatePre, "0", "0", at),
	}
	hub.ingestEvents("plug", "feed", batch, false)
	hub.ingestEvents("plug", "feed", batch, false)
	unsubscribe()
	batch[0].Status.State = core.LiveEventStateIn
	hub.ingestEvents("plug", "feed", batch, false)
	if calls != 1 {
		t.Fatalf("batch hook called %d times, want 1", calls)
	}
}
