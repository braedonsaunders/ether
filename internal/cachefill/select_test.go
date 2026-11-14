package cachefill

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

type selectionCatalog struct {
	items        []core.Title
	children     map[core.RatingKey][]core.Title
	calls        int
	failChildren bool
}

func (c *selectionCatalog) Browse(_ context.Context, q core.BrowseQuery) (core.Page, error) {
	var v []core.Title
	for _, t := range c.items {
		if t.Type == q.Filter.Type {
			v = append(v, t)
		}
	}
	if q.Offset >= len(v) {
		return core.Page{Total: len(v)}, nil
	}
	return core.Page{Items: v[q.Offset:min(q.Offset+q.Limit, len(v))], Total: len(v)}, nil
}
func (c *selectionCatalog) CachedTitle(_ context.Context, k core.RatingKey) (*core.Title, error) {
	for _, t := range c.items {
		if t.Key == k {
			return &t, nil
		}
	}
	return nil, core.ErrNotFound
}
func (c *selectionCatalog) Children(_ context.Context, k core.RatingKey) ([]core.Title, error) {
	c.calls++
	if c.failChildren {
		return nil, errors.New("metadata unavailable")
	}
	return append([]core.Title(nil), c.children[k]...), nil
}

type watchFake struct {
	history  map[int64][]core.HistoryEntry
	progress map[int64][]core.WatchStatus
	fail     bool
}

