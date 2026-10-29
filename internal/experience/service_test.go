package experience

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/state"
	"github.com/braedonsaunders/ether/internal/store"
	"testing"
)

type serviceState struct{ core.State }

func (serviceState) Get(_ context.Context, _ int64, k core.RatingKey) (core.WatchStatus, error) {
	return core.WatchStatus{Key: k}, nil
}
func (serviceState) GetMany(_ context.Context, _ int64, keys []core.RatingKey) (map[core.RatingKey]core.WatchStatus, error) {
	m := map[core.RatingKey]core.WatchStatus{}
	for _, k := range keys {
		m[k] = core.WatchStatus{Key: k}
	}
	return m, nil
}
func TestServiceEnforcesVisibilityBeforePagination(t *testing.T) {
	ctx := context.Background()
	parent := core.Title{Key: core.ShowKey(4), Type: core.TypeShow}
	hidden := core.Title{Key: core.EpisodeKey(4, 1, 1), ShowKey: parent.Key, Type: core.TypeEpisode, Adult: true}
	visible := core.Title{Key: core.EpisodeKey(4, 1, 2), ShowKey: parent.Key, Type: core.TypeEpisode}
	all := map[core.RatingKey]*core.Title{parent.Key: &parent, hidden.Key: &hidden, visible.Key: &visible}
	get := func(_ context.Context, _ *core.User, k core.RatingKey) (*core.Title, error) {
		if t := all[k]; t != nil {
			return t, nil
		}
		return nil, core.ErrNotFound
	}
	vis := Visibility{SectionFor: func(core.MediaType) (int, bool) { return 2, true }, GetTitle: func(ctx context.Context, k core.RatingKey) (*core.Title, error) { return get(ctx, nil, k) }}
	s, err := NewService(ServiceDeps{Active: func(_ context.Context, id int64) (*core.User, error) {
		if id == 1 {
			return &core.User{ID: 1, AllowedSections: []int{2}}, nil
		}
		return nil, core.ErrNotFound
	}, Libraries: []Library{{ID: 2, Type: core.TypeShow}}, Visibility: vis, Get: get, Children: func(context.Context, *core.User, core.RatingKey) ([]core.Title, error) {
		return []core.Title{hidden, visible}, nil
	}, State: serviceState{}, Rows: &RowsService{}, Protocol: "jellyfin", Search: Search{Catalog: func(context.Context, string, []core.MediaType, int) ([]core.Title, error) { return nil, nil }}})
	if err != nil {
		t.Fatal(err)
	}
	s.d.IsWatchlisted = func(_ context.Context, _ *core.User, k core.RatingKey) (bool, error) { return k == visible.Key, nil }
	s.d.IsFavorite = func(_ context.Context, _ *core.User, key core.RatingKey) (bool, error) {
		return key == visible.Key, nil
	}
	detail, err := s.Item(ctx, frontend.ItemRequest{Viewer: &core.User{ID: 1}, Key: visible.Key})
	if err != nil || !detail.UserData.IsFavorite || !detail.UserData.InWatchlist {
		t.Fatalf("favorite item: %+v %v", detail, err)
	}
	parentDetail, err := s.Item(ctx, frontend.ItemRequest{Viewer: &core.User{ID: 1}, Key: parent.Key})
	if err != nil || parentDetail.UserData.IsFavorite {
		t.Fatalf("nonfavorite item: %+v %v", parentDetail, err)
	}
	page, err := s.Children(ctx, &core.User{ID: 1}, core.Device{}, parent.Key, frontend.ItemQuery{Limit: 1})
	if err != nil || page.Total != 1 || len(page.Items) != 1 || page.Items[0].Key != visible.Key {
		t.Fatalf("page %+v %v", page, err)
	}
	if _, err := s.Item(ctx, frontend.ItemRequest{Viewer: &core.User{ID: 1}, Key: hidden.Key}); err == nil {
		t.Fatal("adult item exposed")
	}
	data, err := s.UserData(ctx, &core.User{ID: 1}, []core.RatingKey{hidden.Key, visible.Key})
	if err != nil || len(data) != 1 || !data[visible.Key].IsFavorite || !data[visible.Key].InWatchlist {
		t.Fatalf("userdata %+v %v", data, err)
	}
	if _, err := s.Views(ctx, &core.User{ID: 2}); err == nil {
		t.Fatal("inactive viewer admitted")
	}
}

