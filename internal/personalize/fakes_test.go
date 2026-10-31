package personalize

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/state"
	"github.com/braedonsaunders/ether/internal/store"
)

// testNow is a Wednesday evening.
var testNow = time.Date(2026, 10, 14, 20, 0, 0, 0, time.UTC)

// clock is a settable test clock.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock(t time.Time) *clock { return &clock{t: t} }
func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *clock) Set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

var testGenres = []string{"Science Fiction", "Drama", "Comedy", "Horror", "Animation", "Family", "Thriller", "Romance", "Documentary", "Crime"}

// showShapes give the five shows of each genre a status and length:
// returning, a complete three-season run, a limited series, a new
// returning one-season show, and a complete two-season run.
var showShapes = [5]struct {
	status  string
	seasons int
}{{statusReturning, 2}, {statusEnded, 3}, {statusEnded, 1}, {statusReturning, 1}, {statusEnded, 2}}

// testNetworks are the shows' networks (show j of genre gi airs on
// network (gi+j)%4).
var testNetworks = []string{"Aurora Network", "Harbor TV", "Summit Channel", "Lumen Studios"}

// genreThemes are each genre's four themes (TMDB-style keywords); movie j
// of a genre carries theme j%4.
var genreThemes = map[string][4]string{
	"Science Fiction": {"space", "time travel", "dystopia", "artificial intelligence"},
	"Drama":           {"family relationships", "coming of age", "true story", "grief"},
	"Comedy":          {"buddy", "satire", "workplace", "road trip"},
	"Horror":          {"haunted house", "slasher", "supernatural", "zombie"},
	"Animation":       {"talking animals", "fairy tale", "superhero", "friendship"},
	"Family":          {"holiday", "pets", "magic", "sports"},
	"Thriller":        {"heist", "revenge", "conspiracy", "spy"},
	"Romance":         {"love triangle", "wedding", "second chance", "small town"},
	"Documentary":     {"nature", "music", "true crime", "history"},
	"Crime":           {"detective", "mafia", "serial killer", "police"},
}

// fakeCatalog is a synthetic, deterministic catalogue. Engine background
// workers read it while tests add titles, so its data is guarded by dataMu
// (mu only guards call counts).
type fakeCatalog struct {
	mu     sync.Mutex
	dataMu sync.RWMutex
	titles map[core.RatingKey]core.Title
	order  []core.RatingKey
	colls  map[int64][]core.RatingKey
	lists  map[string][]core.RatingKey
	calls  map[string]int
	// recs are planted crowd co-engagement lists (CrowdRecommendations);
	// titles without one get their genre's most popular titles.
	recs map[core.RatingKey][]core.RatingKey
	// crowdStored emulates the catalogue's crowd store: pages stored per
	// title; crowdPages counts pages "fetched from TMDB".
	crowdStored map[core.RatingKey]int
	crowdPages  int
	// holdSources, when set, pauses similar/list/browse before they are
	// counted. sourcesHeld is closed once, when the first call reaches
	// that pause. Both are nil unless a test is holding startup work.
	holdSources <-chan struct{}
	sourcesHeld chan struct{}
	heldOnce    sync.Once
}

// movieKey / showKey address the synthetic titles: genre index gi, item j.
func movieKey(gi, j int) core.RatingKey { return core.MovieKey(int64(1000*(gi+1) + j + 1)) }
func showKey(gi, j int) core.RatingKey  { return core.ShowKey(int64(1000*(gi+1) + j + 1)) }

const (
	sagaCollection = 9001
	sciFiDirector  = 500 // directs Science Fiction movies 0-3
)