func (w *watchFake) History(_ context.Context, id int64, _ time.Time, _ int) ([]core.HistoryEntry, error) {
	if w.fail {
		return nil, errors.New("database unavailable")
	}
	return w.history[id], nil
}
func (w *watchFake) InProgress(_ context.Context, id int64, _ int) ([]core.WatchStatus, error) {
	return w.progress[id], nil
}
func selectionHarness(t *testing.T) (*Manager, *config.Cache, *selectionCatalog, *watchFake, time.Time) {
	t.Helper()
	m, c, _, _ := harness(t)
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	cat := &selectionCatalog{children: map[core.RatingKey][]core.Title{}}
	w := &watchFake{history: map[int64][]core.HistoryEntry{1: {{Key: core.MovieKey(999), ViewedAt: now}}}, progress: map[int64][]core.WatchStatus{}}
	m.deps.Catalog = cat
	m.deps.State = w
	m.deps.Now = func() time.Time { return now }
	m.deps.Household = func(context.Context, string) ([]core.User, error) { return []core.User{{ID: 1}, {ID: 2}}, nil }
	c.PreloadSource = "smart"
	c.PreloadContent = "mixed"
	return m, c, cat, w, now
}
func movie(id int64, pop float64, now time.Time) core.Title {
	return core.Title{Key: core.MovieKey(id), Title: fmt.Sprint("Movie ", id), Type: core.TypeMovie, Released: now.Add(-24 * time.Hour), Popularity: pop}
}
func series(cat *selectionCatalog, id int64, pop float64, now time.Time) core.Title {
	show := core.Title{Key: core.ShowKey(id), Title: fmt.Sprint("Show ", id), Type: core.TypeShow, Released: now.AddDate(-1, 0, 0), Popularity: pop}
	cat.items = append(cat.items, show)
	for s := 1; s <= 2; s++ {
		season := core.Title{Key: core.SeasonKey(id, s), Type: core.TypeSeason, Season: s, Released: now.Add(-24 * time.Hour)}
		cat.children[show.Key] = append(cat.children[show.Key], season)
		for ep := 1; ep <= 4; ep++ {
			cat.children[season.Key] = append(cat.children[season.Key], core.Title{Key: core.EpisodeKey(id, s, ep), Type: core.TypeEpisode, ShowKey: show.Key, Season: s, Episode: ep, Title: fmt.Sprint("Episode ", ep), Released: now.Add(-time.Hour)})
		}
	}
	return show
}
func completed(w *watchFake, user int64, k core.RatingKey, at time.Time) {
	w.history[user] = append(w.history[user], core.HistoryEntry{Key: k, ViewedAt: at, Completed: true})
}
func TestBlendBalancesPopularityWithRecentHouseholdFrequency(t *testing.T) {
	m, c, cat, w, now := selectionHarness(t)
	popular, favourite := movie(1, 1000, now), movie(2, 3, now)
	cat.items = []core.Title{popular, favourite}
	for u := int64(1); u <= 2; u++ {
		for d := 0; d < 8; d++ {
			completed(w, u, favourite.Key, now.Add(-time.Duration(d)*24*time.Hour))
		}
	}
	plan, err := m.selectCandidates(context.Background(), *c, 10)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Candidates[0].Title.Key != favourite.Key {
		t.Fatal("household favourite lost to pure popularity", plan.Candidates)
	}
	zero := 0
	c.PreloadHouseholdWeight = &zero
	plan, err = m.selectCandidates(context.Background(), *c, 10)
	if err != nil || plan.Candidates[0].Title.Key != popular.Key {
		t.Fatal("zero household weight ignored", err, plan)
	}
	hundred := 100
	c.PreloadHouseholdWeight = &hundred
	c.PreloadSource = "popular"
	plan, _ = m.selectCandidates(context.Background(), *c, 10)
	if plan.Candidates[0].Title.Key != popular.Key {
		t.Fatal("explicit popularity mode ignored")
	}
}
func TestFrequencyCountsCompletionsOnceAndDecaysOldViewing(t *testing.T) {
	m, c, cat, w, now := selectionHarness(t)
	a, b := movie(1, 10, now), movie(2, 10, now)
	cat.items = []core.Title{a, b}
	completed(w, 1, a.Key, now.Add(-time.Hour))
	completed(w, 1, b.Key, now.Add(-80*24*time.Hour))
	w.history[1] = append(w.history[1], core.HistoryEntry{Key: a.Key, ViewedAt: now.Add(-2 * time.Hour)}, core.HistoryEntry{Key: b.Key, ViewedAt: now.Add(-100 * 24 * time.Hour), Completed: true})
	signals, err := m.learn(context.Background(), *c)
	if err != nil {
		t.Fatal(err)
	}
	before := signals.frequency[a.Key]
	w.history[1] = w.history[1][:2]
	signals, _ = m.learn(context.Background(), *c)
	if signals.frequency[a.Key] != before {
		t.Fatal("start event inflated frequency")
	}
	if signals.frequency[a.Key] <= signals.frequency[b.Key]*3 {
		t.Fatal("old viewing did not decay")
	}
	// One bulk scrobble is bounded; a second viewer supplies independent support.
	for i := 0; i < 1000; i++ {
		completed(w, 1, a.Key, now.Add(-time.Minute))
	}
	signals, _ = m.learn(context.Background(), *c)
	if signals.frequency[a.Key] > 1 || signals.viewers[a.Key] != 1 {
		t.Fatal("one viewer dominated", signals)
	}
	completed(w, 2, a.Key, now.Add(-time.Minute))
	signals, _ = m.learn(context.Background(), *c)
	if signals.viewers[a.Key] != 2 {
		t.Fatal("household breadth missing")
	}
}
func TestNoHistoryFallsBackToCatalogueAndGenreAffinityFindsUnseenTitles(t *testing.T) {
	m, c, cat, w, now := selectionHarness(t)
	a, b, d := movie(1, 100, now), movie(2, 20, now), movie(3, 20, now)
	a.Genres = []string{"Drama"}
	b.Genres = []string{"Drama"}
	d.Genres = []string{"Horror"}
	cat.items = []core.Title{a, b, d}
	plan, err := m.selectCandidates(context.Background(), *c, 10)
	if err != nil || plan.Candidates[0].Title.Key != a.Key {
		t.Fatal("cold household broke catalogue fallback", err)
	}
	completed(w, 1, a.Key, now.Add(-time.Hour))
	plan, _ = m.selectCandidates(context.Background(), *c, 10)
	if plan.Candidates[1].Title.Key != b.Key || !strings.Contains(plan.Candidates[1].Reason, "genre") {
		t.Fatal("unseen affinity missing", plan)
	}
}
func TestTVWindowMergesViewersResumesAndCrossesSeasonBoundary(t *testing.T) {
	m, c, cat, w, now := selectionHarness(t)
	show := series(cat, 10, 100, now)
	c.PreloadContent = "tv"
	c.PreloadEpisodesAhead = 3
	completed(w, 1, core.EpisodeKey(10, 1, 4), now.Add(-time.Hour))
	w.progress[2] = []core.WatchStatus{{Key: core.EpisodeKey(10, 1, 2), ViewOffset: time.Minute, LastViewedAt: now.Add(-2 * time.Hour)}}
	// Unknown/future air dates and specials must never become full-source jobs.
	cat.children[show.Key] = append(cat.children[show.Key], core.Title{Key: core.SeasonKey(10, 0), Season: 0, Released: now.Add(-time.Hour)})
	season2 := core.SeasonKey(10, 2)
	cat.children[season2][2].Released = now.Add(24 * time.Hour)
	cat.children[season2][3].Released = time.Time{}
	plan, err := m.selectCandidates(context.Background(), *c, 10)
	if err != nil {
		t.Fatal(err)
	}
	expected := []core.RatingKey{core.EpisodeKey(10, 2, 1), core.EpisodeKey(10, 1, 2), core.EpisodeKey(10, 2, 2)}
	if len(plan.Candidates) != len(expected) {
		t.Fatal(plan)
	}
	for i, k := range expected {
		if plan.Candidates[i].Title.Key != k {
			t.Fatalf("window %d=%v want %v", i, plan.Candidates[i].Title.Key, k)
		}
	}
	if !strings.Contains(plan.Candidates[1].Reason, "in-progress") || !strings.Contains(plan.Candidates[0].Selection.Title, "Show 10") {
		t.Fatal("episode explanation missing", plan)
	}
}
func TestPopularNewShowPreloadsOpeningAndTVOnlyExcludesMovies(t *testing.T) {
	m, c, cat, _, now := selectionHarness(t)
	series(cat, 10, 100, now)
	cat.items = append(cat.items, movie(1, 1000, now))
	c.PreloadContent = "tv"
	c.PreloadMaxTitles = 2
	if err := m.pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m.Status().Ready != 2 {
		t.Fatal(m.Status())
	}
	for _, it := range m.Status().Items {
		if it.Key.Kind() != core.KindEpisode || it.Key.Season() != 1 || it.Key.Episode() > 2 {
			t.Fatal("wrong series start", it)
		}
	}
	c.PreloadContent = "movies"
	if err := m.pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, it := range m.Status().Items {
		if it.Key.Kind() != core.KindMovie {
			t.Fatal("content filter left episode protected", it)
		}
	}
}
func TestEpisodeShelfSlidesWithoutDeletingOldBytes(t *testing.T) {
	m, c, cat, w, now := selectionHarness(t)
	series(cat, 10, 100, now)
	c.PreloadContent = "tv"
	c.PreloadMaxTitles = 10
	c.PreloadEpisodesAhead = 2
	if err := m.pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	old := core.EpisodeKey(10, 1, 1)
	completed(w, 1, old, now.Add(-time.Minute))
	if err := m.pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(m.Status().Items) != 2 {
		t.Fatal("window accumulated episodes", m.Status())
	}
	for _, it := range m.Status().Items {
		if it.Key == old {
			t.Fatal("completed episode still reserved")
		}
	}
	if m.deps.Cache.ReleaseBytes(fmt.Sprint(old)) == 0 {
		t.Fatal("rotation deleted cached bytes")
	}
	if len(m.deps.Cache.Pins()[fmt.Sprint(old)]) != 0 {
		t.Fatal("old episode pin survived")
	}
	cat.failChildren = true
	if err := m.pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(m.Status().Items) != 2 {
		t.Fatal("metadata outage released valid shelf")
	}
}
func TestHouseholdFailureDoesNotRotateExistingShelf(t *testing.T) {
	m, c, cat, w, now := selectionHarness(t)
	cat.items = []core.Title{movie(1, 100, now)}
	if err := m.pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	w.fail = true
	if err := m.pass(context.Background()); err == nil {
		t.Fatal("history failure silently changed ranking")
	}
	if m.Status().Ready != 1 {
		t.Fatal("history outage discarded shelf")
	}
	_ = c
}

