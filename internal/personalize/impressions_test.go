package personalize

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/state"
)

func TestImpressionsBatchedAndCompacted(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	u := &core.User{ID: 7, Username: "ann"}
	hc := hubContext(testNow, core.DeviceTV)
	a, b, c := h.cat.titles[movieKey(0, 1)], h.cat.titles[movieKey(0, 2)], h.cat.titles[movieKey(1, 1)]
	hubs := []core.Hub{
		{ID: "home.top10", Title: "Top 10 for You", Items: []core.Title{a, b}},
		{ID: "home.because.123", Title: "Because You Watched X", Items: []core.Title{c}},
	}
	// A client polling Home 50 times in a day costs one row per hub/item.
	for i := 0; i < 50; i++ {
		hc.Now = testNow.Add(time.Duration(i) * time.Minute)
		h.e.RecordImpressions(u, hc, hubs)
	}
	// Nothing is written until the batch flushes (FlushEvery is an hour).
	var n int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM personalize_row_impressions`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows before flush = %d %v", n, err)
	}
	h.e.Flush(ctx)
	var serves, best int
	var kind string
	if err := h.db.QueryRow(`SELECT COUNT(*), SUM(serves), MIN(best_position) FROM personalize_row_impressions`).Scan(&n, &serves, &best); err != nil {
		t.Fatal(err)
	}
	if n != 2 || serves != 100 || best != 0 {
		t.Fatalf("row impressions = %d rows, %d serves, best %d", n, serves, best)
	}
	if err := h.db.QueryRow(`SELECT kind FROM personalize_row_impressions WHERE hub_id='home.because.123'`).Scan(&kind); err != nil || kind != KindBecause {
		t.Fatalf("kind = %q %v", kind, err)
	}
	var items, shows, rank int
	if err := h.db.QueryRow(`SELECT COUNT(*), SUM(shows), MAX(best_rank) FROM personalize_item_impressions`).Scan(&items, &shows, &rank); err != nil {
		t.Fatal(err)
	}
	if items != 3 || shows != 150 || rank != 1000 {
		t.Fatalf("item impressions = %d items, %d shows, max rank %d", items, shows, rank)
	}
	// The next day is a new row.
	hc.Now = testNow.Add(24 * time.Hour)
	h.e.RecordImpressions(u, hc, hubs[:1])
	h.e.Flush(ctx)
	days, err := h.e.store.shownDays(ctx, u.ID, testNow.AddDate(0, 0, -1))
	if err != nil || days[a.Key] != 2 || days[c.Key] != 1 {
		t.Fatalf("shown days = %v %v", days, err)
	}
}

func TestImpressionsNeverBlock(t *testing.T) {
	st := &sqlStore{loc: time.UTC}
	r := newRecorder(st, quietLogger(), 2, time.Hour) // not running: nothing drains it
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			r.record(impression{userID: 1, at: testNow})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("record blocked on a full buffer")
	}
	if r.dropped.Load() != 98 {
		t.Fatalf("dropped = %d, want 98", r.dropped.Load())
	}
}

func TestParseHubKind(t *testing.T) {
	cases := map[string]string{
		"home.continue":               KindContinue,
		"movie.inprogress.1":          KindContinue,
		"tv.recentlyadded.2":          KindRecentlyAdded,
		"home.playlists":              KindPlaylists,
		"home.because.1000000000123":  KindBecause,
		"movie.because.1000000000123": KindBecause,
		"home.trending.taste":         KindTrendingTaste,
		"home.trending":               KindTrending,
		"tv.popular":                  KindPopular,
		"home.popular.server":         KindPopularServer,
		"home.crowd":                  KindCrowd,
		"movie.crowd.trending":        KindCrowdTrending,
		"home.person.500":             KindPerson,
		"home.franchise.9001":         KindFranchise,
		"home.llm.cosy-mysteries":     KindLLM,
		"collection.abc":              KindCollectionPrefix + "abc",
		"something.else":              "other",
	}
	for id, want := range cases {
		if got := parseHubKind(id); got != want {
			t.Errorf("parseHubKind(%q) = %q, want %q", id, got, want)
		}
	}
}

func TestPlaysAttributedToRows(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	u := core.User{ID: 3, Username: "bo"}
	film := h.cat.titles[movieKey(0, 5)]
	hc := hubContext(testNow.Add(-10*time.Minute), core.DeviceTV)
	h.e.RecordImpressions(&u, hc, []core.Hub{{ID: "home.hiddengems", Title: "Hidden Gems", Items: []core.Title{film}}})

	// Start, watch to completion.
	h.e.handleEvent(ctx, state.Event{Kind: state.EventProgress, UserID: u.ID, Key: film.Key, Fraction: 0.06, Duration: film.Runtime, State: "playing", At: testNow})
	for m := 10; m <= 70; m += 10 { // timeline updates (every ~10s in reality)
		h.e.handleEvent(ctx, state.Event{Kind: state.EventProgress, UserID: u.ID, Key: film.Key, Fraction: float64(m) / 80, Duration: film.Runtime, State: "playing", At: testNow.Add(time.Duration(m) * time.Minute)})
	}
	h.e.handleEvent(ctx, state.Event{Kind: state.EventProgress, UserID: u.ID, Key: film.Key, Fraction: 1, Duration: film.Runtime, State: "playing", Completed: true, At: testNow.Add(80 * time.Minute)})
	h.e.handleEvent(ctx, state.Event{Kind: state.EventProgress, UserID: u.ID, Key: film.Key, Fraction: 0, Duration: film.Runtime, State: "stopped", At: testNow.Add(81 * time.Minute)})

	// An unrelated title, abandoned at 3%: no impression, so no row.
	other := h.cat.titles[movieKey(3, 2)]
	h.e.handleEvent(ctx, state.Event{Kind: state.EventProgress, UserID: u.ID, Key: other.Key, Fraction: 0.01, Duration: other.Runtime, State: "playing", At: testNow.Add(2 * time.Hour)})
	h.e.handleEvent(ctx, state.Event{Kind: state.EventProgress, UserID: u.ID, Key: other.Key, Fraction: 0.03, Duration: other.Runtime, State: "stopped", At: testNow.Add(2*time.Hour + 2*time.Minute)})

	plays, err := h.e.store.plays(ctx, u.ID, time.Time{})
	if err != nil || len(plays) != 2 {
		t.Fatalf("plays = %+v %v", plays, err)
	}
	p := plays[0]
	if p.hubID != "home.hiddengems" || p.kind != KindHiddenGems || !p.completed || p.abandoned || p.device != core.DeviceTV {
		t.Fatalf("attributed play = %+v", p)
	}
	if p.daypart != core.DaypartEvening || p.weekend {
		t.Fatalf("play context = %s weekend=%v", p.daypart, p.weekend)
	}
	if w := p.watched(); w < film.Runtime*9/10 {
		t.Fatalf("watched = %v of %v", w, film.Runtime)
	}
	q := plays[1]
	if q.hubID != "" || !q.abandoned || q.completed {
		t.Fatalf("abandoned play = %+v", q)
	}

	// A new play after a long gap is a separate play.
	h.e.handleEvent(ctx, state.Event{Kind: state.EventProgress, UserID: u.ID, Key: other.Key, Fraction: 0.2, Duration: other.Runtime, State: "playing", At: testNow.Add(5 * time.Hour)})
	plays, _ = h.e.store.plays(ctx, u.ID, time.Time{})
	if len(plays) != 3 || plays[2].startFraction != 0.2 {
		t.Fatalf("resumed play = %+v", plays)
	}

	// Episodes attribute to impressions of their show.
	show := h.cat.titles[showKey(2, 1)]
	h.e.RecordImpressions(&u, hubContext(testNow.Add(6*time.Hour), core.DeviceMobile), []core.Hub{{ID: "home.shortontime", Items: []core.Title{show}}})
	ep := core.EpisodeKey(show.IDs.TMDB, 1, 1)
	h.e.handleEvent(ctx, state.Event{Kind: state.EventProgress, UserID: u.ID, Key: ep, Fraction: 0.1, Duration: show.Runtime, State: "playing", At: testNow.Add(6*time.Hour + time.Minute)})
	plays, _ = h.e.store.plays(ctx, u.ID, time.Time{})
	last := plays[len(plays)-1]
	if last.kind != KindShortOnTime || last.tasteKey != show.Key || last.device != core.DeviceMobile {
		t.Fatalf("episode play = %+v", last)
	}
}

func TestRetentionPurgesOldImpressions(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	u := &core.User{ID: 1}
	item := []core.Hub{{ID: "home.trending", Items: []core.Title{h.cat.titles[movieKey(0, 1)]}}}
	h.e.RecordImpressions(u, hubContext(testNow.AddDate(0, 0, -90), core.DeviceWeb), item)
	h.e.RecordImpressions(u, hubContext(testNow.AddDate(0, 0, -10), core.DeviceWeb), item)
	h.e.Flush(ctx)
	if _, err := h.e.store.purge(ctx, testNow, 60*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	var items, rows int
	h.db.QueryRow(`SELECT COUNT(*) FROM personalize_item_impressions`).Scan(&items)
	h.db.QueryRow(`SELECT COUNT(*) FROM personalize_row_impressions`).Scan(&rows)
	if items != 1 || rows != 2 {
		t.Fatalf("after retention: %d item rows (want 1), %d row rows (want 2, kept 400 days)", items, rows)
	}
}

func TestNeutralImpressionsPreserveKindAndRankWithoutChangingPlexKinds(t *testing.T) {
	h := newHarness(t, nil)
	user := &core.User{ID: 7}
	ctx := context.Background()
	for _, row := range []core.NeutralImpression{
		{RowID: "neutral.one", Kind: KindTop10, Rank: 3, Items: []core.RatingKey{movieKey(0, 1)}, Situation: core.HomeSituation{Now: testNow, Device: core.DeviceTV}},
		{RowID: "neutral.two", Kind: KindBecause, Rank: 5, Items: []core.RatingKey{movieKey(0, 2)}, Situation: core.HomeSituation{Now: testNow, Device: core.DeviceTV}},
	} {
		h.e.RecordNeutralImpression(user, row)
	}
	h.e.Flush(ctx)
	for _, want := range []struct {
		id, kind string
		rank     int
	}{{"neutral.one", KindTop10, 3}, {"neutral.two", KindBecause, 5}} {
		var kind string
		var rank int
		if err := h.db.QueryRow(`SELECT kind,best_position FROM personalize_row_impressions WHERE hub_id=?`, want.id).Scan(&kind, &rank); err != nil || kind != want.kind || rank != want.rank {
			t.Fatalf("%s: %s %d %v", want.id, kind, rank, err)
		}
		h.e.hubMu.RLock()
		_, exists := h.e.hubKinds[want.id]
		h.e.hubMu.RUnlock()
		if exists {
			t.Fatal("neutral event modified Plex kind registry")
		}
	}
}

func TestSemanticRowKindUsesRegisteredProducer(t *testing.T) {
	h := newHarness(t, nil)
	h.e.registerHub("neutral-id", KindBecause)
	if got := h.e.SemanticRowKind("neutral-id"); got != KindBecause {
		t.Fatalf("got %q", got)
	}
	if got := h.e.SemanticRowKind("home.continue"); got != KindContinue {
		t.Fatalf("fallback %q", got)
	}
}