func newFakeCatalog(now time.Time) *fakeCatalog {
	c := &fakeCatalog{titles: map[core.RatingKey]core.Title{}, colls: map[int64][]core.RatingKey{}, lists: map[string][]core.RatingKey{}, calls: map[string]int{},
		recs: map[core.RatingKey][]core.RatingKey{}, crowdStored: map[core.RatingKey]int{}}
	for gi, g := range testGenres {
		for j := 0; j < 48; j++ {
			rating := "PG-13"
			switch {
			case g == "Horror" || g == "Crime" || g == "Thriller":
				rating = "R"
			case g == "Family" || g == "Animation":
				rating = "G"
			case j%2 == 0:
				rating = "PG"
			}
			year, released := 1975+(j%24)*2, time.Date(1975+(j%24)*2, 6, 1, 0, 0, 0, 0, time.UTC)
			if j == 0 { // brand new
				released = now.AddDate(0, 0, -2)
				year = released.Year()
			}
			kws := []string{genreSlug(g) + "-theme", genreThemes[g][j%4]}
			if j%7 == 0 {
				kws = append(kws, "shared-motif")
			}
			t := core.Title{
				Key: movieKey(gi, j), Type: core.TypeMovie, IDs: core.IDs{TMDB: int64(1000*(gi+1) + j + 1)},
				Title: fmt.Sprintf("%s Movie %d", g, j), Year: year, Genres: []string{g}, Keywords: kws,
				Runtime: time.Duration(80+(j%14)*6) * time.Minute, Language: "en", ContentRating: rating,
				Rating: 6.0 + float64(j%6)*0.5, VoteCount: 80 + j*400, Popularity: 4 + float64((j*37)%90),
				Released: released,
				Cast: []core.Person{
					{TMDB: int64(600 + gi*10 + j%3), Name: fmt.Sprintf("%s Actor %d", g, j%3), ThumbPath: fmt.Sprintf("/p%d.jpg", 600+gi*10+j%3)},
					{TMDB: int64(5000 + gi*10 + j%4), Name: fmt.Sprintf("%s Support %d", g, j%4)},
				},
			}
			if j%5 == 0 {
				t.Genres = append(t.Genres, testGenres[(gi+1)%len(testGenres)])
			}
			if g == "Science Fiction" && (j < 4 || (j >= 14 && j < 18) || (j >= 24 && j < 32)) {
				t.Directors = []core.Person{{TMDB: sciFiDirector, Name: "Vera Villeneuve", ThumbPath: "/vera.jpg"}}
			} else {
				t.Directors = []core.Person{{TMDB: int64(10000 + gi*100 + j), Name: fmt.Sprintf("Director %d-%d", gi, j)}}
			}
			if g == "Science Fiction" && j >= 4 && j < 8 {
				t.CollectionID, t.CollectionName = sagaCollection, "Star Saga Collection"
				c.colls[sagaCollection] = append(c.colls[sagaCollection], t.Key)
			}
			if j == 0 { // a talked-about new release
				t.Popularity, t.VoteCount = 60+float64(gi), 150
			}
			c.add(t)
		}
		rating := "TV-14"
		switch g {
		case "Horror", "Crime", "Thriller":
			rating = "TV-MA"
		case "Family", "Animation":
			rating = "TV-G"
		case "Comedy", "Documentary":
			rating = "TV-PG"
		}
		for j := 0; j < 5; j++ {
			rt := 50
			if g == "Comedy" || g == "Animation" {
				rt = 24
			}
			sh := showShapes[j]
			kws := []string{genreSlug(g) + "-theme"}
			if sh.status == statusEnded && sh.seasons == 1 {
				kws = append(kws, "miniseries")
			}
			s := core.Title{
				Key: showKey(gi, j), Type: core.TypeShow, IDs: core.IDs{TMDB: int64(1000*(gi+1) + j + 1)},
				Title: fmt.Sprintf("%s Show %d", g, j), Year: 2010 + j, Genres: []string{g}, Keywords: kws,
				Runtime: time.Duration(rt) * time.Minute, Language: "en", ContentRating: rating,
				Rating: 7.0 + float64(j)*0.3, VoteCount: 300 + j*100, Popularity: 10 + float64(j*15), Released: now.AddDate(-1, 0, -j*30),
				ChildCount: sh.seasons, LeafCount: 8 * sh.seasons, Status: sh.status, Studios: []string{testNetworks[(gi+j)%len(testNetworks)]},
			}
			c.add(s)
			for se := 1; se <= sh.seasons; se++ {
				premiere := s.Released.AddDate(0, 0, 180*(se-1))
				for e := 1; e <= 8; e++ {
					ep := core.Title{Key: core.EpisodeKey(s.IDs.TMDB, se, e), Type: core.TypeEpisode, ShowKey: s.Key, ShowTitle: s.Title,
						Season: se, Episode: e, Title: fmt.Sprintf("%s S%dE%d", s.Title, se, e), Runtime: s.Runtime, Genres: s.Genres,
						ContentRating: s.ContentRating, Released: premiere.AddDate(0, 0, 7*e)}
					if se == 1 {
						ep.Title = fmt.Sprintf("%s E%d", s.Title, e)
					}
					c.titles[ep.Key] = ep
				}
			}
		}
	}
	// Lists: trending = the most popular of each genre, interleaved.
	for _, typ := range []core.MediaType{core.TypeMovie, core.TypeShow} {
		var all []core.Title
		for _, k := range c.order {
			if t := c.titles[k]; t.Type == typ {
				all = append(all, t)
			}
		}
		byPop := append([]core.Title(nil), all...)
		sort.SliceStable(byPop, func(i, j int) bool { return byPop[i].Popularity > byPop[j].Popularity })
		byRating := append([]core.Title(nil), all...)
		sort.SliceStable(byRating, func(i, j int) bool { return byRating[i].Rating > byRating[j].Rating })
		for i, t := range byPop {
			if i < 30 {
				c.lists["trending_week:"+string(typ)] = append(c.lists["trending_week:"+string(typ)], t.Key)
				c.lists["popular:"+string(typ)] = append(c.lists["popular:"+string(typ)], t.Key)
			}
			if i < 10 {
				c.lists["trending_day:"+string(typ)] = append(c.lists["trending_day:"+string(typ)], t.Key)
			}
		}
		for i, t := range byRating {
			if i < 30 {
				c.lists["top_rated:"+string(typ)] = append(c.lists["top_rated:"+string(typ)], t.Key)
			}
		}
	}
	for j := 0; j < 5; j++ {
		c.lists["airing_today:show"] = append(c.lists["airing_today:show"], showKey(j, j%5))
	}
	return c
}

