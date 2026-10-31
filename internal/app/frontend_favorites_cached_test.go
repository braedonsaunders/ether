package app

import (
	"context"
	"errors"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/favorites"
)

func TestFavoriteTitlesCachedInheritedPolicy(t *testing.T) {
	ctx := context.Background()
	viewer := &core.User{ID: 1, MaxContentRating: "PG"}
	show := core.Title{Key: core.ShowKey(4), Type: core.TypeShow, ContentRating: "TV-MA"}
	episode := core.Title{Key: core.EpisodeKey(4, 1, 1), Type: core.TypeEpisode, ShowKey: show.Key, ContentRating: "TV-G"}
	lookup := map[core.RatingKey]core.Title{episode.Key: episode, show.Key: show}
	cached := func(_ context.Context, keys []core.RatingKey) (map[core.RatingKey]core.Title, error) {
		out := map[core.RatingKey]core.Title{}
		for _, key := range keys {
			if title, ok := lookup[key]; ok {
				out[key] = title
			}
		}
		return out, nil
	}
	remote := 0
	vis := experience.Visibility{
		SectionFor: func(core.MediaType) (int, bool) { return 2, true },
		GetTitle: func(context.Context, core.RatingKey) (*core.Title, error) {
			remote++
			return nil, errors.New("remote lookup")
		},
	}
	if ok, err := experience.FavoriteTitlesCached(ctx, viewer, []core.RatingKey{episode.Key}, vis, cached); err != nil || ok || remote != 0 {
		t.Fatalf("restricted parent allowed: %v %v remote %d", ok, err, remote)
	}
	delete(lookup, show.Key)
	if ok, err := experience.FavoriteTitlesCached(ctx, viewer, []core.RatingKey{episode.Key}, vis, cached); err != nil || ok {
		t.Fatalf("missing parent allowed: %v %v", ok, err)
	}
	show.ContentRating = "TV-G"
	lookup[show.Key] = show
	if ok, err := experience.FavoriteTitlesCached(ctx, viewer, []core.RatingKey{episode.Key}, vis, cached); err != nil || !ok {
		t.Fatalf("visible item denied: %v %v", ok, err)
	}
	viewer.AllowedSections = []int{1}
	if ok, err := experience.FavoriteTitlesCached(ctx, viewer, []core.RatingKey{episode.Key}, vis, cached); err != nil || ok {
		t.Fatalf("revoked section allowed: %v %v", ok, err)
	}
}

func TestFavoritePolicyNilAppDependencies(t *testing.T) {
	a := &App{}
	ctx := context.Background()
	viewer := &core.User{ID: 1}
	vis := experience.Visibility{GetTitle: func(context.Context, core.RatingKey) (*core.Title, error) {
		t.Fatal("remote lookup")
		return nil, nil
	}}
	if ok, err := a.favoriteTitlesCached(ctx, viewer, []core.RatingKey{core.MovieKey(1)}, vis); err != nil || ok {
		t.Fatalf("nil catalog: %v %v", ok, err)
	}
	if ok, err := a.favoriteTitlesCached(ctx, nil, nil, vis); err != nil || ok {
		t.Fatalf("nil viewer: %v %v", ok, err)
	}
	subjects := []favorites.Subject{
		{Kind: favorites.KindTitle, ID: core.MovieKey(1).String()},
		{Kind: favorites.KindCollection, ID: core.CollectionKey(1).String()},
		{Kind: favorites.KindChannel, ID: "channel"},
		{Kind: favorites.KindPerson, ID: "tmdb:1"},
		{Kind: favorites.Kind("other"), ID: "x"},
	}
	for _, subject := range subjects {
		if a.favoriteAllowed(ctx, viewer, subject, vis) || a.favoriteAllowed(ctx, nil, subject, vis) {
			t.Fatalf("nil dependencies allowed %+v", subject)
		}
	}
	if string(favorites.KindTitle) != string(experience.FavoriteKindTitle) || string(favorites.KindPerson) != string(experience.FavoriteKindPerson) || string(favorites.KindCollection) != string(experience.FavoriteKindCollection) || string(favorites.KindChannel) != string(experience.FavoriteKindChannel) {
		t.Fatal("favorite kind strings drifted from the experience port")
	}
}

