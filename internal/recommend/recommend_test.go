package recommend

import (
	"context"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/state"
	"github.com/braedonsaunders/ether/internal/store"
)

// ---- fakes -----------------------------------------------------------------

type fakeCatalog struct {
	mu       sync.Mutex
	filters  []core.Filter // Browse filters seen
	byKey    map[core.RatingKey]core.Title
	similar  map[core.RatingKey][]core.RatingKey
	lists    map[string][]core.RatingKey // "trending_week:movie"
	colls    map[int64][]core.RatingKey
	collName map[int64]string
}

func newFake() *fakeCatalog {
	return &fakeCatalog{byKey: map[core.RatingKey]core.Title{}, similar: map[core.RatingKey][]core.RatingKey{},
		lists: map[string][]core.RatingKey{}, colls: map[int64][]core.RatingKey{}, collName: map[int64]string{}}
}

func (f *fakeCatalog) add(t core.Title) core.Title { f.byKey[t.Key] = t; return t }
func (f *fakeCatalog) resolve(keys []core.RatingKey) []core.Title {
	var out []core.Title
	for _, k := range keys {
		if t, ok := f.byKey[k]; ok {
			out = append(out, t)
		}
	}
	return out
}
func (f *fakeCatalog) Get(_ context.Context, k core.RatingKey) (*core.Title, error) {
	if t, ok := f.byKey[k]; ok {
		return &t, nil
	}
	return nil, core.ErrNotFound
}
func (f *fakeCatalog) Children(context.Context, core.RatingKey) ([]core.Title, error) {
	return nil, nil
}
func (f *fakeCatalog) Grandchildren(context.Context, core.RatingKey) ([]core.Title, error) {
	return nil, nil
}
func (f *fakeCatalog) Browse(_ context.Context, q core.BrowseQuery) (core.Page, error) {
	f.mu.Lock()
	f.filters = append(f.filters, q.Filter)
	f.mu.Unlock()
	var items []core.Title
	for _, t := range f.byKey {
		if q.Filter.Type != "" && t.Type != q.Filter.Type {
			continue
		}
		if q.Filter.Person != 0 {
			found := false
			for _, p := range append(append([]core.Person{}, t.Cast...), t.Directors...) {
				if p.TMDB == q.Filter.Person {
					found = true
				}
			}
			if !found {
				continue
			}
		}
		items = append(items, t)
	}
	switch q.Sort {
	case core.SortReleased:
		sort.SliceStable(items, func(i, j int) bool { return items[i].Released.After(items[j].Released) })
	default:
		sort.SliceStable(items, func(i, j int) bool { return items[i].Popularity > items[j].Popularity })
	}
	if q.Limit > 0 && len(items) > q.Limit {
		items = items[:q.Limit]
	}
	return core.Page{Items: items, Total: len(items)}, nil
}
func (f *fakeCatalog) Search(context.Context, string, []core.MediaType, int) ([]core.Title, error) {
	return nil, nil
}
func (f *fakeCatalog) List(_ context.Context, name string, t core.MediaType, limit int) ([]core.Title, error) {
	items := f.resolve(f.lists[name+":"+string(t)])
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}
func (f *fakeCatalog) Similar(_ context.Context, k core.RatingKey, _ int) ([]core.Title, error) {
	return f.resolve(f.similar[k]), nil
}
func (f *fakeCatalog) Collection(_ context.Context, id int64) (string, []core.Title, error) {
	return f.collName[id], f.resolve(f.colls[id]), nil
}
func (f *fakeCatalog) Facet(context.Context, core.MediaType, string) ([]core.FacetValue, error) {
	return nil, nil
}
func (f *fakeCatalog) NextEpisode(_ context.Context, ep core.RatingKey) (*core.Title, error) {
	next := core.EpisodeKey(ep.TMDB(), ep.Season(), ep.Episode()+1)
	if t, ok := f.byKey[next]; ok {
		return &t, nil
	}
	return nil, core.ErrNotFound
}
func (f *fakeCatalog) ByExternalID(context.Context, string, string) (*core.Title, error) {
	return nil, core.ErrNotFound
}

type fakeCollections struct {
	items map[core.RatingKey][]core.Title
	list  []core.Collection
}