func (c *fakeCatalog) add(t core.Title) {
	c.dataMu.Lock()
	defer c.dataMu.Unlock()
	c.titles[t.Key] = t
	c.order = append(c.order, t.Key)
}

// set replaces or inserts a title without changing browse order.
func (c *fakeCatalog) set(t core.Title) {
	c.dataMu.Lock()
	defer c.dataMu.Unlock()
	c.titles[t.Key] = t
}

// prependList puts k at the front of a list ("trending_week:movie").
func (c *fakeCatalog) prependList(name string, k core.RatingKey) {
	c.dataMu.Lock()
	defer c.dataMu.Unlock()
	c.lists[name] = append([]core.RatingKey{k}, c.lists[name]...)
}

func (c *fakeCatalog) count(name string) {
	c.mu.Lock()
	c.calls[name]++
	c.mu.Unlock()
}

// awaitSources lets a test pause catalogue source calls. The pause is
// before the call is counted, so a sample taken while startup work is
// held does not yet include it.
func (c *fakeCatalog) awaitSources() {
	c.mu.Lock()
	hold, held := c.holdSources, c.sourcesHeld
	c.mu.Unlock()
	if held != nil {
		c.heldOnce.Do(func() { close(held) })
	}
	if hold != nil {
		<-hold
	}
}

func (c *fakeCatalog) callCount(name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[name]
}

// resolve maps keys to titles; the caller holds dataMu.
func (c *fakeCatalog) resolve(keys []core.RatingKey) []core.Title {
	var out []core.Title
	for _, k := range keys {
		if t, ok := c.titles[k]; ok {
			out = append(out, t)
		}
	}
	return out
}

