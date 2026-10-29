package personalize

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

type fakeTrakt struct{ recs, watch []core.Title }

func (f *fakeTrakt) Recommendations(context.Context, int64, int) ([]core.Title, error) {
	return f.recs, nil
}
func (f *fakeTrakt) Watchlist(context.Context, int64, int) ([]core.Title, error) { return f.watch, nil }

type fakeLLM struct {
	sync  bool
	hubs  []core.Hub
	seeds []core.Title
}

func (f *fakeLLM) ThemedRows(_ context.Context, _ *core.User, seeds []core.Title, _ int) ([]core.Hub, error) {
	f.seeds = seeds
	return f.hubs, nil
}
func (f *fakeLLM) MoreLikeThis(context.Context, *core.Title, int) ([]core.Title, error) {
	return nil, nil
}

type fakeLLMSync struct{ fakeLLM }

func (f *fakeLLMSync) ThemedRowsSync(ctx context.Context, u *core.User, seeds []core.Title, limit int) ([]core.Hub, error) {
	f.sync = true
	return f.ThemedRows(ctx, u, seeds, limit)
}

func rowsOfKind(m *userModel, kind string) []*candRow {
	var out []*candRow
	for _, r := range m.rows {
		if r.kind == kind {
			out = append(out, r)
		}
	}
	return out
}

func keysOfRow(r *candRow) map[core.RatingKey]bool {
	out := map[core.RatingKey]bool{}
	for _, it := range r.items {
		out[it.t.Key] = true
	}
	return out
}

// sciFiFan has finished science fiction films (two of the Star Saga, three
// by the same director), rewatched and rated one, binged a show and is
// mid-way through a film.
func sciFiFan(h *harness, uid int64) {
	day := func(d int) time.Time { return testNow.AddDate(0, 0, -d) }
	h.finish(uid, movieKey(0, 1), day(2))
	h.finish(uid, movieKey(0, 2), day(4))
	h.finish(uid, movieKey(0, 3), day(6))
	h.finish(uid, movieKey(0, 4), day(8)) // Star Saga part 1
	h.finish(uid, movieKey(0, 5), day(9)) // Star Saga part 2
	h.finish(uid, movieKey(0, 9), day(200))
	h.finish(uid, movieKey(0, 9), day(90)) // rewatched
	h.at(day(90), func() { _ = h.st.Rate(context.Background(), uid, movieKey(0, 9), 9) })
	show := showKey(0, 1)
	for e := 1; e <= 3; e++ {
		h.finish(uid, core.EpisodeKey(show.TMDB(), 1, e), day(3).Add(time.Duration(e)*time.Hour))
	}
	h.start(uid, movieKey(0, 10), 0.4, day(1))
	h.e.syncEvents(context.Background())
}

