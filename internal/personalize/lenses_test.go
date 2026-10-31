package personalize

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/contentrating"
	"github.com/braedonsaunders/ether/internal/core"
)

// shown counts a kind's rows that have titles.
func shown(m *userModel, kind string) int {
	n := 0
	for _, r := range rowsOfKind(m, kind) {
		if len(r.items) > 0 {
			n++
		}
	}
	return n
}

func rowTitled(m *userModel, title string) *candRow {
	for _, r := range m.rows {
		if r.title == title {
			return r
		}
	}
	return nil
}

func lensModel(t *testing.T, setup func(h *harness, uid int64)) (*harness, *userModel) {
	t.Helper()
	h := newHarness(t, nil)
	u := core.User{ID: 1, Username: "viewer"}
	h.users.users = []core.User{u}
	setup(h, u.ID)
	h.e.syncEvents(context.Background())
	return h, h.rebuild(u)
}

func assertItems(t *testing.T, r *candRow, ok func(*core.Title) bool, what string) {
	t.Helper()
	if r == nil {
		t.Fatalf("no %s row", what)
	}
	for _, it := range r.items {
		if !ok(&it.t) {
			t.Fatalf("%s row %q holds %s", what, r.title, it.t.Title)
		}
		if it.reason == "" {
			t.Fatalf("%s row %q: %s has no reason", what, r.title, it.t.Title)
		}
	}
}

func TestTasteClusterRows(t *testing.T) {
	_, m := lensModel(t, sciFiFan)
	r := rowTitled(m, "Time-Travel Sci-Fi Films")
	assertItems(t, r, func(x *core.Title) bool {
		return x.Type == core.TypeMovie && hasKeyword(x, "time travel") && anyShared(normGenres(x.Genres), []string{"Science Fiction"})
	}, "time-travel cluster")
	if len(r.items) < 5 || !strings.Contains(r.reason, "3 films") || !strings.HasPrefix(r.items[0].reason, "Time-Travel Sci-Fi Films · Rated") {
		t.Fatalf("cluster %q: %d items, reason %q, item %q", r.title, len(r.items), r.reason, r.items[0].reason)
	}
	// A theme that is as common in her viewing as in the catalogue is not
	// a cluster.
	for _, c := range rowsOfKind(m, KindTasteCluster) {
		if strings.Contains(c.title, "Space") || strings.Contains(c.title, "Dystopia") {
			t.Fatalf("undistinctive cluster %q", c.title)
		}
	}

	// Genre pairs name themselves: horror-animation films.
	_, m = lensModel(t, func(h *harness, uid int64) {
		for i, j := range []int{5, 10, 15, 20} {
			h.finish(uid, movieKey(3, j), testNow.AddDate(0, 0, -2-i))
		}
		h.finish(uid, movieKey(3, 1), testNow.AddDate(0, 0, -9))
	})
	r = rowTitled(m, "Animated Horror Films")
	assertItems(t, r, func(x *core.Title) bool {
		gs := normGenres(x.Genres)
		return anyShared(gs, []string{"Horror"}) && anyShared(gs, []string{"Animation"})
	}, "horror × animation cluster")

	// No history, no clusters.
	_, m = lensModel(t, func(*harness, int64) {})
	if shown(m, KindTasteCluster) != 0 {
		t.Fatal("clusters without history")
	}
}

func TestEraRow(t *testing.T) {
	_, m := lensModel(t, sciFiFan)
	r := rowTitled(m, "Best of the '80s for You")
	assertItems(t, r, func(x *core.Title) bool { return x.Year >= 1980 && x.Year <= 1989 && x.Type == core.TypeMovie }, "era")
	if !strings.Contains(r.reason, "3 films from the 1980s") {
		t.Fatalf("era reason %q", r.reason)
	}
	_, m = lensModel(t, func(h *harness, uid int64) { h.finish(uid, movieKey(0, 3), testNow.AddDate(0, 0, -2)) })
	if shown(m, KindEra) != 0 {
		t.Fatal("one film is not an era")
	}
}