// Priority is lexicographic, including eviction under a tight byte allocation:
// neither an owner blend weight of zero nor extreme popularity can outrank
// continuation or a pin, and idle viewers never start catalogue work.
func TestWarmPriorityUnderTightBudget(t *testing.T) {
	m, c, cat, w, now := selectionHarness(t)
	show := series(cat, 10, 0.01, now)
	pinned, popular := movie(2, .01, now), movie(3, 1e9, now)
	cat.items = append(cat.items, pinned, popular)
	c.PreloadEpisodesAhead = 1
	c.PreloadBudgetBytes = 2 * (32<<20 + 32) // exactly two fixture sources plus overhead
	c.PreloadMaxTitles = 10
	c.PreloadSource = "popular"
	completed(w, 1, core.EpisodeKey(10, 1, 1), now.Add(-time.Minute))
	// First fill with speculative movies so priority must also displace old pins.
	saved := cat.items
	cat.items = []core.Title{pinned, popular}
	if err := m.pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	cat.items = saved
	c.KeepWarmTitles = []core.RatingKey{pinned.Key}
	plan, err := m.selectCandidates(context.Background(), *c, 10)
	if err != nil {
		t.Fatal(err)
	}
	next := core.EpisodeKey(show.Key.TMDB(), 1, 2)
	if len(plan.Candidates) != 3 || plan.Candidates[0].Key != next || plan.Candidates[1].Key != pinned.Key || plan.Candidates[2].Key != popular.Key {
		t.Fatalf("wrong priority: %+v", plan.Candidates)
	}
	if err := m.pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	items := m.Status().Items
	if m.Status().Ready != 2 {
		t.Fatalf("tight allocation: %+v", items)
	}
	for _, it := range items {
		if it.ReleaseID != "" && it.Key != next && it.Key != pinned.Key {
			t.Fatalf("speculation displaced priority title: %+v", it)
		}
	}
	// Explicit watchlist events are below pins but above any recommendation,
	// sorted newest first, and a watched show starts at that viewer's next ep.
	watchShow := series(cat, 20, .001, now)
	older := movie(4, .001, now)
	cat.items = append(cat.items, older)
	completed(w, 1, core.EpisodeKey(20, 1, 2), now.Add(-20*24*time.Hour))
	intents := []WarmIntent{{UserID: 1, User: core.User{ID: 1}, Key: older.Key, Added: now.Add(-time.Hour)}, {UserID: 1, User: core.User{ID: 1}, Key: watchShow.Key, Added: now}}
	m.deps.WatchlistIntents = func(context.Context) ([]WarmIntent, error) { return intents, nil }
	m.deps.WatchlistDone = func(_ context.Context, done WarmIntent) error {
		for i, it := range intents {
			if it.Key == done.Key {
				intents = append(intents[:i], intents[i+1:]...)
				break
			}
		}
		return nil
	}
	plan, err = m.selectCandidates(context.Background(), *c, 10)
	watchNext := core.EpisodeKey(20, 1, 3)
	if err != nil || len(plan.Candidates) < 5 || plan.Candidates[0].Key != next || plan.Candidates[1].Key != pinned.Key || plan.Candidates[2].Key != watchNext || plan.Candidates[3].Key != older.Key {
		t.Fatalf("watchlist priority/continuation: %+v %v", plan, err)
	}
	c.PreloadBudgetBytes = 3 * (32<<20 + 32)
	if err := m.pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, it := range m.Status().Items {
		if it.ReleaseID != "" && it.Key != next && it.Key != pinned.Key && it.Key != watchNext {
			t.Fatalf("speculation displaced watchlist: %+v", it)
		}
	}
	if len(intents) != 1 || intents[0].Key != older.Key {
		t.Fatalf("intent wasn't consumed once: %+v", intents)
	}
	// A cold show starts at the pilot, and an inactive viewer is discarded.
	w.history = nil
	w.progress = nil
	cold := series(cat, 30, 1, now)
	w.history = map[int64][]core.HistoryEntry{1: {{Key: core.MovieKey(999), ViewedAt: now}}}
	coldEvents := []WarmIntent{{User: core.User{ID: 1}, Key: cold.Key, Added: now}}
	coldCandidates, err := m.watchlistCandidates(context.Background(), *c, coldEvents)
	if err != nil || len(coldCandidates) != 1 || coldCandidates[0].Key != core.EpisodeKey(30, 1, 1) {
		t.Fatalf("cold watchlist show: %+v %v", coldCandidates, err)
	}
	m.deps.WatchlistIntents = nil
	// Restore the continuation fixture for the speculation-off assertion.
	cat.items = []core.Title{show, pinned, popular}
	completed(w, 1, core.EpisodeKey(10, 1, 1), now.Add(-time.Minute))
	c.SpeculativeWarming = "off"
	plan, err = m.selectCandidates(context.Background(), *c, 10)
	if err != nil || len(plan.Candidates) != 2 {
		t.Fatalf("off lost continuation/pin: %+v %v", plan, err)
	}
	w.history = nil
	w.progress = nil
	c.SpeculativeWarming = "on"
	plan, err = m.selectCandidates(context.Background(), *c, 10)
	if err != nil || len(plan.Candidates) != 1 || plan.Candidates[0].Key != pinned.Key {
		t.Fatalf("idle viewers started speculation: %+v %v", plan, err)
	}
	c.KeepWarmTitles = nil
	plan, err = m.selectCandidates(context.Background(), *c, 10)
	if err != nil || len(plan.Candidates) != 0 {
		t.Fatalf("empty household did work: %+v %v", plan, err)
	}
}