type mutationState struct {
	serviceState
	watched, unwatched int
	rating             float64
}

func (s *mutationState) Scrobble(context.Context, int64, core.RatingKey) error {
	s.watched++
	return nil
}
func (s *mutationState) Unscrobble(context.Context, int64, core.RatingKey) error {
	s.unwatched++
	return nil
}
func (s *mutationState) ScrobbleMany(_ context.Context, _ int64, keys []core.RatingKey) error {
	s.watched += len(keys)
	return nil
}
func (s *mutationState) UnscrobbleMany(_ context.Context, _ int64, keys []core.RatingKey) error {
	s.unwatched += len(keys)
	return nil
}
func (s *mutationState) Rate(_ context.Context, _ int64, _ core.RatingKey, r float64) error {
	s.rating = r
	return nil
}
func TestMutationsRequireVisibleActiveViewer(t *testing.T) {
	ctx := context.Background()
	state := &mutationState{}
	title := &core.Title{Key: core.MovieKey(4), Type: core.TypeMovie}
	favoriteCalls := 0
	watchlistCalls := 0
	s := &Service{d: ServiceDeps{Active: func(_ context.Context, id int64) (*core.User, error) {
		if id == 1 {
			return &core.User{ID: 1}, nil
		}
		return nil, core.ErrNotFound
	}, Get: func(context.Context, *core.User, core.RatingKey) (*core.Title, error) { return title, nil }, Visibility: Visibility{SectionFor: func(core.MediaType) (int, bool) { return 1, true }}, State: state, Favorites: func(_ context.Context, _ *core.User, _ core.RatingKey, on bool) (bool, error) {
		favoriteCalls++
		return true, nil
	}}}
	s.d.SetWatchlistSource = func(_ context.Context, _ *core.User, _ core.RatingKey, _ bool) error { watchlistCalls++; return nil }
	v := &core.User{ID: 1}
	if err := s.SetPlayed(ctx, v, title.Key); err != nil {
		t.Fatal(err)
	}
	if err := s.SetUnplayed(ctx, v, title.Key); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRating(ctx, v, title.Key, 7); err != nil {
		t.Fatal(err)
	}
	if err := s.SetFavorite(ctx, v, title.Key, true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetFavorite(ctx, v, title.Key, false); err != nil {
		t.Fatal(err)
	}
	if err := s.SetWatchlist(ctx, v, title.Key, true); err != nil {
		t.Fatal(err)
	}
	if state.watched != 1 || state.unwatched != 1 || state.rating != 7 || favoriteCalls != 2 || watchlistCalls != 1 {
		t.Fatal("mutation was not routed")
	}
	title.Adult = true
	if err := s.SetPlayed(ctx, v, title.Key); err == nil || state.watched != 1 {
		t.Fatal("hidden title mutated")
	}
	if err := s.SetWatchlist(ctx, v, title.Key, false); err == nil || watchlistCalls != 1 {
		t.Fatal("hidden watchlist mutation")
	}
	if err := s.SetFavorite(ctx, &core.User{ID: 2}, title.Key, true); err == nil || favoriteCalls != 2 {
		t.Fatal("inactive viewer mutated favorites")
	}
}

func TestRecursiveChildrenSeasonAndAuthorization(t *testing.T) {
	ctx := context.Background()
	show := core.Title{Key: core.ShowKey(42), Type: core.TypeShow}
	a := core.Title{Key: core.EpisodeKey(42, 1, 1), ShowKey: show.Key, Type: core.TypeEpisode, Season: 1}
	b := core.Title{Key: core.EpisodeKey(42, 2, 1), ShowKey: show.Key, Type: core.TypeEpisode, Season: 2}
	hidden := core.Title{Key: core.EpisodeKey(42, 2, 2), ShowKey: show.Key, Type: core.TypeEpisode, Season: 2, Adult: true}
	v := &core.User{ID: 1}
	s := &Service{d: ServiceDeps{Active: func(context.Context, int64) (*core.User, error) { return v, nil }, Get: func(_ context.Context, _ *core.User, k core.RatingKey) (*core.Title, error) {
		if k == show.Key {
			return &show, nil
		}
		return nil, core.ErrNotFound
	}, Grandchildren: func(context.Context, *core.User, core.RatingKey) ([]core.Title, error) {
		return []core.Title{a, b, hidden}, nil
	}, Visibility: Visibility{SectionFor: func(core.MediaType) (int, bool) { return 1, true }, GetTitle: func(context.Context, core.RatingKey) (*core.Title, error) { return &show, nil }}}}
	season := 2
	p, err := s.Children(ctx, v, core.Device{}, show.Key, frontend.ItemQuery{Recursive: true, Types: []core.MediaType{core.TypeEpisode}, Season: &season})
	if err != nil || p.Total != 1 || len(p.Items) != 1 || p.Items[0].Key != b.Key {
		t.Fatalf("page %+v: %v", p, err)
	}
}

type serviceCollections struct {
	core.Collections
	collection core.Collection
	titles     []core.Title
}

func (c serviceCollections) Get(context.Context, *core.User, core.RatingKey) (*core.Collection, error) {
	return &c.collection, nil
}
func (c serviceCollections) Items(_ context.Context, _ *core.User, _ core.RatingKey, offset, limit int) (core.Page, error) {
	start := min(offset, len(c.titles))
	end := min(start+limit, len(c.titles))
	return core.Page{Items: c.titles[start:end], Total: len(c.titles)}, nil
}

type servicePlaylists struct {
	core.Playlists
	playlist core.Playlist
}

func (p servicePlaylists) Get(context.Context, int64, core.RatingKey) (*core.Playlist, error) {
	return &p.playlist, nil
}
func TestCollectionsAndPlaylistsDoNotExposeHiddenKeys(t *testing.T) {
	ctx := context.Background()
	v := &core.User{ID: 1}
	visible := core.Title{Key: core.MovieKey(1)}
	hidden := core.Title{Key: core.MovieKey(2), Adult: true}
	ck := core.CollectionKey(3)
	pk := core.PlaylistKey(4)
	get := func(_ context.Context, _ *core.User, k core.RatingKey) (*core.Title, error) {
		if k == visible.Key {
			return &visible, nil
		}
		if k == hidden.Key {
			return &hidden, nil
		}
		return nil, core.ErrNotFound
	}
	s := &Service{d: ServiceDeps{Active: func(context.Context, int64) (*core.User, error) { return v, nil }, Get: get, Visibility: Visibility{SectionFor: func(core.MediaType) (int, bool) { return 1, true }}, Collections: serviceCollections{collection: core.Collection{Key: ck, Type: core.TypeMovie}, titles: []core.Title{visible, hidden}}, Playlists: servicePlaylists{playlist: core.Playlist{Key: pk, UserID: 1, Items: []core.RatingKey{hidden.Key, visible.Key}}}}}
	c, err := s.Item(ctx, frontend.ItemRequest{Viewer: v, Key: ck})
	if err != nil || c.Collection == nil || c.Collection.ChildCount != 1 {
		t.Fatalf("collection %+v %v", c, err)
	}
	p, err := s.Item(ctx, frontend.ItemRequest{Viewer: v, Key: pk})
	if err != nil || p.Playlist == nil || len(p.Playlist.Items) != 1 || p.Playlist.Items[0] != visible.Key {
		t.Fatalf("playlist %+v %v", p, err)
	}
}

func TestLibraryItemsPolicyBeforePaginationAndTotal(t *testing.T) {
	v := &core.User{ID: 1, AllowedSections: []int{1}}
	all := []core.Title{{Key: core.MovieKey(1), Type: core.TypeMovie, Adult: true}, {Key: core.MovieKey(2), Type: core.TypeMovie, Title: "ok"}, {Key: core.MovieKey(3), Type: core.TypeMovie, Title: "next"}}
	s := &Service{d: ServiceDeps{Active: func(context.Context, int64) (*core.User, error) { return v, nil }, Libraries: []Library{{ID: 1, Type: core.TypeMovie}}, Visibility: Visibility{SectionFor: func(core.MediaType) (int, bool) { return 1, true }}, Browse: func(_ context.Context, q core.BrowseQuery) (core.Page, error) {
		start := min(q.Offset, len(all))
		return core.Page{Items: all[start:], Total: len(all)}, nil
	}}}
	p, err := s.LibraryItems(context.Background(), v, core.Device{}, "1", frontend.ItemQuery{Offset: 1, Limit: 1})
	if err != nil || p.Total != 2 || len(p.Items) != 1 || p.Items[0].Key != all[2].Key {
		t.Fatalf("page %+v %v", p, err)
	}
	if _, err := s.LibraryItems(context.Background(), v, core.Device{}, "2", frontend.ItemQuery{}); err == nil {
		t.Fatal("unauthorized library")
	}
}

type atomicLikes struct {
	mutationState
	calls int
	value *bool
}

func (s *atomicLikes) SetLikes(_ context.Context, _ int64, _ core.RatingKey, v *bool) error {
	s.calls++
	s.value = v
	return nil
}
func TestSetLikesRequiresAtomicCapabilityAndVisibleItem(t *testing.T) {
	ctx := context.Background()
	title := &core.Title{Key: core.MovieKey(5), Type: core.TypeMovie}
	v := &core.User{ID: 1}
	likes := &atomicLikes{}
	s := &Service{d: ServiceDeps{Active: func(context.Context, int64) (*core.User, error) { return v, nil }, Get: func(context.Context, *core.User, core.RatingKey) (*core.Title, error) { return title, nil }, Visibility: Visibility{SectionFor: func(core.MediaType) (int, bool) { return 1, true }}, State: likes}}
	yes := true
	if err := s.SetLikes(ctx, v, title.Key, &yes); err != nil || likes.calls != 1 || likes.value == nil || !*likes.value {
		t.Fatalf("likes routed: %d %v", likes.calls, err)
	}
	title.Adult = true
	if err := s.SetLikes(ctx, v, title.Key, nil); err == nil || likes.calls != 1 {
		t.Fatal("hidden title mutated")
	}
	title.Adult = false
	s.d.State = serviceState{}
	if err := s.SetLikes(ctx, v, title.Key, nil); err == nil {
		t.Fatal("non-atomic implementation accepted")
	}
}

func TestItemAndUserDataDenyHiddenParent(t *testing.T) {
	parent := core.ShowKey(7)
	child := core.Title{Key: core.EpisodeKey(7, 1, 1), ShowKey: parent, Type: core.TypeEpisode}
	viewer := &core.User{ID: 1}
	s := &Service{d: ServiceDeps{Active: func(context.Context, int64) (*core.User, error) { return viewer, nil }, Get: func(context.Context, *core.User, core.RatingKey) (*core.Title, error) { return &child, nil }, Visibility: Visibility{SectionFor: func(core.MediaType) (int, bool) { return 1, true }, GetTitle: func(context.Context, core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: parent, Adult: true}, nil
	}}, State: serviceState{}}}
	if _, err := s.Item(context.Background(), frontend.ItemRequest{Viewer: viewer, Key: child.Key}); err == nil {
		t.Fatal("hidden show episode leaked")
	}
	data, err := s.UserData(context.Background(), viewer, []core.RatingKey{child.Key})
	if err != nil || len(data) != 0 {
		t.Fatalf("hidden userdata leaked %+v %v", data, err)
	}
}