func TestWorldCinemaRow(t *testing.T) {
	_, m := lensModel(t, func(h *harness, uid int64) {
		ko := h.cat.addWorldCinema("ko", "South Korea", 0, 12)
		sciFiFan(h, uid)
		for i, k := range ko[:3] {
			h.finish(uid, k, testNow.AddDate(0, 0, -3-i))
		}
	})
	r := rowTitled(m, "Korean Cinema for You")
	assertItems(t, r, func(x *core.Title) bool { return x.Language == "ko" }, "Korean cinema")
	if !strings.Contains(r.reason, "3 Korean films") || !strings.HasPrefix(r.items[0].reason, "Korean · Rated") {
		t.Fatalf("world reasons %q / %q", r.reason, r.items[0].reason)
	}
	// An English-only viewer gets no world cinema row.
	_, m = lensModel(t, sciFiFan)
	if shown(m, KindWorld) != 0 {
		t.Fatal("world cinema for an English-only history")
	}
}

func TestOccasionRows(t *testing.T) {
	_, m := lensModel(t, familyViewer)
	fam := rowTitled(m, "Family Night")
	assertItems(t, fam, func(x *core.Title) bool {
		return anyShared(normGenres(x.Genres), []string{"Family", "Animation"}) && contentrating.Allowed(x.ContentRating, "PG")
	}, "family night")
	if rowTitled(m, "Date Night") != nil {
		t.Fatal("Date Night without romance in the history")
	}
	_, m = lensModel(t, func(h *harness, uid int64) {
		for i, j := range []int{1, 2, 3, 4} {
			h.finish(uid, movieKey(7, j), testNow.AddDate(0, 0, -2-2*i)) // Romance
		}
	})
	date := rowTitled(m, "Date Night")
	assertItems(t, date, func(x *core.Title) bool {
		gs := normGenres(x.Genres)
		mins := minutes(x)
		return anyShared(gs, []string{"Romance"}) && !anyShared(gs, []string{"Animation", "Family"}) && mins >= 85 && mins <= 135
	}, "date night")
	if rowTitled(m, "Family Night") != nil {
		t.Fatal("Family Night without family films in the history")
	}
	// Under 100 Minutes: only short films, and only for a viewer who
	// favours them.
	short := rowTitled(m, "Under 100 Minutes")
	assertItems(t, short, func(x *core.Title) bool { return minutes(x) > 0 && minutes(x) <= 100 }, "under 100")
	_, m = lensModel(t, func(h *harness, uid int64) {
		for i, j := range []int{11, 12, 13} { // 146-, 152-, 158-minute films
			h.finish(uid, movieKey(1, j), testNow.AddDate(0, 0, -2-i))
		}
	})
	if rowTitled(m, "Under 100 Minutes") != nil {
		t.Fatal("Under 100 Minutes for a viewer of long films")
	}
	// The context favours each occasion at its time.
	sat := hubContext(time.Date(2026, 10, 17, 20, 0, 0, 0, time.UTC), core.DeviceTV)
	morning := hubContext(time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC), core.DeviceTV)
	if occasionMult(occasionDateNight, sat) <= occasionMult(occasionDateNight, morning) {
		t.Fatal("date night favours weekend evenings")
	}
}

func TestAcclaimedAndTopGenreRows(t *testing.T) {
	h, m := lensModel(t, sciFiFan)
	b := &builder{e: h.e, ctx: context.Background(), now: testNow, memo: map[string][]core.Title{}}
	r := rowsOfKind(m, KindAcclaimed)
	if len(r) != 1 {
		t.Fatalf("acclaimed rows %d", len(r))
	}
	assertItems(t, r[0], func(x *core.Title) bool { return x.Type == core.TypeMovie && b.acclaimed(x, 1.5, 7.3) }, "acclaimed")
	top := rowTitled(m, "Top-Rated Sci-Fi Films")
	assertItems(t, top, func(x *core.Title) bool {
		return x.Type == core.TypeMovie && anyShared(normGenres(x.Genres), []string{"Science Fiction"})
	}, "top-rated sci-fi")
	_, m = lensModel(t, func(*harness, int64) {})
	if shown(m, KindAcclaimed)+shown(m, KindTopGenre) != 0 {
		t.Fatal("personal lenses without history")
	}
}

