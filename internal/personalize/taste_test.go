package personalize

import (
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func approx(a, b, eps float64) bool { return math.Abs(a-b) <= eps }

func baseInput(cat *fakeCatalog) tasteInput {
	return tasteInput{userID: 1, now: testNow, loc: time.UTC, statuses: map[core.RatingKey]core.WatchStatus{},
		titles: map[core.RatingKey]core.Title{}, shownDays: map[core.RatingKey]int{}}
}

// completed adds a finished viewing of key at `at` to the input.
func (in *tasteInput) completed(cat *fakeCatalog, key core.RatingKey, at time.Time) {
	in.history = append([]core.HistoryEntry{{Key: key, ViewedAt: at, Completed: true}}, in.history...)
	ws := in.statuses[key]
	ws.Key, ws.ViewCount, ws.LastViewedAt, ws.Duration = key, ws.ViewCount+1, at, cat.titles[key].Runtime
	in.statuses[key] = ws
	tk := tasteKey(key)
	in.titles[tk] = cat.titles[tk]
}

func TestTasteCompletionDecayAndNormalisation(t *testing.T) {
	cat := newFakeCatalog(testNow)
	in := baseInput(cat)
	sf := testGenres[0]
	in.completed(cat, movieKey(0, 1), testNow)                     // today: full weight
	in.completed(cat, movieKey(0, 2), testNow.AddDate(0, 0, -120)) // one half-life ago
	ts := buildTaste(in)

	ev := ts.evidenceFor(movieKey(0, 1))
	if ev == nil || ev.Kind != EvidenceFinished || !approx(ev.Weight, 1.0, 1e-9) {
		t.Fatalf("today's completion evidence = %+v, want weight 1", ev)
	}
	old := ts.evidenceFor(movieKey(0, 2))
	if old == nil || !approx(old.Weight, 0.5, 1e-6) {
		t.Fatalf("120-day-old completion weight = %+v, want 0.5 (120-day half-life)", old)
	}
	// Raw genre mass 1.5 is below the floor (3), so the weight is 0.5.
	if g := ts.Genre[sf]; !approx(g, 0.5, 1e-6) {
		t.Fatalf("genre weight = %v, want 0.5", g)
	}
	// The director of both films carries their combined weight / floor.
	if p := ts.Person[sciFiDirector]; !approx(p, 0.75, 1e-6) {
		t.Fatalf("director weight = %v, want 0.75", p)
	}
	if info := ts.People[sciFiDirector]; info.role != "director" || info.name != "Vera Villeneuve" {
		t.Fatalf("director info = %+v", info)
	}
	if ts.Decade[1970] <= 0 {
		t.Fatalf("decade affinities missing: %v", ts.Decade)
	}
	if !approx(ts.Confidence, 1-math.Exp(-1.5/5), 1e-9) {
		t.Fatalf("confidence = %v", ts.Confidence)
	}

	// A heavy history saturates the family at 1.0 and keeps others relative.
	for j := 3; j < 12; j++ {
		in.completed(cat, movieKey(0, j), testNow.AddDate(0, 0, -j))
	}
	in.completed(cat, movieKey(1, 0), testNow.AddDate(0, 0, -1))
	ts = buildTaste(in)
	if g := ts.Genre[sf]; !approx(g, 1, 1e-9) {
		t.Fatalf("dominant genre = %v, want 1", g)
	}
	if d := ts.Genre["Drama"]; d <= 0 || d >= 0.5 {
		t.Fatalf("minor genre = %v, want a small positive weight", d)
	}
	if top := ts.TopGenres(1); len(top) != 1 || top[0] != sf {
		t.Fatalf("TopGenres = %v", top)
	}
}

func TestTasteSignals(t *testing.T) {
	cat := newFakeCatalog(testNow)
	in := baseInput(cat)
	horror := movieKey(3, 1)
	comedy := movieKey(2, 1)
	drama := movieKey(1, 1)
	romance := movieKey(7, 1)
	sciShow := showKey(0, 0)
	for _, k := range []core.RatingKey{horror, comedy, drama, romance, sciShow} {
		in.titles[k] = cat.titles[k]
	}
	// Abandoned play (stopped at 4%, never resumed).
	in.plays = []playRecord{{userID: 1, key: horror, tasteKey: horror, started: testNow.Add(-time.Hour), updated: testNow.Add(-time.Hour), maxFraction: 0.04, abandoned: true}}
	in.history = append(in.history, core.HistoryEntry{Key: horror, ViewedAt: testNow.Add(-time.Hour)})
	// Rewatched twice and rated 9.
	in.completed(cat, comedy, testNow.AddDate(0, 0, -30))
	in.completed(cat, comedy, testNow)
	ws := in.statuses[comedy]
	ws.UserRating = 9
	in.statuses[comedy] = ws
	// Stalled at 20% for a month: abandoned.
	in.statuses[drama] = core.WatchStatus{Key: drama, ViewOffset: 20 * time.Minute, Duration: 100 * time.Minute, LastViewedAt: testNow.AddDate(0, -1, 0)}
	// Shown on five days, never played.
	in.shownDays[romance] = 5
	// A binge: six episodes of one show finished today.
	for e := 1; e <= 6; e++ {
		in.completed(cat, core.EpisodeKey(sciShow.TMDB(), 1, e), testNow.Add(time.Duration(e)*30*time.Minute-6*time.Hour))
	}
	ts := buildTaste(in)

	if ev := ts.evidenceFor(horror); ev == nil || ev.Kind != EvidenceAbandoned || ev.Weight >= 0 {
		t.Fatalf("abandon evidence = %+v", ev)
	}
	c := ts.evidenceFor(comedy)
	if c == nil || !c.Rewatched || c.Completions != 2 {
		t.Fatalf("rewatch evidence = %+v", c)
	}
	// 1.0 + 0.3 (second viewing) + (9-5)/5*1.2 = 2.26, clamped to 2.
	if !approx(c.Weight, 2, 1e-9) || !approx(c.Raw, 2.26, 1e-9) {
		t.Fatalf("rewatched+rated weight = %v (raw %v)", c.Weight, c.Raw)
	}
	if ev := ts.evidenceFor(drama); ev == nil || ev.Kind != EvidenceAbandoned || ev.Weight >= 0 {
		t.Fatalf("stalled evidence = %+v", ev)
	}
	if ev := ts.evidenceFor(romance); ev == nil || ev.Kind != EvidenceIgnored || !approx(ev.Weight, -0.18, 1e-9) {
		t.Fatalf("ignored evidence = %+v", ev)
	}
	show := ts.evidenceFor(core.EpisodeKey(sciShow.TMDB(), 1, 3))
	// 6 episodes: 0.6 + 0.4 + min(0.1*3, 0.8) = 1.3.
	if show == nil || show.Key != sciShow || show.Completions != 6 || !approx(show.Weight, 1.3, 0.01) {
		t.Fatalf("show evidence = %+v", show)
	}
	if ts.Genre["Horror"] >= 0 || ts.Genre["Romance"] >= 0 {
		t.Fatalf("negative evidence should push genres negative: horror %v romance %v", ts.Genre["Horror"], ts.Genre["Romance"])
	}
	if ts.Habits.BingeScore != 1 {
		t.Fatalf("binge score = %v, want 1 (all episodes in one run)", ts.Habits.BingeScore)
	}
	if ts.Habits.RewatchScore <= 0 {
		t.Fatalf("rewatch score = %v", ts.Habits.RewatchScore)
	}
}

func TestTasteOverridesAndFit(t *testing.T) {
	cat := newFakeCatalog(testNow)
	in := baseInput(cat)
	for j := 0; j < 6; j++ {
		in.completed(cat, movieKey(3, j), testNow.AddDate(0, 0, -j)) // a horror fan…
	}
	ts := buildTaste(in)
	scary := cat.titles[movieKey(3, 10)]
	fitBefore, why := ts.Fit(&scary)
	if fitBefore <= 0.3 || why == "" {
		t.Fatalf("fit before override = %v %q", fitBefore, why)
	}
	// …whose owner says they're not into horror any more.
	in.overrides = []core.TasteOverride{{Kind: "genre", Value: "horror", Weight: -1}, {Kind: "person", Value: "700", Weight: 0.9}}
	ts = buildTaste(in)
	if ts.Genre["Horror"] != -1 {
		t.Fatalf("override not applied: %v", ts.Genre["Horror"])
	}
	if ts.Person[700] != 0.9 {
		t.Fatalf("person override = %v", ts.Person[700])
	}
	fitAfter, _ := ts.Fit(&scary)
	if fitAfter >= 0 {
		t.Fatalf("fit after override = %v, want negative", fitAfter)
	}
	p := ts.Profile(func(path string, w int) string { return "img:" + path })
	if len(p.Overrides) != 2 || p.Genres[0].Name != "Horror" || p.Genres[0].Weight != -1 {
		t.Fatalf("profile = %+v", p)
	}
	for _, person := range p.People {
		if person.ID == 600+3*10 && person.Image != "img:/p630.jpg" {
			t.Fatalf("person image = %q", person.Image)
		}
	}
	// Combined TV genres share film vocabulary.
	if g := normGenres([]string{"Sci-Fi & Fantasy", "Drama", "Science Fiction"}); len(g) != 3 || g[0] != "Science Fiction" || g[1] != "Fantasy" {
		t.Fatalf("normGenres = %v", g)
	}
}

func TestHabitsAndContextLearning(t *testing.T) {
	cat := newFakeCatalog(testNow)
	in := baseInput(cat)
	// Weeknights (Mon–Thu 21:00): one 24-minute comedy episode each.
	comedyShow := showKey(2, 0)
	for d := 0; d < 4; d++ {
		at := time.Date(2026, 10, 5+d, 21, 0, 0, 0, time.UTC) // Mon 5th … Thu 8th
		in.completed(cat, core.EpisodeKey(comedyShow.TMDB(), 1, d+1), at)
	}
	// Weekend afternoons: family films.
	for d, day := range []int{10, 11} { // Sat, Sun
		in.completed(cat, movieKey(5, d), time.Date(2026, 10, day, 14, 0, 0, 0, time.UTC))
		in.completed(cat, movieKey(5, d+2), time.Date(2026, 10, day, 16, 0, 0, 0, time.UTC))
	}
	in.plays = []playRecord{{device: core.DeviceTV}, {device: core.DeviceTV}, {device: core.DeviceMobile}}
	ts := buildTaste(in)
	h := ts.Habits
	if h.WeeknightMinutes != 24 {
		t.Fatalf("weeknight minutes = %v, want 24", h.WeeknightMinutes)
	}
	// Each weekend day: two family films (80+0*6 … ) — mean of the days' totals.
	if h.WeekendMinutes < 160 {
		t.Fatalf("weekend minutes = %v", h.WeekendMinutes)
	}
	if h.PreferredDevice != core.DeviceTV {
		t.Fatalf("preferred device = %q", h.PreferredDevice)
	}
	if len(h.PeakHours) == 0 || h.PeakHours[0] != 21 {
		t.Fatalf("peak hours = %v, want 21 first", h.PeakHours)
	}
	if ts.DaypartGenre[core.DaypartAfternoon]["Family"] <= 0 {
		t.Fatalf("afternoon family lift = %v", ts.DaypartGenre[core.DaypartAfternoon])
	}
	wkAfternoon := ts.movieShareFor(core.HubContext{Weekend: true, Daypart: core.DaypartAfternoon})
	wkNight := ts.movieShareFor(core.HubContext{Weekend: false, Daypart: core.DaypartEvening})
	if wkAfternoon <= wkNight {
		t.Fatalf("film share weekend afternoon %v should exceed weeknight %v", wkAfternoon, wkNight)
	}
	// Short-on-time target follows the habit on weeknights.
	weeknight := ts.targetMinutes(core.HubContext{Device: core.DeviceTV, Daypart: core.DaypartEvening})
	weekendTV := ts.targetMinutes(core.HubContext{Device: core.DeviceTV, Daypart: core.DaypartEvening, Weekend: true})
	if weeknight >= 60 || weekendTV <= weeknight {
		t.Fatalf("targets weeknight %v weekend %v", weeknight, weekendTV)
	}
}

func TestTasteDeterministic(t *testing.T) {
	cat := newFakeCatalog(testNow)
	in := baseInput(cat)
	for j := 0; j < 10; j++ {
		in.completed(cat, movieKey(j%4, j), testNow.AddDate(0, 0, -j*3))
	}
	a := buildTaste(in).Profile(nil)
	b := buildTaste(in).Profile(nil)
	if len(a.Genres) != len(b.Genres) {
		t.Fatal("non-deterministic genres")
	}
	for i := range a.Genres {
		if a.Genres[i] != b.Genres[i] {
			t.Fatalf("genre %d differs: %v vs %v", i, a.Genres[i], b.Genres[i])
		}
	}
	for i := range a.People {
		if a.People[i].ID != b.People[i].ID {
			t.Fatalf("people order differs at %d", i)
		}
	}
}

func TestFavoriteTasteSignalAndDecay(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	key := core.MovieKey(4242)
	title := core.Title{Key: key, Type: core.TypeMovie, Genres: []string{"Drama"}}
	in := tasteInput{now: now, halfLife: 120 * 24 * time.Hour, titles: map[core.RatingKey]core.Title{key: title}}
	baseline := buildTaste(in)
	in.favorites = []FavoriteSignal{{Key: key, AddedAt: now.Add(-120 * 24 * time.Hour)}, {PersonID: 123, AddedAt: now}}
	fav := buildTaste(in)
	if len(baseline.Evidence) != 0 || len(fav.Evidence) != 1 || fav.Evidence[0].Kind != "favorite" || fav.Evidence[0].Weight <= 1 || fav.Person[123] <= 0 || fav.Confidence <= baseline.Confidence {
		t.Fatalf("favorite signal: baseline=%+v favorite=%+v", baseline, fav)
	}
}

func TestEmptyFavoritesPreservesTaste(t *testing.T) {
	key := core.MovieKey(77)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	in := tasteInput{now: now, titles: map[core.RatingKey]core.Title{key: {Key: key, Type: core.TypeMovie, Genres: []string{"Drama"}}}, history: []core.HistoryEntry{{Key: key, Completed: true, ViewedAt: now}}}
	original := buildTaste(in)
	in.favorites = []FavoriteSignal{}
	if got := buildTaste(in); !reflect.DeepEqual(original, got) {
		t.Fatal("empty favorites changed taste")
	}
}

func TestFavoriteDoesNotEraseWatchedEvidence(t *testing.T) {
	key := core.MovieKey(78)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	in := tasteInput{now: now, titles: map[core.RatingKey]core.Title{key: {Key: key, Type: core.TypeMovie}}, history: []core.HistoryEntry{{Key: key, Completed: true, ViewedAt: now}}, favorites: []FavoriteSignal{{Key: key, AddedAt: now}}}
	if e := buildTaste(in).evidenceFor(key); e == nil || e.Kind != EvidenceFinished || e.Completions != 1 {
		t.Fatalf("watched evidence lost: %+v", e)
	}
}