func (c *fakeCatalog) Get(_ context.Context, k core.RatingKey) (*core.Title, error) {
	c.count("get")
	c.dataMu.RLock()
	defer c.dataMu.RUnlock()
	if t, ok := c.titles[k]; ok {
		return &t, nil
	}
	return nil, core.ErrNotFound
}

// Children lists a show's seasons, derived from its episodes (a season's
// release is its first episode's).
func (c *fakeCatalog) Children(_ context.Context, k core.RatingKey) ([]core.Title, error) {
	c.count("children")
	if k.Kind() != core.KindShow {
		return nil, nil
	}
	c.dataMu.RLock()
	defer c.dataMu.RUnlock()
	seasons := map[int]*core.Title{}
	for _, t := range c.titles {
		if t.Type != core.TypeEpisode || t.ShowKey != k {
			continue
		}
		s := seasons[t.Season]
		if s == nil {
			s = &core.Title{Key: core.SeasonKey(k.TMDB(), t.Season), Type: core.TypeSeason, ShowKey: k, ShowTitle: t.ShowTitle,
				Season: t.Season, Title: fmt.Sprintf("Season %d", t.Season), Released: t.Released}
			seasons[t.Season] = s
		}
		s.ChildCount++
		if t.Released.Before(s.Released) {
			s.Released = t.Released
		}
	}
	out := make([]core.Title, 0, len(seasons))
	for _, s := range seasons {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Season < out[j].Season })
	return out, nil
}

// addSeason gives a show another season of eight episodes premiering at
// premiere (weekly after that).
func (c *fakeCatalog) addSeason(show core.RatingKey, season int, premiere time.Time) {
	c.dataMu.Lock()
	defer c.dataMu.Unlock()
	s := c.titles[show]
	for e := 1; e <= 8; e++ {
		ep := core.Title{Key: core.EpisodeKey(show.TMDB(), season, e), Type: core.TypeEpisode, ShowKey: show, ShowTitle: s.Title,
			Season: season, Episode: e, Title: fmt.Sprintf("%s S%dE%d", s.Title, season, e), Runtime: s.Runtime, Genres: s.Genres,
			ContentRating: s.ContentRating, Released: premiere.AddDate(0, 0, 7*(e-1))}
		c.titles[ep.Key] = ep
	}
	if season > s.ChildCount {
		s.ChildCount = season
		s.LeafCount = 8 * season
	}
	c.titles[show] = s
}

// addWorldCinema adds n films in a language (alternating Drama and
// Thriller), modestly popular so they stay out of the trending lists.
func (c *fakeCatalog) addWorldCinema(lang, country string, base, n int) []core.RatingKey {
	var keys []core.RatingKey
	for j := 0; j < n; j++ {
		g := []string{"Drama", "Thriller"}[j%2]
		id := int64(50000 + base + j)
		t := core.Title{Key: core.MovieKey(id), Type: core.TypeMovie, IDs: core.IDs{TMDB: id},
			Title: fmt.Sprintf("%s Film %d", lang, j), Year: 2000 + j, Genres: []string{g}, Keywords: []string{genreThemes[g][j%4]},
			Runtime: time.Duration(95+j*3) * time.Minute, Language: lang, Countries: []string{country}, ContentRating: "R",
			Rating: 7.0 + float64(j%5)*0.3, VoteCount: 1500 + j*200, Popularity: 6 + float64(j%5),
			Released:  time.Date(2000+j, 3, 1, 0, 0, 0, 0, time.UTC),
			Directors: []core.Person{{TMDB: id + 900000, Name: fmt.Sprintf("%s Director %d", lang, j)}}}
		c.add(t)
		keys = append(keys, t.Key)
	}
	return keys
}
func (c *fakeCatalog) Grandchildren(context.Context, core.RatingKey) ([]core.Title, error) {
	return nil, nil
}