func TestNewSeasonsAndFinishRows(t *testing.T) {
	h, m := lensModel(t, func(h *harness, uid int64) {
		tvFan(h, uid)
		// Crime Show 3 (one season, all watched) gets a second season.
		h.cat.addSeason(showKey(9, 3), 2, testNow.AddDate(0, 0, -10))
	})
	ns := rowsOfKind(m, KindNewSeasons)
	if len(ns) != 1 {
		t.Fatalf("new seasons rows %d", len(ns))
	}
	var crime *candItem
	for i := range ns[0].items {
		if ns[0].items[i].t.Key == showKey(9, 3) {
			crime = &ns[0].items[i]
		}
	}
	if crime == nil || crime.reason != "Season 2 premiered 10 days ago" || ns[0].items[0].t.Key != showKey(9, 3) {
		t.Fatalf("new seasons = %+v", ns[0].items)
	}
	fin := rowsOfKind(m, KindFinishShow)
	if len(fin) != 1 {
		t.Fatalf("finish rows %d", len(fin))
	}
	got := map[core.RatingKey]string{}
	for _, it := range fin[0].items {
		got[it.t.Key] = it.reason
	}
	if !strings.HasPrefix(got[showKey(1, 4)], "3 of 16 episodes watched · last watched") {
		t.Fatalf("stalled drama: %q (%v)", got[showKey(1, 4)], got)
	}
	if _, ok := got[showKey(9, 0)]; ok {
		t.Fatal("a series watched this week is not stalled (Up Next covers it)")
	}
	if _, ok := got[showKey(9, 3)]; ok {
		t.Fatal("a series whose only unwatched episodes are a new season belongs to New Seasons")
	}
	// Ordered by progress.
	for i := 1; i < len(fin[0].items); i++ {
		if fin[0].items[i].base > fin[0].items[i-1].base {
			t.Fatal("finish row not ordered by progress")
		}
	}
	// Starting the new season retires it from the row.
	uid := int64(1)
	h.finish(uid, core.EpisodeKey(showKey(9, 3).TMDB(), 2, 1), testNow.AddDate(0, 0, -1))
	h.e.syncEvents(context.Background())
	m = h.rebuild(core.User{ID: uid, Username: "viewer"})
	for _, r := range rowsOfKind(m, KindNewSeasons) {
		for _, it := range r.items {
			if it.t.Key == showKey(9, 3) {
				t.Fatal("started season still offered as new")
			}
		}
	}
	// A film-only viewer gets neither.
	_, m = lensModel(t, sciFiFan)
	if shown(m, KindNewSeasons) != 0 {
		t.Fatal("new seasons for a viewer without a new season")
	}
}

func TestSeriesLensRows(t *testing.T) {
	_, m := lensModel(t, tvFan)
	start := rowsOfKind(m, KindStartNew)
	if len(start) != 1 {
		t.Fatal("no Start Something New")
	}
	assertItems(t, start[0], func(x *core.Title) bool { return x.Type == core.TypeShow }, "start new")
	if !strings.HasPrefix(start[0].items[0].reason, "Start with the pilot") {
		t.Fatalf("start reason %q", start[0].items[0].reason)
	}
	binge := rowsOfKind(m, KindBinge)
	if len(binge) != 1 {
		t.Fatal("no binge row")
	}
	assertItems(t, binge[0], func(x *core.Title) bool { return x.Status == statusEnded && x.ChildCount >= 2 }, "complete series")
	if !strings.HasPrefix(binge[0].items[0].reason, "Complete · ") {
		t.Fatalf("binge reason %q", binge[0].items[0].reason)
	}
	lim := rowsOfKind(m, KindLimited)
	if len(lim) != 1 {
		t.Fatal("no limited series row")
	}
	assertItems(t, lim[0], func(x *core.Title) bool { return limited(x) }, "limited series")
	if !strings.HasPrefix(lim[0].items[0].reason, "Limited series · 8 episodes") {
		t.Fatalf("limited reason %q", lim[0].items[0].reason)
	}
	air := rowsOfKind(m, KindAiringTaste)
	if len(air) != 1 {
		t.Fatal("no ongoing row")
	}
	airing := map[core.RatingKey]bool{}
	for j := 0; j < 5; j++ {
		airing[showKey(j, j%5)] = true // the fake's airing_today list
	}
	assertItems(t, air[0], func(x *core.Title) bool { return x.Status == statusReturning || airing[x.Key] }, "ongoing")
	net := rowsOfKind(m, KindNetwork)
	if len(net) == 0 || net[0].title != "More from Summit Channel" || !strings.Contains(net[0].reason, "2 Summit Channel series") {
		var titles []string
		for _, r := range net {
			titles = append(titles, r.title+": "+r.reason)
		}
		t.Fatalf("network rows %v", titles)
	}
	assertItems(t, net[0], func(x *core.Title) bool { return anyShared(x.Studios, []string{"Summit Channel"}) }, "network")
	// Every TV lens is TV only and unwatched.
	for _, kind := range []string{KindStartNew, KindBinge, KindLimited, KindAiringTaste, KindNetwork} {
		for _, r := range rowsOfKind(m, kind) {
			for _, it := range r.items {
				if it.t.Key == showKey(1, 1) || it.t.Key == showKey(9, 3) {
					t.Fatalf("%s offers watched %s", kind, it.t.Title)
				}
			}
		}
	}
	// No series history: no series lenses that need it.
	_, m = lensModel(t, familyViewer)
	if shown(m, KindNetwork)+shown(m, KindFinishShow) != 0 {
		t.Fatal("series lenses without series history")
	}
	_, m = lensModel(t, func(*harness, int64) {})
	for _, kind := range []string{KindStartNew, KindBinge, KindLimited, KindAiringTaste} {
		if len(rowsOfKind(m, kind)) != 0 {
			t.Fatalf("%s for a viewer with no history", kind)
		}
	}
}