func TestEntityFavoritesPolicy(t *testing.T) {
	viewer := &core.User{ID: 1}
	allowed := frontend.FavoriteRef{Kind: frontend.FavoritePerson, ID: "allowed"}
	denied := frontend.FavoriteRef{Kind: frontend.FavoritePerson, ID: "denied"}
	calls := 0
	s := &Service{d: ServiceDeps{Active: func(context.Context, int64) (*core.User, error) { return viewer, nil }, CanFavoriteEntity: func(_ context.Context, _ *core.User, r frontend.FavoriteRef) bool { return r == allowed }, SetEntityFavoriteSource: func(context.Context, *core.User, frontend.FavoriteRef, bool) error { calls++; return nil }, EntityFavoritesSource: func(_ context.Context, _ *core.User, refs []frontend.FavoriteRef) (map[frontend.FavoriteRef]bool, error) {
		if len(refs) != 1 || refs[0] != allowed {
			t.Fatal("unauthorized reference reached backend")
		}
		return map[frontend.FavoriteRef]bool{allowed: true, denied: true}, nil
	}}}
	result, err := s.EntityFavorites(context.Background(), viewer, []frontend.FavoriteRef{allowed, denied})
	if err != nil || len(result) != 1 || !result[allowed] {
		t.Fatalf("favorites %+v %v", result, err)
	}
	if err := s.SetEntityFavorite(context.Background(), viewer, denied, true); err == nil || calls != 0 {
		t.Fatal("unauthorized favorite accepted")
	}
	if err := s.SetEntityFavorite(context.Background(), viewer, allowed, true); err != nil || calls != 1 {
		t.Fatalf("allowed favorite %v", err)
	}
}