func (c *fakeCatalog) Browse(_ context.Context, q core.BrowseQuery) (core.Page, error) {
	c.awaitSources()
	c.count("browse")
	c.dataMu.RLock()
	defer c.dataMu.RUnlock()
	f := q.Filter
	var items []core.Title
	for _, k := range c.order {
		t := c.titles[k]
		if f.Type != "" && t.Type != f.Type {
			continue
		}
		if len(f.Genres) > 0 && !overlap(t.Genres, f.Genres) {
			continue
		}
		if f.MinRating > 0 && t.Rating < f.MinRating {
			continue
		}
		if f.MaxRuntime > 0 && t.Runtime > f.MaxRuntime {
			continue
		}
		if f.MinRuntime > 0 && t.Runtime < f.MinRuntime {
			continue
		}
		if f.Person != 0 && !hasPerson(&t, f.Person) {
			continue
		}
		if f.YearFrom > 0 && t.Year < f.YearFrom || f.YearTo > 0 && t.Year > f.YearTo {
			continue
		}
		if len(f.Languages) > 0 && !overlap([]string{t.Language}, f.Languages) {
			continue
		}
		if len(f.Countries) > 0 && !overlap(t.Countries, f.Countries) {
			continue
		}
		if f.Studio != "" && !overlap(t.Studios, []string{f.Studio}) {
			continue
		}
		items = append(items, t)
	}
	switch q.Sort {
	case core.SortReleased:
		sort.SliceStable(items, func(i, j int) bool { return items[i].Released.After(items[j].Released) })
	case core.SortRating:
		sort.SliceStable(items, func(i, j int) bool { return items[i].Rating > items[j].Rating })
	case core.SortAdded:
		// insertion order, newest last → reverse
		for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 {
			items[i], items[j] = items[j], items[i]
		}
	default:
		sort.SliceStable(items, func(i, j int) bool { return items[i].Popularity > items[j].Popularity })
	}
	total := len(items)
	if q.Limit > 0 && len(items) > q.Limit {
		items = items[:q.Limit]
	}
	return core.Page{Items: items, Total: total}, nil
}

func overlap(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}