func (f fakeCollections) List(_ context.Context, _ *core.User, t core.MediaType) ([]core.Collection, error) {
	var out []core.Collection
	for _, c := range f.list {
		if c.Type == t {
			out = append(out, c)
		}
	}
	return out, nil
}
func (f fakeCollections) Get(context.Context, *core.User, core.RatingKey) (*core.Collection, error) {
	return nil, core.ErrNotFound
}
func (f fakeCollections) Items(_ context.Context, _ *core.User, k core.RatingKey, _, _ int) (core.Page, error) {
	it := f.items[k]
	return core.Page{Items: it, Total: len(it)}, nil
}
func (f fakeCollections) CreateSmart(context.Context, core.Collection) (*core.Collection, error) {
	return nil, nil
}
func (f fakeCollections) DeleteSmart(context.Context, core.RatingKey) error { return nil }

// ---- fixture ---------------------------------------------------------------

var (
	arrival  = core.Title{Key: core.MovieKey(1), Type: core.TypeMovie, Title: "Arrival", Year: 2016, Genres: []string{"Sci-Fi", "Drama"}, Directors: []core.Person{{TMDB: 100, Name: "Villeneuve"}}, Language: "en", Rating: 7.6, Popularity: 50, Released: time.Date(2016, 11, 1, 0, 0, 0, 0, time.UTC), ContentRating: "PG-13"}
	dune     = core.Title{Key: core.MovieKey(2), Type: core.TypeMovie, Title: "Dune", Year: 2021, Genres: []string{"Sci-Fi", "Adventure"}, Directors: []core.Person{{TMDB: 100, Name: "Villeneuve"}}, Language: "en", Rating: 8.0, Popularity: 90, CollectionID: 55, CollectionName: "Dune Collection", Released: time.Date(2021, 10, 1, 0, 0, 0, 0, time.UTC), ContentRating: "PG-13"}
	dune2    = core.Title{Key: core.MovieKey(3), Type: core.TypeMovie, Title: "Dune: Part Two", Year: 2024, Genres: []string{"Sci-Fi", "Adventure"}, Directors: []core.Person{{TMDB: 100, Name: "Villeneuve"}}, Language: "en", Rating: 8.3, Popularity: 95, CollectionID: 55, CollectionName: "Dune Collection", Released: time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC), ContentRating: "PG-13"}
	comedy   = core.Title{Key: core.MovieKey(4), Type: core.TypeMovie, Title: "Silly Comedy", Year: 2023, Genres: []string{"Comedy"}, Language: "en", Rating: 6.0, Popularity: 80, Released: time.Date(2023, 5, 1, 0, 0, 0, 0, time.UTC), ContentRating: "PG-13"}
	goryR    = core.Title{Key: core.MovieKey(5), Type: core.TypeMovie, Title: "Gory Sci-Fi", Year: 2022, Genres: []string{"Sci-Fi", "Horror"}, Directors: []core.Person{{TMDB: 100, Name: "Villeneuve"}}, Language: "en", Rating: 8.5, Popularity: 70, ContentRating: "R"}
	sever    = core.Title{Key: core.ShowKey(9), Type: core.TypeShow, Title: "Severance", Year: 2022, Genres: []string{"Sci-Fi", "Thriller"}, Language: "en", Rating: 8.4, Popularity: 85, ContentRating: "TV-14"}
	ep1      = core.Title{Key: core.EpisodeKey(9, 1, 1), Type: core.TypeEpisode, Title: "Ep1", ShowTitle: "Severance", Season: 1, Episode: 1, ContentRating: "TV-14"}
	ep2      = core.Title{Key: core.EpisodeKey(9, 1, 2), Type: core.TypeEpisode, Title: "Ep2", ShowTitle: "Severance", Season: 1, Episode: 2, ContentRating: "TV-14"}
	ep3      = core.Title{Key: core.EpisodeKey(9, 1, 3), Type: core.TypeEpisode, Title: "Ep3", ShowTitle: "Severance", Season: 1, Episode: 3, ContentRating: "TV-14"}
	inProg   = core.Title{Key: core.MovieKey(6), Type: core.TypeMovie, Title: "Half Watched", Year: 2020, Genres: []string{"Drama"}, Popularity: 10, ContentRating: "PG-13"}
	newMovie = core.Title{Key: core.MovieKey(7), Type: core.TypeMovie, Title: "Brand New", Year: 2026, Genres: []string{"Drama"}, Popularity: 5, Released: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), ContentRating: "PG-13"}
	sicario  = core.Title{Key: core.MovieKey(8), Type: core.TypeMovie, Title: "Sicario", Year: 2015, Genres: []string{"Thriller", "Drama"}, Directors: []core.Person{{TMDB: 100, Name: "Villeneuve"}}, Language: "en", Rating: 7.4, Popularity: 40, Released: time.Date(2015, 9, 1, 0, 0, 0, 0, time.UTC), ContentRating: "PG-13"}
)

