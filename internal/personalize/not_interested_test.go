package personalize

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/notinterested"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestNotInterestedHomeAndPostPlayInvalidateWithoutChangingContinue(t *testing.T) {
	ctx := context.Background()
	viewer := core.User{ID: 41, Username: "scifi"}
	var rejected []NotInterestedSignal
	h := newHarness(t, func(o *Options) {
		o.NotInterested = func(_ context.Context, u *core.User) ([]NotInterestedSignal, error) {
			if u.ID == viewer.ID {
				return append([]NotInterestedSignal(nil), rejected...), nil
			}
			return nil, nil
		}
	})
	h.users.users = []core.User{viewer}
	for _, j := range []int{14, 15, 16, 17} {
		h.finish(viewer.ID, movieKey(0, j), testNow.AddDate(0, 0, -j))
	}
	h.finish(viewer.ID, movieKey(0, 4), testNow.Add(-2*time.Hour))
	h.rebuild(viewer)
	hc := hubContext(testNow, core.DeviceTV)
	original, err := h.e.HomeCandidatesFor(ctx, &viewer, hc, 12)
	if err != nil {
		t.Fatal(err)
	}
	empty, err := h.e.HomeCandidatesFor(ctx, &viewer, hc, 12)
	if err != nil || !reflect.DeepEqual(original, empty) {
		t.Fatalf("empty exclusions changed Home: %v", err)
	}
	picks, err := h.e.PostPlay(ctx, &viewer, movieKey(0, 4), hc, 12)
	if err != nil || len(picks) == 0 {
		t.Fatalf("missing postplay: %v", err)
	}
	target := picks[0].Title.Key
	rejected = []NotInterestedSignal{{Key: target, AddedAt: testNow}}
	filtered, err := h.e.PostPlay(ctx, &viewer, movieKey(0, 4), hc, 12)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range filtered {
		if p.Title.Key == target {
			t.Fatal("cached postplay leaked exclusion")
		}
	}
	rows, err := h.e.HomeCandidatesFor(ctx, &viewer, hc, 12)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.ID == "home.continue" {
			continue
		}
		for _, item := range row.Items {
			if item.Key == target {
				t.Fatal("Home leaked excluded recommendation")
			}
		}
	}
	rejected = nil
	restored, err := h.e.PostPlay(ctx, &viewer, movieKey(0, 4), hc, 12)
	if err != nil || !reflect.DeepEqual(restored, picks) {
		t.Fatalf("undo did not restore postplay: %v", err)
	}
}

func TestNotInterestedRevisionRejectsStaleConcurrentBuildsAndPeers(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	var revision uint64
	var signals []NotInterestedSignal
	var block bool
	entered, release := make(chan struct{}), make(chan struct{})
	snapshot := func(_ context.Context, _ *core.User) ([]NotInterestedSignal, uint64, error) {
		mu.Lock()
		r := revision
		current := append([]NotInterestedSignal(nil), signals...)
		shouldBlock := block
		if shouldBlock {
			block = false
		}
		mu.Unlock()
		if shouldBlock {
			close(entered)
			<-release
		}
		return current, r, nil
	}
	mutate := func(o *Options) { o.NotInterestedSnapshot = snapshot }
	a, b := newHarness(t, mutate), newHarness(t, mutate)
	viewer := core.User{ID: 13}
	a.rebuild(viewer)
	b.rebuild(viewer)
	mu.Lock()
	block = true
	mu.Unlock()
	result := make(chan error, 1)
	go func() { _, err := a.e.buildAndStore(ctx, viewer.ID); result <- err }()
	<-entered
	mu.Lock()
	revision++
	signals = []NotInterestedSignal{{Key: movieKey(0, 1), AddedAt: testNow}}
	mu.Unlock()
	close(release)
	if err := <-result; err == nil {
		t.Fatal("stale in-flight build was accepted")
	}
	// A peer with a warmed model must never serve the stale preference revision,
	// even before an asynchronous cluster invalidation arrives.
	for _, h := range []*harness{a, b} {
		m := h.e.modelFor(ctx, &viewer, false)
		if m != nil && !m.generic && m.preferenceRevision != 1 {
			t.Fatalf("served stale model revision %d", m.preferenceRevision)
		}
	}
	mu.Lock()
	revision++
	signals = nil
	mu.Unlock()
	set, sig, err := a.e.exclusions(ctx, &viewer)
	if err != nil || len(set) != 0 || sig != "|r2" {
		t.Fatalf("undo lost generation: %+v %q %v", set, sig, err)
	}
	for _, h := range []*harness{a, b} {
		m := h.e.modelFor(ctx, &viewer, false)
		if m != nil && !m.generic && m.preferenceRevision != 2 {
			t.Fatalf("served pre-undo model revision %d", m.preferenceRevision)
		}
	}
}