func (c *fakeCatalog) Search(context.Context, string, []core.MediaType, int) ([]core.Title, error) {
	return nil, nil
}
func (c *fakeCatalog) List(_ context.Context, name string, t core.MediaType, limit int) ([]core.Title, error) {
	c.awaitSources()
	c.count("list")
	c.dataMu.RLock()
	items := c.resolve(c.lists[name+":"+string(t)])
	c.dataMu.RUnlock()
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

// Similar: planted crowd recommendations first (like the catalogue, whose
// first page is TMDB's recommendations), then the same primary genre by
// popularity.
func (c *fakeCatalog) Similar(_ context.Context, k core.RatingKey, limit int) ([]core.Title, error) {
	c.awaitSources()
	c.count("similar")
	c.dataMu.RLock()
	defer c.dataMu.RUnlock()
	src, ok := c.titles[k]
	if !ok || len(src.Genres) == 0 {
		return nil, nil
	}
	out := c.resolve(c.recs[k])
	if len(out) > 0 {
		seen := map[core.RatingKey]bool{}
		for _, t := range out {
			seen[t.Key] = true
		}
		var rest []core.Title
		for _, key := range c.order {
			t := c.titles[key]
			if key != k && !seen[key] && (t.Type == core.TypeMovie || t.Type == core.TypeShow) && len(t.Genres) > 0 && t.Genres[0] == src.Genres[0] {
				rest = append(rest, t)
			}
		}
		sort.SliceStable(rest, func(i, j int) bool { return rest[i].Popularity > rest[j].Popularity })
		out = append(out, rest...)
		if limit > 0 && len(out) > limit {
			out = out[:limit]
		}
		return out, nil
	}
	for _, key := range c.order {
		t := c.titles[key]
		if key != k && (t.Type == core.TypeMovie || t.Type == core.TypeShow) && len(t.Genres) > 0 && t.Genres[0] == src.Genres[0] {
			out = append(out, t)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Popularity > out[j].Popularity })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// CrowdRecommendations emulates the catalogue's crowd store: a list is
// "fetched" (counted per page) when fetch is set and fewer pages are
// stored; without fetch an unstored list is nil.
func (c *fakeCatalog) CrowdRecommendations(_ context.Context, k core.RatingKey, pages int, fetch bool) ([]core.Title, bool, error) {
	c.count("crowd")
	k = tasteKey(k)
	c.mu.Lock()
	stored := c.crowdStored[k]
	fetched := false
	switch {
	case stored >= pages:
	case fetch:
		c.crowdStored[k] = pages
		c.crowdPages += pages - stored
		fetched, stored = true, pages
	case stored == 0:
		c.mu.Unlock()
		return nil, false, nil
	}
	c.mu.Unlock()
	c.dataMu.RLock()
	defer c.dataMu.RUnlock()
	var out []core.Title
	if keys, ok := c.recs[k]; ok {
		out = c.resolve(keys)
	} else if src, ok := c.titles[k]; ok && len(src.Genres) > 0 {
		for _, key := range c.order {
			t := c.titles[key]
			if key != k && (t.Type == core.TypeMovie || t.Type == core.TypeShow) && len(t.Genres) > 0 && t.Genres[0] == src.Genres[0] {
				out = append(out, t)
			}
		}
		sort.SliceStable(out, func(i, j int) bool { return out[i].Popularity > out[j].Popularity })
	}
	if n := 20 * stored; len(out) > n {
		out = out[:n]
	}
	return out, fetched, nil
}

func (c *fakeCatalog) fetchedPages() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.crowdPages
}

func (c *fakeCatalog) CrowdGraphBatch(_ context.Context, keys []core.RatingKey, maxPerKey, maxTargets int) (map[core.RatingKey][]core.RatingKey, map[core.RatingKey]core.Title, error) {
	c.dataMu.RLock()
	defer c.dataMu.RUnlock()
	edges := map[core.RatingKey][]core.RatingKey{}
	titles := map[core.RatingKey]core.Title{}
	for _, k := range keys {
		tk := tasteKey(k)
		recs, ok := c.recs[tk]
		if !ok || len(recs) == 0 {
			continue
		}
		if maxPerKey > 0 && len(recs) > maxPerKey {
			recs = recs[:maxPerKey]
		}
		edges[tk] = append([]core.RatingKey(nil), recs...)
		for _, rk := range recs {
			if t, ok := c.titles[rk]; ok {
				titles[rk] = t
			}
		}
	}
	return edges, titles, nil
}

func (c *fakeCatalog) Collection(_ context.Context, id int64) (string, []core.Title, error) {
	if id == sagaCollection {
		c.dataMu.RLock()
		defer c.dataMu.RUnlock()
		return "Star Saga Collection", c.resolve(c.colls[id]), nil
	}
	return "", nil, core.ErrNotFound
}
func (c *fakeCatalog) Facet(context.Context, core.MediaType, string) ([]core.FacetValue, error) {
	return nil, nil
}
func (c *fakeCatalog) NextEpisode(_ context.Context, ep core.RatingKey) (*core.Title, error) {
	next := core.EpisodeKey(ep.TMDB(), ep.Season(), ep.Episode()+1)
	c.dataMu.RLock()
	defer c.dataMu.RUnlock()
	if t, ok := c.titles[next]; ok {
		return &t, nil
	}
	return nil, core.ErrNotFound
}
func (c *fakeCatalog) ByExternalID(context.Context, string, string) (*core.Title, error) {
	return nil, core.ErrNotFound
}

// fakeUsers is a static user list.
type fakeUsers struct{ users []core.User }

func (f *fakeUsers) Users(context.Context) ([]core.User, error) { return f.users, nil }

// fakeCollections returns fixed owner rows.
type fakeCollections struct {
	mu    sync.Mutex
	rows  []core.CollectionRow
	calls int
}

func (f *fakeCollections) HomeRowsFor(context.Context, *core.User, core.HubContext) ([]core.CollectionRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.rows, nil
}

// harness wires an engine to a real in-memory state store.
type harness struct {
	t     *testing.T
	db    *sql.DB
	clock *clock
	cat   *fakeCatalog
	st    *state.State
	e     *Engine
	users *fakeUsers
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newHarness(t *testing.T, mutate func(*Options)) *harness {
	t.Helper()
	db, err := store.OpenMemory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	clk := newClock(testNow)
	h := &harness{t: t, db: db, clock: clk, cat: newFakeCatalog(testNow), users: &fakeUsers{}}
	h.st = state.New(db, state.Options{Now: clk.Now, Logger: quietLogger()})
	opts := Options{
		DB: db, Catalog: h.cat, State: h.st, Events: h.st, Users: h.users,
		Logger: quietLogger(), Now: clk.Now, Location: time.UTC,
		RefreshEvery: -1, RebuildDebounce: time.Hour, ColdWait: 5 * time.Second, OwnerRowsWait: 2 * time.Second,
		FlushEvery: time.Hour, ServerName: func() string { return "Test Server" },
		// The fake catalogue is small; the row floor has its own test.
		MinRowItems: -1,
	}
	if mutate != nil {
		mutate(&opts)
	}
	e, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Close)
	h.e = e
	return h
}

// at runs fn with the clock set to t (state writes are stamped with it).
func (h *harness) at(t time.Time, fn func()) {
	h.t.Helper()
	prev := h.clock.Now()
	h.clock.Set(t)
	fn()
	h.clock.Set(prev)
}

// finish records a completed viewing of key at t.
func (h *harness) finish(uid int64, key core.RatingKey, at time.Time) {
	h.t.Helper()
	ctx := context.Background()
	rt := 100 * time.Minute
	if t, ok := h.cat.titles[key]; ok && t.Runtime > 0 {
		rt = t.Runtime
	}
	for off := rt / 5; off < rt*9/10; off += 10 * time.Minute { // timeline ticks
		h.at(at.Add(off-rt/5), func() {
			if err := h.st.Progress(ctx, uid, key, off, rt, "playing"); err != nil {
				h.t.Fatal(err)
			}
		})
	}
	h.at(at.Add(rt*9/10), func() {
		if err := h.st.Progress(ctx, uid, key, rt*95/100, rt, "stopped"); err != nil {
			h.t.Fatal(err)
		}
	})
}

// start records a viewing stopped at fraction f.
func (h *harness) start(uid int64, key core.RatingKey, f float64, at time.Time) {
	h.t.Helper()
	rt := 100 * time.Minute
	if t, ok := h.cat.titles[key]; ok && t.Runtime > 0 {
		rt = t.Runtime
	}
	h.at(at, func() {
		if err := h.st.Progress(context.Background(), uid, key, time.Duration(float64(rt)*f), rt, "stopped"); err != nil {
			h.t.Fatal(err)
		}
	})
}

func (h *harness) rebuild(u core.User) *userModel {
	h.t.Helper()
	if err := h.e.Rebuild(context.Background(), &u); err != nil {
		h.t.Fatal(err)
	}
	h.e.mu.Lock()
	defer h.e.mu.Unlock()
	return h.e.models[u.ID]
}

func hubContext(at time.Time, device string) core.HubContext {
	hc := core.NewHubContext(at, "", "", "")
	hc.Device = device
	return hc
}

func rowByKind(rows []core.HomeRow, kind string) *core.HomeRow {
	for i := range rows {
		if rows[i].Kind == kind {
			return &rows[i]
		}
	}
	return nil
}

func kindsOf(rows []core.HomeRow) []string {
	var out []string
	for _, r := range rows {
		if !r.Hidden {
			out = append(out, r.Kind)
		}
	}
	return out
}

func genreShare(rows []core.HomeRow, genre string, skipKinds ...string) float64 {
	n, hit := 0, 0
	skip := map[string]bool{}
	for _, k := range skipKinds {
		skip[k] = true
	}
	for _, r := range rows {
		if r.Hidden || skip[r.Kind] {
			continue
		}
		for _, t := range r.Items {
			n++
			if overlap(normGenres(t.Genres), []string{genre}) {
				hit++
			}
		}
	}
	if n == 0 {
		return 0
	}
	return float64(hit) / float64(n)
}