func TestGeneratorsForAFan(t *testing.T) {
	trakt := &fakeTrakt{recs: nil}
	llm := &fakeLLMSync{}
	h := newHarness(t, func(o *Options) { o.Trakt = trakt; o.LLM = llm })
	llm.hubs = []core.Hub{{ID: "cosmic-dread", Title: "Cosmic Dread", Items: []core.Title{h.cat.titles[movieKey(0, 11)], h.cat.titles[movieKey(6, 2)], h.cat.titles[movieKey(0, 1)]}}}
	trakt.recs = []core.Title{h.cat.titles[movieKey(6, 3)], h.cat.titles[movieKey(0, 2)]}
	trakt.watch = []core.Title{h.cat.titles[movieKey(1, 4)]}
	alice := core.User{ID: 1, Username: "alice"}
	sciFiFan(h, alice.ID)
	// Another viewer's plays make "Popular on this server".
	h.finish(2, movieKey(6, 7), testNow.AddDate(0, 0, -3))
	h.e.syncEvents(context.Background())

	m := h.rebuild(alice)
	if m.taste.Confidence < 0.5 {
		t.Fatalf("confidence = %v", m.taste.Confidence)
	}
	watched := map[core.RatingKey]bool{}
	for _, j := range []int{1, 2, 3, 4, 5, 9, 10} {
		watched[movieKey(0, j)] = true
	}
	watched[showKey(0, 1)] = true

	discovery := []string{KindTop10, KindBecause, KindPerson, KindTrendingTaste, KindHiddenGems, KindNewThisWeek, KindDifferent, KindShortOnTime, KindTrending, KindPopularServer, KindLLM, KindTraktRecs}
	for _, kind := range discovery {
		for _, r := range rowsOfKind(m, kind) {
			for _, it := range r.items {
				if watched[tasteKey(it.t.Key)] {
					t.Errorf("%s row %q recommends already-watched %s", kind, r.title, it.t.Title)
				}
			}
		}
	}

	top := rowsOfKind(m, KindTop10)
	if len(top) != 1 || top[0].maxItems != 10 {
		t.Fatalf("top 10 rows = %d", len(top))
	}
	sf := 0
	for _, it := range top[0].items[:10] {
		if overlap(it.t.Genres, []string{"Science Fiction"}) {
			sf++
		}
	}
	if sf < 7 {
		t.Fatalf("Top 10 has %d science fiction titles, want most", sf)
	}

	because := rowsOfKind(m, KindBecause)
	if len(because) < 3 || !strings.HasPrefix(because[0].title, "Because You Watched Science Fiction Movie 1") {
		var titles []string
		for _, r := range because {
			titles = append(titles, r.title)
		}
		t.Fatalf("because rows = %v (most recent completion first)", titles)
	}
	if because[0].scale <= because[1].scale {
		t.Fatal("more recent seeds should rank higher")
	}

	person := rowsOfKind(m, KindPerson)
	foundVera := false
	for _, r := range person {
		if r.title == "More from Vera Villeneuve" {
			foundVera = true
			if !strings.Contains(r.reason, "3 titles") {
				t.Errorf("person reason = %q", r.reason)
			}
			if !keysOfRow(r)[movieKey(0, 0)] || !keysOfRow(r)[movieKey(0, 15)] {
				t.Errorf("unwatched Vera film missing: %+v", r.items)
			}
		}
	}
	if !foundVera {
		t.Fatalf("no row for the favourite director: %d person rows", len(person))
	}

	fr := rowsOfKind(m, KindFranchise)
	if len(fr) != 1 || fr[0].title != "Finish Star Saga Collection" || fr[0].collKey != core.CollectionKey(sagaCollection) {
		t.Fatalf("franchise rows = %+v", fr)
	}
	if len(fr[0].items) != 2 || fr[0].items[0].t.Key != movieKey(0, 6) || fr[0].items[1].t.Key != movieKey(0, 7) || !fr[0].keepOrder {
		t.Fatalf("franchise items should be the unwatched parts in order: %+v", fr[0].items)
	}
	if fr[0].reason != "You've seen 2 of 4 Star Saga Collection films." {
		t.Fatalf("franchise reason = %q", fr[0].reason)
	}

	comfort := rowsOfKind(m, KindComfort)
	if len(comfort) != 1 || len(comfort[0].items) != 1 || comfort[0].items[0].t.Key != movieKey(0, 9) {
		t.Fatalf("comfort = %+v", comfort)
	}

	nw := rowsOfKind(m, KindNewThisWeek)
	if len(nw) != 1 || !keysOfRow(nw[0])[movieKey(0, 0)] || !strings.HasPrefix(nw[0].title, "New This Week in Science Fiction") {
		t.Fatalf("new this week = %+v", nw)
	}

	diff := rowsOfKind(m, KindDifferent)
	if len(diff) == 1 {
		for _, it := range diff[0].items {
			if overlap(normGenres(it.t.Genres), m.taste.TopGenres(3)) {
				t.Fatalf("something different includes a top genre: %s %v", it.t.Title, it.t.Genres)
			}
		}
	}

	for _, r := range rowsOfKind(m, KindShortOnTime) {
		for _, it := range r.items {
			if !isShort(&it.t) {
				t.Fatalf("long title in Short on Time: %s %v", it.t.Title, it.t.Runtime)
			}
		}
	}

	gems := rowsOfKind(m, KindHiddenGems)
	for _, r := range gems {
		for _, it := range r.items {
			if it.t.Rating < 7.2 || it.fit < 0.12 {
				t.Fatalf("hidden gem %s rating %v fit %v", it.t.Title, it.t.Rating, it.fit)
			}
		}
	}

	upNext := rowsOfKind(m, KindUpNext)
	if len(upNext) != 1 || len(upNext[0].items) != 1 || upNext[0].items[0].t.Key != core.EpisodeKey(showKey(0, 1).TMDB(), 1, 4) {
		t.Fatalf("up next = %+v", upNext)
	}
	cw := rowsOfKind(m, KindContinue)
	if len(cw) != 1 || len(cw[0].items) != 1 || cw[0].items[0].t.Key != movieKey(0, 10) {
		t.Fatalf("continue = %+v", cw)
	}

	pop := rowsOfKind(m, KindPopularServer)
	if len(pop) != 1 || pop[0].title != "Popular on Test Server" || !keysOfRow(pop[0])[movieKey(6, 7)] {
		t.Fatalf("popular on server = %+v", pop)
	}

	llmRows := rowsOfKind(m, KindLLM)
	if !llm.sync || len(llmRows) != 1 || llmRows[0].id != "llm.cosmic-dread" || len(llmRows[0].items) != 2 {
		t.Fatalf("llm rows = %+v (sync %v)", llmRows, llm.sync)
	}
	if len(llm.seeds) == 0 || llm.seeds[0].Key != movieKey(0, 1) {
		t.Fatalf("llm seeds = %v", llm.seeds)
	}
	recs := rowsOfKind(m, KindTraktRecs)
	if len(recs) != 1 || len(recs[0].items) != 1 {
		t.Fatalf("trakt recommendations = %+v", recs)
	}
	if wl := rowsOfKind(m, KindTraktWatch); len(wl) != 1 || !wl[0].keepOrder {
		t.Fatalf("trakt watchlist = %+v", wl)
	}
}

