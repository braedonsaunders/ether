package app

import (
	"context"
	"errors"
	"strconv"
	"strings"
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

func TestFavoritePolicyMatchesIndependentLegacy(t *testing.T) {
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

	compareAllowed := func(subject experience.FavoriteSubject, sources experience.FavoriteSources) {
		t.Helper()
		got := experience.FavoriteAllowed(ctx, viewer, subject, vis, sources)
		want := legacyFavoriteAllowed(ctx, viewer, subject, vis, sources)
		if got != want {
			t.Fatalf("%+v got %v legacy %v", subject, got, want)
		}
	}

	compareAllowed(experience.FavoriteSubject{Kind: experience.FavoriteKindTitle, ID: movieKey.String()}, experience.FavoriteSources{
		Title: func(context.Context, core.RatingKey) (*core.Title, error) { return &movie, nil },
	})
	compareAllowed(experience.FavoriteSubject{Kind: experience.FavoriteKindTitle, ID: movieKey.String()}, experience.FavoriteSources{
		Title: func(context.Context, core.RatingKey) (*core.Title, error) { return nil, dbErr },
	})
	compareAllowed(experience.FavoriteSubject{Kind: experience.FavoriteKindTitle, ID: movieKey.String()}, experience.FavoriteSources{
		Title: func(context.Context, core.RatingKey) (*core.Title, error) {
			return &core.Title{Key: core.MovieKey(1), Type: core.TypeMovie}, nil
		},
	})
	compareAllowed(experience.FavoriteSubject{Kind: experience.FavoriteKindTitle, ID: "bad"}, experience.FavoriteSources{
		Title: func(context.Context, core.RatingKey) (*core.Title, error) {
			t.Fatal("lookup")
			return nil, nil
		},
	})
	compareAllowed(experience.FavoriteSubject{Kind: experience.FavoriteKindCollection, ID: collectionKey.String()}, experience.FavoriteSources{
		Collection: func(context.Context, *core.User, core.RatingKey) (*core.Collection, error) {
			return collection, nil
		},
	})
	hidden := *collection
	viewer.AllowedSections = []int{1}
	compareAllowed(experience.FavoriteSubject{Kind: experience.FavoriteKindCollection, ID: collectionKey.String()}, experience.FavoriteSources{
		Collection: func(context.Context, *core.User, core.RatingKey) (*core.Collection, error) { return &hidden, nil },
	})
	viewer.AllowedSections = nil
	compareAllowed(experience.FavoriteSubject{Kind: experience.FavoriteKindCollection, ID: collectionKey.String()}, experience.FavoriteSources{
		Collection: func(context.Context, *core.User, core.RatingKey) (*core.Collection, error) { return nil, dbErr },
	})
	compareAllowed(experience.FavoriteSubject{Kind: experience.FavoriteKindChannel, ID: "news"}, experience.FavoriteSources{
		ChannelIDs: func(context.Context, *core.User) ([]string, error) { return []string{"sports", "news"}, nil },
	})
	compareAllowed(experience.FavoriteSubject{Kind: experience.FavoriteKindChannel, ID: "secret"}, experience.FavoriteSources{
		ChannelIDs: func(context.Context, *core.User) ([]string, error) { return []string{"sports"}, nil },
	})
	compareAllowed(experience.FavoriteSubject{Kind: experience.FavoriteKindChannel, ID: "news"}, experience.FavoriteSources{
		ChannelIDs: func(context.Context, *core.User) ([]string, error) { return nil, dbErr },
	})
	compareAllowed(experience.FavoriteSubject{Kind: experience.FavoriteKindPerson, ID: "tmdb:01"}, experience.FavoriteSources{
		PersonPage: func(context.Context, int64, int, int) ([]core.Title, error) {
			t.Fatal("browse")
			return nil, nil
		},
	})
	compareAllowed(experience.FavoriteSubject{Kind: experience.FavoriteKindPerson, ID: "tmdb:15"}, experience.FavoriteSources{
		PersonPage: func(_ context.Context, person int64, offset, limit int) ([]core.Title, error) {
			if person != 15 || offset != 0 || limit != 100 {
				t.Fatalf("query %d %d %d", person, offset, limit)
			}
			return []core.Title{episode}, nil
		},
	})
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
	})
	if seen[0] != 2 || seen[100] != 2 || len(seen) != 2 {
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
	compareCached := func(user *core.User, keys []core.RatingKey, read func(context.Context, []core.RatingKey) (map[core.RatingKey]core.Title, error)) {
		t.Helper()
		got, gotErr := experience.FavoriteTitlesCached(ctx, user, keys, vis, read)
		want, wantErr := legacyFavoriteTitlesCached(ctx, user, keys, vis, read)
		if got != want || !errors.Is(gotErr, wantErr) && gotErr != wantErr {
			t.Fatalf("keys %v got %v %v legacy %v %v", keys, got, gotErr, want, wantErr)
		}
	}
	compareCached(viewer, []core.RatingKey{episodeKey}, cached)
	delete(lookup, show.Key)
	compareCached(viewer, []core.RatingKey{episodeKey}, cached)
	lookup[show.Key] = show
	show.ContentRating = "TV-MA"
	lookup[show.Key] = show
	compareCached(viewer, []core.RatingKey{episodeKey}, cached)
	compareCached(nil, []core.RatingKey{movieKey}, cached)
	compareCached(viewer, []core.RatingKey{movieKey}, nil)
	compareCached(viewer, []core.RatingKey{movieKey}, func(context.Context, []core.RatingKey) (map[core.RatingKey]core.Title, error) {
		return nil, dbErr
	})
	compareCached(viewer, []core.RatingKey{episodeKey}, func(_ context.Context, keys []core.RatingKey) (map[core.RatingKey]core.Title, error) {
		if len(keys) == 1 && keys[0] == episodeKey {
			return map[core.RatingKey]core.Title{episodeKey: episode}, nil
		}
		return nil, dbErr
	})
	compareCached(viewer, []core.RatingKey{core.MovieKey(99)}, cached)
}