func TestNotInterestedSharedRevisionAcrossNodesAndUndo(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	viewer := &core.User{ID: 31}
	service, err := notinterested.New(notinterested.Options{DB: db, Active: func(_ context.Context, id int64) (*core.User, error) { return &core.User{ID: id}, nil }, Allowed: func(context.Context, *core.User, core.RatingKey) bool { return true }, Changed: func(context.Context, int64, core.RatingKey) {}})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := func(ctx context.Context, u *core.User) ([]NotInterestedSignal, uint64, error) {
		entries, rev, err := service.Snapshot(ctx, u)
		signals := make([]NotInterestedSignal, 0, len(entries))
		for _, entry := range entries {
			signals = append(signals, NotInterestedSignal{Key: entry.Key, AddedAt: entry.AddedAt})
		}
		return signals, rev, err
	}
	makeNode := func() *harness { return newHarness(t, func(o *Options) { o.NotInterestedSnapshot = snapshot }) }
	a, b := makeNode(), makeNode()
	a.rebuild(*viewer)
	b.rebuild(*viewer)
	_, before, err := a.e.exclusions(ctx, viewer)
	if err != nil || before != "" {
		t.Fatalf("initial signature: %q %v", before, err)
	}
	key := movieKey(0, 1)
	if changed, err := service.Set(ctx, viewer, key, true); err != nil || !changed {
		t.Fatalf("set: %v %v", changed, err)
	}
	for _, node := range []*harness{a, b} {
		set, sig, err := node.e.exclusions(ctx, viewer)
		if err != nil || !set[key] || sig == before {
			t.Fatalf("peer missed committed change: %v %q %v", set, sig, err)
		}
		model := node.e.modelFor(ctx, viewer, false)
		if model != nil && !model.generic && model.preferenceRevision != 1 {
			t.Fatalf("peer served stale revision %d", model.preferenceRevision)
		}
	}
	if changed, err := service.Set(ctx, viewer, key, false); err != nil || !changed {
		t.Fatalf("undo: %v %v", changed, err)
	}
	for _, node := range []*harness{a, b} {
		set, sig, err := node.e.exclusions(ctx, viewer)
		if err != nil || len(set) != 0 || sig == before || sig != "|r2" {
			t.Fatalf("peer reused pre-set cache: %v %q %v", set, sig, err)
		}
		model := node.e.modelFor(ctx, viewer, false)
		if model != nil && !model.generic && model.preferenceRevision != 2 {
			t.Fatalf("peer served pre-undo revision %d", model.preferenceRevision)
		}
	}
}

func TestNotInterestedModelInvalidatedWhenVisibleSignalsChange(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	visible := true
	snapshot := func(context.Context, *core.User) ([]NotInterestedSignal, uint64, error) {
		mu.Lock()
		defer mu.Unlock()
		if visible {
			return []NotInterestedSignal{{Key: movieKey(0, 1)}}, 1, nil
		}
		return nil, 1, nil
	}
	h := newHarness(t, func(o *Options) { o.NotInterestedSnapshot = snapshot })
	viewer := core.User{ID: 19}
	h.rebuild(viewer)
	mu.Lock()
	visible = false
	mu.Unlock()
	m := h.e.modelFor(ctx, &viewer, false)
	if m != nil && !m.generic && m.preferenceSignature != "|r1" {
		t.Fatalf("policy change reused stale model: %q", m.preferenceSignature)
	}
}

func TestNotInterestedUnavailableDoesNotExposeRecommendations(t *testing.T) {
	viewer := &core.User{ID: 7}
	h := newHarness(t, func(o *Options) {
		o.NotInterested = func(context.Context, *core.User) ([]NotInterestedSignal, error) {
			return nil, errors.New("store unavailable")
		}
	})
	rows, err := h.e.HomeCandidatesFor(context.Background(), viewer, hubContext(testNow, core.DeviceTV), 12)
	if err != nil || len(rows) != 0 {
		t.Fatalf("failed exclusion lookup exposed rows: %+v %v", rows, err)
	}
	if _, err := h.e.PostPlay(context.Background(), viewer, movieKey(0, 4), hubContext(testNow, core.DeviceTV), 12); err == nil {
		t.Fatal("postplay masked exclusion failure")
	}
}