func TestLensRowsAreTypeNative(t *testing.T) {
	_, m := lensModel(t, func(h *harness, uid int64) { sciFiFan(h, uid); tvFan(h, uid) })
	for _, r := range m.rows {
		if !lensKinds[r.kind] || r.kind == KindPerson || r.kind == KindBridge || r.kind == KindHiddenGems || r.kind == KindTrendingTaste || r.kind == KindDifferent {
			continue
		}
		typ := r.items[0].t.Type
		for _, it := range r.items {
			if it.t.Type != typ {
				t.Fatalf("lens row %q mixes %s and %s", r.title, typ, it.t.Type)
			}
		}
	}
	// Mixed rows carry type-native lists for the pages.
	for _, r := range rowsOfKind(m, KindHiddenGems) {
		for typ, list := range r.typed {
			for _, it := range list {
				if baseType(it.t.Type) != typ {
					t.Fatalf("typed list %s holds %s", typ, it.t.Title)
				}
			}
		}
	}
}

func TestBridgeStudioCraftAndSticksLandingRows(t *testing.T) {
	h, m := lensModel(t, func(h *harness, uid int64) {
		// Give several Science Fiction films a boutique studio ("A24-ish") and a Cinematographer.
		dp := core.Person{TMDB: 88801, Name: "Roger Lens", Role: "Cinematographer", ThumbPath: "/rl.jpg"}
		for _, j := range []int{1, 2, 3, 14, 15, 16, 17, 18, 19} {
			k := movieKey(0, j)
			tt := h.cat.titles[k]
			tt.Studios = []string{"A24-ish"}
			tt.Crew = []core.Person{dp}
			h.cat.add(tt)
		}
		sciFiFan(h, uid)
		tvFan(h, uid)
	})
	// 1. Multi-seed Bridge row ("Where <Sci-Fi> Meets <Crime/Drama>").
	bridges := rowsOfKind(m, KindBridge)
	if len(bridges) == 0 || !strings.HasPrefix(bridges[0].title, "Where ") || !strings.Contains(bridges[0].title, " Meets ") {
		t.Fatalf("expected bridge row, got %+v", bridges)
	}
	// 2. Boutique Studio lens ("More from A24-ish").
	studioRow := rowTitled(m, "More from A24-ish")
	assertItems(t, studioRow, func(x *core.Title) bool {
		return x.Type == core.TypeMovie && anyShared(x.Studios, []string{"A24-ish"})
	}, "studio lens")
	// 3. Cinematographer craft shelf ("Shot by Roger Lens") & MicroReason.
	dpRow := rowTitled(m, "Shot by Roger Lens")
	assertItems(t, dpRow, func(x *core.Title) bool {
		return hasPerson(x, 88801)
	}, "cinematographer shelf")
	unseenDP := h.cat.titles[movieKey(0, 18)]
	mr := h.e.MicroReason(context.Background(), &core.User{ID: 1}, &unseenDP, testNow)
	if !strings.Contains(mr.Header, "Shot by Roger Lens") {
		t.Fatalf("expected MicroReason to highlight cinematographer, got %q", mr.Header)
	}
	// 4. "Series That Stick the Landing" lens.
	sticks := rowsOfKind(m, KindSticksLanding)
	if len(sticks) != 1 {
		t.Fatalf("expected Sticks the Landing row, got %d", len(sticks))
	}
	assertItems(t, sticks[0], func(x *core.Title) bool {
		return x.Type == core.TypeShow && x.Status == statusEnded && x.ChildCount >= 2
	}, "sticks the landing")
}