func fixture(t *testing.T) (*Recommender, *state.State, *core.User) {
	t.Helper()
	r, st, user, _ := fixtureWith(t, nil)
	return r, st, user
}

// fixtureWith builds the standard scenario; tweak adjusts Options before
// the Recommender is constructed.
func fixtureWith(t *testing.T, tweak func(*Options)) (*Recommender, *state.State, *core.User, *fakeCatalog) {
	t.Helper()
	ctx := context.Background()
	cat := newFake()
	for _, title := range []core.Title{arrival, dune, dune2, comedy, goryR, sever, ep1, ep2, ep3, inProg, newMovie, sicario} {
		cat.add(title)
	}
	cat.similar[arrival.Key] = []core.RatingKey{dune.Key, dune2.Key, goryR.Key, comedy.Key, sicario.Key}
	cat.similar[dune.Key] = []core.RatingKey{dune2.Key, arrival.Key}
	cat.lists["trending_week:movie"] = []core.RatingKey{comedy.Key, dune2.Key}
	cat.lists["trending_week:show"] = []core.RatingKey{sever.Key}
	cat.lists["top_rated:movie"] = []core.RatingKey{dune2.Key}
	cat.lists["popular:movie"] = []core.RatingKey{comedy.Key}
	cat.colls[55] = []core.RatingKey{dune.Key, dune2.Key}
	cat.collName[55] = "Dune Collection"

	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	st := state.New(db, state.Options{})
	// Watched Arrival (rated 9) and Dune; finished Severance E1; half-way through inProg.
	_ = st.Scrobble(ctx, 1, arrival.Key)
	_ = st.Rate(ctx, 1, arrival.Key, 9)
	_ = st.Scrobble(ctx, 1, dune.Key)
	_ = st.Scrobble(ctx, 1, ep1.Key)
	_ = st.Progress(ctx, 1, inProg.Key, 50*time.Minute, 100*time.Minute, "paused")

	opts := Options{Catalog: cat, State: st}
	if tweak != nil {
		tweak(&opts)
	}
	r := New(opts)
	t.Cleanup(r.Close)
	user := &core.User{ID: 1, MaxContentRating: "PG-13"}
	return r, st, user, cat
}

func hubIDs(hubs []core.Hub) []string {
	out := make([]string, len(hubs))
	for i, h := range hubs {
		out[i] = h.ID
	}
	return out
}

func findHub(hubs []core.Hub, idPrefix string) *core.Hub {
	for i := range hubs {
		if hubs[i].ID == idPrefix || strings.HasPrefix(hubs[i].ID, idPrefix) {
			return &hubs[i]
		}
	}
	return nil
}

// ---- tests -----------------------------------------------------------------

func TestRelatedHubs(t *testing.T) {
	ctx := context.Background()
	r, _, user := fixture(t)

	rel, err := r.Related(ctx, user, dune.Key, 10)
	if err != nil {
		t.Fatal(err)
	}
	if findHub(rel, "movie.similar") == nil {
		t.Fatalf("related: %v", hubIDs(rel))
	}
	fr := findHub(rel, "movie.collection")
	if fr == nil || len(fr.Items) != 1 || fr.Items[0].Key != dune2.Key {
		t.Fatalf("franchise related (self excluded): %+v", fr)
	}
	if findHub(rel, "movie.by.actor.or.director.100") == nil {
		t.Fatalf("director related missing: %v", hubIDs(rel))
	}
	for _, h := range rel {
		for _, it := range h.Items {
			if it.Key == dune.Key {
				t.Fatal("related hub contains the title itself")
			}
		}
	}
}

// A restricted viewer's limit compares age levels and never admits
// unrated titles, the same rule the PMS boundary enforces.
func TestFilterTitlesByLevel(t *testing.T) {
	in := []core.Title{{Key: 1, ContentRating: "TV-14"}, {Key: 2, ContentRating: "R"}, {Key: 3}, {Key: 4, ContentRating: "NR"}, {Key: 5, ContentRating: "gb/12A"}, {Key: 6, ContentRating: "PG"}}
	got := keysOf(FilterTitles(in, &core.User{MaxContentRating: "PG-13"}))
	if len(got) != 3 || got[0] != 1 || got[1] != 5 || got[2] != 6 {
		t.Fatalf("FilterTitles = %v", got)
	}
	if len(FilterTitles(in, &core.User{})) != len(in) {
		t.Fatal("no limit filtered")
	}
}

func keysOf(items []core.Title) []core.RatingKey {
	out := make([]core.RatingKey, len(items))
	for i, t := range items {
		out[i] = t.Key
	}
	return out
}