func TestNotInterestedExactEpisodeShowAndStrongTaste(t *testing.T) {
	show := core.ShowKey(42)
	episode := core.EpisodeKey(42, 1, 1)
	sibling := core.EpisodeKey(42, 1, 2)
	item := core.Title{Key: episode, ShowKey: show, Type: core.TypeEpisode}
	if !(excludedTitles{show: true}).contains(&item) || (excludedTitles{episode: true}).contains(&core.Title{Key: sibling, ShowKey: show}) {
		t.Fatal("show or episode exclusion scope wrong")
	}
	movie := core.Title{Key: core.MovieKey(5), Type: core.TypeMovie, Genres: []string{"Drama"}}
	taste := buildTaste(tasteInput{now: testNow, loc: time.UTC, halfLife: 120 * 24 * time.Hour, titles: map[core.RatingKey]core.Title{movie.Key: movie}, notInterested: []NotInterestedSignal{{Key: movie.Key, AddedAt: testNow}}})
	if len(taste.Evidence) != 1 || taste.Evidence[0].Kind != "notInterested" || taste.Evidence[0].Weight != -1.8 || taste.Genre["Drama"] >= 0 {
		t.Fatalf("negative taste missing: %+v", taste)
	}
}

func TestNotInterestedFiltersBeforeRankAndKeepsContinue(t *testing.T) {
	cat := newFakeCatalog(testNow)
	keys := []core.RatingKey{movieKey(0, 1), movieKey(0, 2), movieKey(0, 3)}
	row := testRow(KindTop10, "top", items(cat, keys...))
	row.keepOrder = true
	row.source = core.RowSourceEngine
	a := &assembly{model: emptyModel(row), user: core.User{ID: 1}, hc: hubContext(testNow, core.DeviceTV), limit: 2, excluded: excludedTitles{keys[0]: true}}
	got := a.contextItems(row)
	if len(got) != 2 || got[0].t.Key != keys[1] || got[1].t.Key != keys[2] {
		t.Fatalf("did not backfill before ranking: %+v", got)
	}
	row.kind = KindContinue
	got = a.contextItems(row)
	if len(got) != 3 || got[0].t.Key != keys[0] {
		t.Fatalf("Continue was filtered: %+v", got)
	}
	row.kind = KindTop10
	row.source = core.RowSourceOwner
	got = a.contextItems(row)
	if len(got) != 3 || got[0].t.Key != keys[0] {
		t.Fatalf("owner-curated collection was filtered: %+v", got)
	}
}

func TestNotInterestedRediscoverBackfillsWithoutHidingContinue(t *testing.T) {
	h := newHarness(t, nil)
	viewer := &core.User{ID: 1}
	first, second := movieKey(0, 1), movieKey(0, 2)
	h.start(viewer.ID, first, 0.2, testNow.AddDate(0, 0, -31))
	h.start(viewer.ID, second, 0.2, testNow.AddDate(0, 0, -32))
	d := NewDiscovery(h.db, h.cat, h.st, nil)
	d.now = func() time.Time { return testNow }
	d.NotInterested = func(context.Context, *core.User) ([]NotInterestedSignal, error) {
		return []NotInterestedSignal{{Key: first}}, nil
	}
	row, err := d.Rediscover(context.Background(), viewer, 1)
	if err != nil || len(row.Items) != 1 || row.Items[0].Key != second {
		t.Fatalf("rediscover did not backfill: %+v %v", row, err)
	}
	title, err := h.cat.Get(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	if got := d.FilterContinue(context.Background(), viewer, []core.Title{*title}); len(got) != 1 || got[0].Key != first {
		t.Fatalf("Continue changed: %+v", got)
	}
}

func TestNotInterestedReviewRowDoesNotAlterDirectCollection(t *testing.T) {
	h := newHarness(t, nil)
	viewer := &core.User{ID: 1}
	first, second := movieKey(0, 1), movieKey(0, 2)
	for i, key := range []core.RatingKey{first, second} {
		h.at(time.Date(2025, 5, 1+i, 12, 0, 0, 0, time.UTC), func() {
			if err := h.st.Scrobble(context.Background(), viewer.ID, key); err != nil {
				t.Fatal(err)
			}
		})
	}
	d := NewDiscovery(h.db, h.cat, h.st, nil)
	d.now = func() time.Time { return testNow }
	d.NotInterested = func(context.Context, *core.User) ([]NotInterestedSignal, error) {
		return []NotInterestedSignal{{Key: second}}, nil
	}
	hubs, err := d.Hubs(context.Background(), viewer, 1)
	if err != nil || len(hubs) != 1 || len(hubs[0].Items) != 1 || hubs[0].Items[0].Key != first {
		t.Fatalf("review did not backfill: %+v %v", hubs, err)
	}
	history, err := d.YearInReview(context.Background(), viewer, 2025, 10)
	if err != nil || len(history.Items) != 2 {
		t.Fatalf("direct collection history changed: %+v %v", history, err)
	}
}