func TestLibraryRecursiveEpisodesBeforePagination(t *testing.T) {
	v := &core.User{ID: 1}
	show := core.Title{Key: core.ShowKey(7), Type: core.TypeShow}
	first := core.Title{Key: core.EpisodeKey(7, 1, 1), Type: core.TypeEpisode, ShowKey: show.Key, Season: 1, Adult: true}
	second := core.Title{Key: core.EpisodeKey(7, 2, 1), Type: core.TypeEpisode, ShowKey: show.Key, Season: 2}
	s := &Service{d: ServiceDeps{Active: func(context.Context, int64) (*core.User, error) { return v, nil }, Libraries: []Library{{ID: 2, Type: core.TypeShow}}, Browse: func(context.Context, core.BrowseQuery) (core.Page, error) {
		return core.Page{Items: []core.Title{show}, Total: 1}, nil
	}, Grandchildren: func(context.Context, *core.User, core.RatingKey) ([]core.Title, error) {
		return []core.Title{first, second}, nil
	}, Visibility: Visibility{SectionFor: func(core.MediaType) (int, bool) { return 2, true }, GetTitle: func(context.Context, core.RatingKey) (*core.Title, error) { return &show, nil }}}}
	season := 2
	p, err := s.LibraryItems(context.Background(), v, core.Device{}, "2", frontend.ItemQuery{Recursive: true, Types: []core.MediaType{core.TypeEpisode}, Season: &season, Limit: 1})
	if err != nil || p.Total != 1 || len(p.Items) != 1 || p.Items[0].Key != second.Key {
		t.Fatalf("page %+v %v", p, err)
	}
}