func TestGenericModelHasNoPersonalRows(t *testing.T) {
	h := newHarness(t, nil)
	m, err := h.e.buildModel(context.Background(), core.User{ID: genericID}, true)
	if err != nil || !m.generic {
		t.Fatalf("generic model: %v", err)
	}
	for _, kind := range []string{KindTop10, KindBecause, KindPerson, KindTrendingTaste, KindHiddenGems, KindNewThisWeek, KindDifferent, KindComfort, KindFranchise, KindContinue, KindUpNext} {
		if rows := rowsOfKind(m, kind); len(rows) > 0 {
			t.Errorf("generic model has a %s row", kind)
		}
	}
	for _, kind := range []string{KindTrending, KindNewReleases, KindTopRated, KindPopular, KindAiringToday, KindShortOnTime} {
		if rows := rowsOfKind(m, kind); len(rows) != 1 || len(rows[0].items) == 0 {
			t.Errorf("generic model lacks %s", kind)
		}
	}
}

func TestOwnerCollectionRows(t *testing.T) {
	cols := &fakeCollections{}
	h := newHarness(t, func(o *Options) { o.Collections = cols })
	var picks []core.Title
	for j := 0; j < 6; j++ {
		picks = append(picks, h.cat.titles[movieKey(8, j)])
	}
	cols.rows = []core.CollectionRow{
		{CollectionID: "docs", HomeRow: true, Hub: core.Hub{ID: "collection.docs", Title: "Staff Documentary Picks", Context: "hub.home.collection", Items: picks}},
		{CollectionID: "secret", SectionRow: true, Hub: core.Hub{ID: "collection.secret", Title: "Section Only", Items: picks}},
	}
	u := core.User{ID: 4, Username: "dee"}
	h.rebuild(u)
	p, err := h.e.PreviewHome(context.Background(), &u, hubContext(testNow, core.DeviceTV), "", 12)
	if err != nil {
		t.Fatal(err)
	}
	r := rowByKind(p.Rows, KindCollectionPrefix+"docs")
	if r == nil || r.Source != core.RowSourceOwner || r.HubID != "collection.docs" || r.Hub.Context != "hub.home.collection" || len(r.Items) != 6 {
		t.Fatalf("owner row = %+v", r)
	}
	if r.Items[0].Key != picks[0].Key {
		t.Fatal("owner rows keep the owner's order")
	}
	if rowByKind(p.Rows, KindCollectionPrefix+"secret") != nil {
		t.Fatal("section-only row on Home")
	}
	sec := hubContext(testNow, core.DeviceTV)
	sec.SectionID = 1
	p, _ = h.e.PreviewHome(context.Background(), &u, sec, "movies", 12)
	if rowByKind(p.Rows, KindCollectionPrefix+"secret") == nil {
		t.Fatal("section row missing from the Movies section")
	}
	// Cached per context: a second Home load does not call collections again.
	before := cols.calls
	h.e.HomeHubsFor(context.Background(), &u, hubContext(testNow, core.DeviceTV), 12)
	if cols.calls != before {
		t.Fatalf("owner rows fetched again (%d → %d)", before, cols.calls)
	}
}