// legacyFavoriteAllowed is the pre-extraction decision, transcribed against the
// same ports. It is the independent control for experience.FavoriteAllowed.
func legacyFavoriteAllowed(ctx context.Context, u *core.User, subject experience.FavoriteSubject, visibility experience.Visibility, sources experience.FavoriteSources) bool {
	if u == nil {
		return false
	}
	switch subject.Kind {
	case experience.FavoriteKindTitle:
		if sources.Title == nil {
			return false
		}
		key, err := core.ParseRatingKey(subject.ID)
		if err != nil {
			return false
		}
		title, err := sources.Title(ctx, key)
		return err == nil && title != nil && title.Key == key && visibility.Visible(ctx, u, title)
	case experience.FavoriteKindCollection:
		if sources.Collection == nil {
			return false
		}
		key, err := core.ParseRatingKey(subject.ID)
		if err != nil {
			return false
		}
		collection, err := sources.Collection(ctx, u, key)
		return err == nil && collection != nil && visibility.CollectionAllowed(u, collection)
	case experience.FavoriteKindChannel:
		if sources.ChannelIDs == nil {
			return false
		}
		ids, err := sources.ChannelIDs(ctx, u)
		if err != nil {
			return false
		}
		for _, id := range ids {
			if id == subject.ID {
				return true
			}
		}
		return false
	case experience.FavoriteKindPerson:
		if sources.PersonPage == nil {
			return false
		}
		raw, ok := strings.CutPrefix(subject.ID, "tmdb:")
		if !ok {
			return false
		}
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 || strconv.FormatInt(id, 10) != raw {
			return false
		}
		for offset := 0; offset < 1000; offset += 100 {
			items, err := sources.PersonPage(ctx, id, offset, 100)
			if err != nil {
				return false
			}
			for i := range items {
				if visibility.Visible(ctx, u, &items[i]) {
					return true
				}
			}
			if len(items) < 100 {
				return false
			}
		}
		return false
	}
	return false
}

// legacyFavoriteTitlesCached is the pre-extraction cache decision. It reads
// only the injected cache function and fails closed on that function's error.
func legacyFavoriteTitlesCached(ctx context.Context, u *core.User, keys []core.RatingKey, visibility experience.Visibility, cached func(context.Context, []core.RatingKey) (map[core.RatingKey]core.Title, error)) (bool, error) {
	if u == nil || cached == nil {
		return false, nil
	}
	titles, err := cached(ctx, keys)
	if err != nil {
		return false, err
	}
	var parents []core.RatingKey
	for _, key := range keys {
		title, ok := titles[key]
		if !ok {
			return false, nil
		}
		if title.ShowKey != 0 {
			parents = append(parents, title.ShowKey)
		}
	}
	shows, err := cached(ctx, parents)
	if err != nil {
		return false, err
	}
	visibility.GetTitle = func(_ context.Context, key core.RatingKey) (*core.Title, error) {
		show, ok := shows[key]
		if !ok {
			return nil, core.ErrNotFound
		}
		return &show, nil
	}
	for _, key := range keys {
		title := titles[key]
		if !visibility.Visible(ctx, u, &title) {
			return false, nil
		}
	}
	return true, nil
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