func TestQueryFiltersBeforeLimit(t *testing.T) {
	v := &core.User{ID: 1}
	titles := []core.Title{{Key: core.MovieKey(1), Type: core.TypeMovie, Genres: []string{"Drama"}}, {Key: core.MovieKey(2), Type: core.TypeMovie, Genres: []string{"Comedy"}}, {Key: core.MovieKey(3), Type: core.TypeMovie, Genres: []string{"Drama"}}}
	s := &Service{d: ServiceDeps{Active: func(context.Context, int64) (*core.User, error) { return v, nil }, Libraries: []Library{{ID: 1, Type: core.TypeMovie}}, Browse: func(context.Context, core.BrowseQuery) (core.Page, error) {
		return core.Page{Items: titles, Total: len(titles)}, nil
	}, Visibility: Visibility{SectionFor: func(core.MediaType) (int, bool) { return 1, true }}, IsFavorite: func(_ context.Context, _ *core.User, k core.RatingKey) (bool, error) { return k == titles[2].Key, nil }}}
	favorite := true
	p, err := s.LibraryItems(context.Background(), v, core.Device{}, "1", frontend.ItemQuery{Favorite: &favorite, Filter: core.Filter{Genres: []string{"Drama"}}, Limit: 1})
	if err != nil || p.Total != 1 || len(p.Items) != 1 || p.Items[0].Key != titles[2].Key {
		t.Fatalf("filtered page %+v %v", p, err)
	}
}