func TestFavoritesRowIsOwnerPlacedAndPreservesOrder(t *testing.T) {
	keys := []core.RatingKey{core.MovieKey(1), core.MovieKey(2)}
	h := newHarness(t, func(o *Options) {
		o.Favorites = func(context.Context, *core.User) ([]FavoriteSignal, error) {
			return []FavoriteSignal{{Key: keys[0], AddedAt: testNow}, {Key: keys[1], AddedAt: testNow.Add(-time.Hour)}}, nil
		}
	})
	// Use catalogue keys known to the harness.
	h.cat.dataMu.Lock()
	for _, key := range keys {
		h.cat.titles[key] = core.Title{Key: key, Type: core.TypeMovie, Title: key.String()}
	}
	h.cat.dataMu.Unlock()
	b := &builder{e: h.e, ctx: context.Background(), user: core.User{ID: 1}}
	rows := b.genFavorites()
	if len(rows) != 1 || len(rows[0].items) != 2 || rows[0].items[0].t.Key != keys[0] || !rows[0].keepOrder || rows[0].minItems != 1 {
		t.Fatalf("favorite rows: %+v", rows)
	}
	if len(infoFor(KindFavorites).surfaces) != 0 {
		t.Fatal("favorites row must not be enabled by default")
	}
}

func TestEmptyFavoritesPreservesGeneratedRows(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	user := core.User{ID: 1}
	before, err := h.e.buildModel(ctx, user, false)
	if err != nil {
		t.Fatal(err)
	}
	h.e.opts.Favorites = func(context.Context, *core.User) ([]FavoriteSignal, error) { return []FavoriteSignal{}, nil }
	after, err := h.e.buildModel(ctx, user, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(before.rows) != len(after.rows) {
		t.Fatalf("row count changed: %d vs %d", len(before.rows), len(after.rows))
	}
	for i := range before.rows {
		a, b := before.rows[i], after.rows[i]
		if a.id != b.id || a.kind != b.kind || len(a.items) != len(b.items) {
			t.Fatalf("row %d changed", i)
		}
		for j := range a.items {
			if a.items[j].t.Key != b.items[j].t.Key {
				t.Fatalf("row %d item %d changed", i, j)
			}
		}
	}
}

func TestFavoriteHistoryPriorityStableAndEmptyParity(t *testing.T) {
	a, b := core.ShowKey(1), core.ShowKey(2)
	hist := []core.HistoryEntry{{Key: core.EpisodeKey(1, 1, 1)}, {Key: core.EpisodeKey(2, 1, 1)}, {Key: core.EpisodeKey(2, 1, 2)}, {Key: core.MovieKey(3)}}
	original := append([]core.HistoryEntry(nil), hist...)
	prioritizeFavoriteHistory(hist, nil)
	for i := range hist {
		if hist[i].Key != original[i].Key {
			t.Fatal("empty favorites changed order")
		}
	}
	prioritizeFavoriteHistory(hist, []FavoriteSignal{{Key: b}})
	want := []core.RatingKey{original[1].Key, original[2].Key, original[0].Key, original[3].Key}
	for i := range want {
		if hist[i].Key != want[i] {
			t.Fatalf("order %+v (other show %v)", hist, a)
		}
	}
}