func TestFavoritePolicyDecisions(t *testing.T) {
	ctx := context.Background()
	viewer := &core.User{ID: 3, MaxContentRating: "PG"}
	show := core.Title{Key: core.ShowKey(4), Type: core.TypeShow, ContentRating: "TV-G"}
	episodeKey := core.EpisodeKey(4, 1, 2)
	episode := core.Title{Key: episodeKey, Type: core.TypeEpisode, ShowKey: show.Key, ContentRating: "TV-G"}
	movieKey := core.MovieKey(6)
	movie := core.Title{Key: movieKey, Type: core.TypeMovie, ContentRating: "PG"}
	collectionKey := core.CollectionKey(8)
	collection := &core.Collection{Key: collectionKey, Type: core.TypeMovie}
	dbErr := errors.New("database down")
	vis := experience.Visibility{SectionFor: func(core.MediaType) (int, bool) { return 2, true }}

	compareAllowed := func(subject experience.FavoriteSubject, sources experience.FavoriteSources, want bool) {
		t.Helper()
		got := experience.FavoriteAllowed(ctx, viewer, subject, vis, sources)
		if got != want {
			t.Fatalf("%+v got %v want %v", subject, got, want)
		}
	}

	compareAllowed(experience.FavoriteSubject{Kind: experience.FavoriteKindTitle, ID: movieKey.String()}, experience.FavoriteSources{
		Title: func(context.Context, core.RatingKey) (*core.Title, error) { return &movie, nil },
	}, true)
	compareAllowed(experience.FavoriteSubject{Kind: experience.FavoriteKindTitle, ID: movieKey.String()}, experience.FavoriteSources{
		Title: func(context.Context, core.RatingKey) (*core.Title, error) { return nil, dbErr },
	}, false)
	compareAllowed(experience.FavoriteSubject{Kind: experience.FavoriteKindTitle, ID: movieKey.String()}, experience.FavoriteSources{
		Title: func(context.Context, core.RatingKey) (*core.Title, error) {
			return &core.Title{Key: core.MovieKey(1), Type: core.TypeMovie}, nil
		},
	}, false)
	compareAllowed(experience.FavoriteSubject{Kind: experience.FavoriteKindTitle, ID: "bad"}, experience.FavoriteSources{
		Title: func(context.Context, core.RatingKey) (*core.Title, error) {
			t.Fatal("lookup")
			return nil, nil
		},
	}, false)
	compareAllowed(experience.FavoriteSubject{Kind: experience.FavoriteKindCollection, ID: collectionKey.String()}, experience.FavoriteSources{
		Collection: func(context.Context, *core.User, core.RatingKey) (*core.Collection, error) {
			return collection, nil
		},
	}, true)
	hidden := *collection
	viewer.AllowedSections = []int{1}
	compareAllowed(experience.FavoriteSubject{Kind: experience.FavoriteKindCollection, ID: collectionKey.String()}, experience.FavoriteSources{
		Collection: func(context.Context, *core.User, core.RatingKey) (*core.Collection, error) { return &hidden, nil },
	}, false)
	viewer.AllowedSections = nil
	compareAllowed(experience.FavoriteSubject{Kind: experience.FavoriteKindCollection, ID: collectionKey.String()}, experience.FavoriteSources{
		Collection: func(context.Context, *core.User, core.RatingKey) (*core.Collection, error) { return nil, dbErr },
	}, false)
	compareAllowed(experience.FavoriteSubject{Kind: experience.FavoriteKindChannel, ID: "news"}, experience.FavoriteSources{
		ChannelIDs: func(context.Context, *core.User) ([]string, error) { return []string{"sports", "news"}, nil },
	}, true)
	compareAllowed(experience.FavoriteSubject{Kind: experience.FavoriteKindChannel, ID: "secret"}, experience.FavoriteSources{
		ChannelIDs: func(context.Context, *core.User) ([]string, error) { return []string{"sports"}, nil },
	}, false)
	compareAllowed(experience.FavoriteSubject{Kind: experience.FavoriteKindChannel, ID: "news"}, experience.FavoriteSources{
		ChannelIDs: func(context.Context, *core.User) ([]string, error) { return nil, dbErr },
	}, false)
	compareAllowed(experience.FavoriteSubject{Kind: experience.FavoriteKindPerson, ID: "tmdb:01"}, experience.FavoriteSources{
		PersonPage: func(context.Context, int64, int, int) ([]core.Title, error) {
			t.Fatal("browse")
			return nil, nil
		},
	}, false)
	compareAllowed(experience.FavoriteSubject{Kind: experience.FavoriteKindPerson, ID: "tmdb:15"}, experience.FavoriteSources{
		PersonPage: func(_ context.Context, person int64, offset, limit int) ([]core.Title, error) {
			if person != 15 || offset != 0 || limit != 100 {
				t.Fatalf("query %d %d %d", person, offset, limit)
			}
			return []core.Title{episode}, nil
		},
	}, false)
	seen := map[int]int{}
	compareAllowed(experience.FavoriteSubject{Kind: experience.FavoriteKindPerson, ID: "tmdb:15"}, experience.FavoriteSources{
		PersonPage: func(_ context.Context, person int64, offset, limit int) ([]core.Title, error) {
			if person != 15 || limit != 100 || offset >= 1000 {
				t.Fatalf("query person %d offset %d limit %d", person, offset, limit)
			}
			seen[offset]++
			if offset == 100 {
				return nil, dbErr
			}
			items := make([]core.Title, 100)
			for i := range items {
				items[i] = core.Title{Type: core.TypeMovie, Adult: true}
			}
			return items, nil
		},
	}, false)
	if seen[0] != 1 || seen[100] != 1 || len(seen) != 2 {
		t.Fatalf("person error pages %+v", seen)
	}

	lookup := map[core.RatingKey]core.Title{episodeKey: episode, show.Key: show, movieKey: movie}
	cached := func(_ context.Context, keys []core.RatingKey) (map[core.RatingKey]core.Title, error) {
		out := make(map[core.RatingKey]core.Title, len(keys))
		for _, key := range keys {
			if title, ok := lookup[key]; ok {
				out[key] = title
			}
		}
		return out, nil
	}
	compareCached := func(user *core.User, keys []core.RatingKey, read func(context.Context, []core.RatingKey) (map[core.RatingKey]core.Title, error), want bool, wantErr error) {
		t.Helper()
		got, gotErr := experience.FavoriteTitlesCached(ctx, user, keys, vis, read)
		if got != want || !errors.Is(gotErr, wantErr) {
			t.Fatalf("keys %v got %v %v want %v %v", keys, got, gotErr, want, wantErr)
		}
	}
	compareCached(viewer, []core.RatingKey{episodeKey}, cached, true, nil)
	delete(lookup, show.Key)
	compareCached(viewer, []core.RatingKey{episodeKey}, cached, false, nil)
	lookup[show.Key] = show
	show.ContentRating = "TV-MA"
	lookup[show.Key] = show
	compareCached(viewer, []core.RatingKey{episodeKey}, cached, false, nil)
	compareCached(nil, []core.RatingKey{movieKey}, cached, false, nil)
	compareCached(viewer, []core.RatingKey{movieKey}, nil, false, nil)
	compareCached(viewer, []core.RatingKey{movieKey}, func(context.Context, []core.RatingKey) (map[core.RatingKey]core.Title, error) {
		return nil, dbErr
	}, false, dbErr)
	compareCached(viewer, []core.RatingKey{episodeKey}, func(_ context.Context, keys []core.RatingKey) (map[core.RatingKey]core.Title, error) {
		if len(keys) == 1 && keys[0] == episodeKey {
			return map[core.RatingKey]core.Title{episodeKey: episode}, nil
		}
		return nil, dbErr
	}, false, dbErr)
	compareCached(viewer, []core.RatingKey{core.MovieKey(99)}, cached, false, nil)
}

func TestResolveBrowseParentsPreservesGetOutcomes(t *testing.T) {
	ctx := context.Background()
	warm, cold, stale, eligible, missing := core.ShowKey(1), core.ShowKey(2), core.ShowKey(3), core.ShowKey(4), core.ShowKey(5)
	calls := map[core.RatingKey]int{}
	get := func(_ context.Context, key core.RatingKey) (*core.Title, error) {
		calls[key]++
		if key == missing {
			return nil, core.ErrNotFound
		}
		return &core.Title{Key: key, Type: core.TypeShow}, nil
	}
	got := resolveBrowseParents(ctx, []core.RatingKey{warm, warm, cold, stale, eligible, missing, missing}, get)
	for _, key := range []core.RatingKey{warm, cold, stale, eligible} {
		if got[key].Key != key || calls[key] != 1 {
			t.Fatalf("key %s result %+v calls %d", key, got[key], calls[key])
		}
	}
	if _, ok := got[missing]; ok || calls[missing] != 1 {
		t.Fatal("missing parent not denied")
	}
}