func TestSimilarReturnsHonestContinuationAndFilters(t *testing.T) {
	ctx := context.Background()
	v := &core.User{ID: 1}
	base := core.Title{Key: core.MovieKey(1), Type: core.TypeMovie}
	hidden := core.Title{Key: core.MovieKey(2), Type: core.TypeMovie, Adult: true}
	ok := core.Title{Key: core.MovieKey(3), Type: core.TypeMovie}
	s := &Service{d: ServiceDeps{Active: func(context.Context, int64) (*core.User, error) { return v, nil }, Get: func(context.Context, *core.User, core.RatingKey) (*core.Title, error) { return &base, nil }, Visibility: Visibility{SectionFor: func(core.MediaType) (int, bool) { return 1, true }}, SimilarSource: func(context.Context, core.RatingKey, int) ([]core.Title, error) { return []core.Title{hidden, ok}, nil }}}
	result, err := s.Similar(ctx, v, core.Device{}, base.Key, frontend.ItemQuery{Limit: 1})
	if err != nil || result.TotalExact || result.Page.Total != 1 || result.Continuation != frontend.ContinuationUnknown || result.Page.Items[0].Key != ok.Key {
		t.Fatalf("similar %+v %v", result, err)
	}
}

func TestAggregateLibrariesRespectsViewerSections(t *testing.T) {
	v := &core.User{ID: 1, AllowedSections: []int{1}}
	movie := core.Title{Key: core.MovieKey(1), Type: core.TypeMovie, Title: "Movie"}
	show := core.Title{Key: core.ShowKey(2), Type: core.TypeShow, Title: "Show"}
	s := &Service{d: ServiceDeps{Active: func(context.Context, int64) (*core.User, error) { return v, nil }, Libraries: []Library{{ID: 1, Type: core.TypeMovie}, {ID: 2, Type: core.TypeShow}}, Visibility: Visibility{SectionFor: func(t core.MediaType) (int, bool) {
		if t == core.TypeMovie {
			return 1, true
		}
		return 2, true
	}}, Browse: func(_ context.Context, q core.BrowseQuery) (core.Page, error) {
		if q.Filter.Type == core.TypeMovie {
			return core.Page{Items: []core.Title{movie}, Total: 1}, nil
		}
		return core.Page{Items: []core.Title{show}, Total: 1}, nil
	}}}
	p, err := s.LibraryItems(context.Background(), v, core.Device{}, "", frontend.ItemQuery{Limit: 1})
	if err != nil || p.Total != 1 || len(p.Items) != 1 || p.Items[0].Key != movie.Key {
		t.Fatalf("aggregate %+v %v", p, err)
	}
}
func TestContainerScrobbleChecksAllChildrenBeforeWrites(t *testing.T) {
	ctx := context.Background()
	v := &core.User{ID: 1}
	show := core.Title{Key: core.ShowKey(1), Type: core.TypeShow}
	one := core.Title{Key: core.EpisodeKey(1, 1, 1), Type: core.TypeEpisode, ShowKey: show.Key}
	two := core.Title{Key: core.EpisodeKey(1, 1, 2), Type: core.TypeEpisode, ShowKey: show.Key, Adult: true}
	state := &mutationState{}
	get := func(_ context.Context, _ *core.User, k core.RatingKey) (*core.Title, error) {
		switch k {
		case show.Key:
			return &show, nil
		case one.Key:
			return &one, nil
		case two.Key:
			return &two, nil
		}
		return nil, core.ErrNotFound
	}
	s := &Service{d: ServiceDeps{Active: func(context.Context, int64) (*core.User, error) { return v, nil }, Get: get, Grandchildren: func(context.Context, *core.User, core.RatingKey) ([]core.Title, error) {
		return []core.Title{one, two}, nil
	}, Visibility: Visibility{SectionFor: func(core.MediaType) (int, bool) { return 1, true }, GetTitle: func(context.Context, core.RatingKey) (*core.Title, error) { return &show, nil }}, State: state}}
	if err := s.SetPlayed(ctx, v, show.Key); err == nil || state.watched != 0 {
		t.Fatal("unauthorized child caused partial writes")
	}
	two.Adult = false
	if err := s.SetPlayed(ctx, v, show.Key); err != nil || state.watched != 2 {
		t.Fatalf("scrobble %d %v", state.watched, err)
	}
}

func TestStateMutationsCarrySurfaceOrigin(t *testing.T) {
	ctx := context.Background()
	viewer := &core.User{ID: 1}
	title := &core.Title{Key: core.MovieKey(42), Type: core.TypeMovie}
	watch := state.New(store.OpenTest(t), state.Options{})
	var origins []core.Protocol
	unsub := watch.Subscribe(func(event state.Event) { origins = append(origins, event.Origin.Surface) })
	defer unsub()
	s := &Service{d: ServiceDeps{Protocol: "jellyfin", Active: func(context.Context, int64) (*core.User, error) { return viewer, nil }, Get: func(context.Context, *core.User, core.RatingKey) (*core.Title, error) { return title, nil }, Visibility: Visibility{SectionFor: func(core.MediaType) (int, bool) { return 1, true }}, State: watch}}
	yes := true
	if err := s.SetPlayed(ctx, viewer, title.Key); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRating(ctx, viewer, title.Key, 4); err != nil {
		t.Fatal(err)
	}
	if err := s.SetLikes(ctx, viewer, title.Key, &yes); err != nil {
		t.Fatal(err)
	}
	if len(origins) < 3 {
		t.Fatalf("missing events %v", origins)
	}
	for _, origin := range origins {
		if origin != "jellyfin" {
			t.Fatalf("missing origin: %v", origins)
		}
	}
}

func TestFacetsContainOnlyVisibleTitlesAndNames(t *testing.T) {
	v := &core.User{ID: 1}
	s := &Service{d: ServiceDeps{Active: func(context.Context, int64) (*core.User, error) { return v, nil }, Libraries: []Library{{ID: 1, Type: core.TypeMovie}}, Visibility: Visibility{SectionFor: func(core.MediaType) (int, bool) { return 1, true }}, Browse: func(context.Context, core.BrowseQuery) (core.Page, error) {
		return core.Page{Items: []core.Title{{Key: core.MovieKey(1), Type: core.TypeMovie, Genres: []string{"Drama"}}, {Key: core.MovieKey(2), Type: core.TypeMovie, Adult: true, Genres: []string{"Hidden"}}}, Total: 2}, nil
	}}}
	facets, err := s.Facets(context.Background(), v, core.Device{}, "1", "genre")
	if err != nil || len(facets) != 1 || facets[0].Key != "Drama" || facets[0].Title != "Drama" || facets[0].Count != 1 {
		t.Fatalf("facets %+v %v", facets, err)
	}
	if len(facets[0].ID) != 32 || facets[0].ID == facetID("studio", "Drama") {
		t.Fatal("facet namespace is not stable and distinct")
	}
	resolved, err := s.ResolveFacet(context.Background(), v, core.Device{}, "1", "genre", facets[0].ID)
	if err != nil || resolved.Key != "Drama" {
		t.Fatalf("resolve %+v %v", resolved, err)
	}
	if _, err = s.ResolveFacet(context.Background(), v, core.Device{}, "1", "genre", facetID("genre", "Hidden")); err == nil {
		t.Fatal("hidden facet resolved")
	}

}

func TestPersonFacetsAreViewerScopedAndDeduplicated(t *testing.T) {
	v := &core.User{ID: 1}
	p := core.Person{TMDB: 42, Name: "Actor"}
	hidden := core.Person{TMDB: 99, Name: "Hidden"}
	s := &Service{d: ServiceDeps{Active: func(context.Context, int64) (*core.User, error) { return v, nil }, Libraries: []Library{{ID: 1, Type: core.TypeMovie}}, Visibility: Visibility{SectionFor: func(core.MediaType) (int, bool) { return 1, true }}, Browse: func(context.Context, core.BrowseQuery) (core.Page, error) {
		return core.Page{Items: []core.Title{{Key: core.MovieKey(1), Type: core.TypeMovie, Cast: []core.Person{p}, Directors: []core.Person{p}}, {Key: core.MovieKey(2), Type: core.TypeMovie, Adult: true, Cast: []core.Person{hidden}}}, Total: 2}, nil
	}}}
	facets, err := s.Facets(context.Background(), v, core.Device{}, "1", "person")
	if err != nil || len(facets) != 1 || facets[0].Key != "42" || facets[0].Title != "Actor" || facets[0].Count != 1 {
		t.Fatalf("person facets %+v %v", facets, err)
	}
	if _, err = s.ResolveFacet(context.Background(), v, core.Device{}, "1", "person", facetID("person", "99")); err == nil {
		t.Fatal("hidden person resolved")
	}
}
