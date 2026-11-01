package experience

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestCanonicalTMDBPerson(t *testing.T) {
	if id, ok := CanonicalTMDBPerson("tmdb:42"); !ok || id != 42 {
		t.Fatalf("canonical id: %d %v", id, ok)
	}
	max := strconv.FormatInt(1<<63-1, 10)
	if id, ok := CanonicalTMDBPerson("tmdb:" + max); !ok || id != 1<<63-1 {
		t.Fatalf("max int64: %d %v", id, ok)
	}
	for _, id := range []string{"", "42", "TMDB:1", "tmdb:", "tmdb:0", "tmdb:-1", "tmdb:01", "tmdb:+1", "tmdb:1.2", "tmdb:1a", "tmdb: 1", "tmdb:9223372036854775808"} {
		if _, ok := CanonicalTMDBPerson(id); ok {
			t.Fatalf("accepted %q", id)
		}
	}
}

func TestFavoriteAllowedNilAndErrors(t *testing.T) {
	ctx := context.Background()
	viewer := &core.User{ID: 1}
	vis := Visibility{}
	key := core.MovieKey(3)
	subject := FavoriteSubject{Kind: FavoriteKindTitle, ID: key.String()}
	boom := errors.New("database down")
	sources := FavoriteSources{
		Title: func(context.Context, core.RatingKey) (*core.Title, error) {
			t.Fatal("title lookup")
			return nil, nil
		},
		Collection: func(context.Context, *core.User, core.RatingKey) (*core.Collection, error) {
			t.Fatal("collection lookup")
			return nil, nil
		},
		ChannelIDs: func(context.Context, *core.User) ([]string, error) {
			t.Fatal("channel lookup")
			return nil, nil
		},
		PersonPage: func(context.Context, int64, int, int) ([]core.Title, error) {
			t.Fatal("person browse")
			return nil, nil
		},
	}
	if FavoriteAllowed(ctx, nil, subject, vis, sources) {
		t.Fatal("nil viewer allowed")
	}
	if FavoriteAllowed(ctx, viewer, subject, vis, FavoriteSources{}) {
		t.Fatal("nil title source allowed")
	}
	if FavoriteAllowed(ctx, viewer, FavoriteSubject{Kind: FavoriteKindCollection, ID: core.CollectionKey(1).String()}, vis, FavoriteSources{}) {
		t.Fatal("nil collection source allowed")
	}
	if FavoriteAllowed(ctx, viewer, FavoriteSubject{Kind: FavoriteKindChannel, ID: "news"}, vis, FavoriteSources{}) {
		t.Fatal("nil channel source allowed")
	}
	if FavoriteAllowed(ctx, viewer, FavoriteSubject{Kind: FavoriteKindPerson, ID: "tmdb:1"}, vis, FavoriteSources{}) {
		t.Fatal("nil person source allowed")
	}
	if FavoriteAllowed(ctx, viewer, FavoriteSubject{Kind: FavoriteKind("other"), ID: "x"}, vis, sources) {
		t.Fatal("unknown kind allowed")
	}
	if FavoriteAllowed(ctx, viewer, FavoriteSubject{Kind: FavoriteKindTitle, ID: "not-a-key"}, vis, sources) {
		t.Fatal("malformed title allowed")
	}
	if FavoriteAllowed(ctx, viewer, FavoriteSubject{Kind: FavoriteKindCollection, ID: "not-a-key"}, vis, sources) {
		t.Fatal("malformed collection allowed")
	}
	if FavoriteAllowed(ctx, viewer, FavoriteSubject{Kind: FavoriteKindPerson, ID: "tmdb:01"}, vis, sources) {
		t.Fatal("non-canonical person browsed")
	}
	sources.Title = func(context.Context, core.RatingKey) (*core.Title, error) { return nil, boom }
	if FavoriteAllowed(ctx, viewer, subject, vis, sources) {
		t.Fatal("title database error allowed")
	}
	sources.Title = func(context.Context, core.RatingKey) (*core.Title, error) { return nil, nil }
	if FavoriteAllowed(ctx, viewer, subject, vis, sources) {
		t.Fatal("nil title allowed")
	}
	sources.Title = func(context.Context, core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: core.MovieKey(9), Type: core.TypeMovie}, nil
	}
	if FavoriteAllowed(ctx, viewer, subject, vis, sources) {
		t.Fatal("mismatched title key allowed")
	}
	sources.Collection = func(context.Context, *core.User, core.RatingKey) (*core.Collection, error) {
		return nil, boom
	}
	if FavoriteAllowed(ctx, viewer, FavoriteSubject{Kind: FavoriteKindCollection, ID: core.CollectionKey(4).String()}, vis, sources) {
		t.Fatal("collection database error allowed")
	}
	sources.Collection = func(context.Context, *core.User, core.RatingKey) (*core.Collection, error) {
		return nil, nil
	}
	if FavoriteAllowed(ctx, viewer, FavoriteSubject{Kind: FavoriteKindCollection, ID: core.CollectionKey(4).String()}, vis, sources) {
		t.Fatal("nil collection allowed")
	}
	sources.ChannelIDs = func(context.Context, *core.User) ([]string, error) { return nil, boom }
	if FavoriteAllowed(ctx, viewer, FavoriteSubject{Kind: FavoriteKindChannel, ID: "news"}, vis, sources) {
		t.Fatal("channel database error allowed")
	}
	pages := 0
	sources.PersonPage = func(context.Context, int64, int, int) ([]core.Title, error) {
		pages++
		if pages == 1 {
			items := make([]core.Title, favoritePersonPageSize)
			for i := range items {
				items[i].Adult = true
			}
			return items, nil
		}
		return nil, boom
	}
	if FavoriteAllowed(ctx, viewer, FavoriteSubject{Kind: FavoriteKindPerson, ID: "tmdb:7"}, vis, sources) || pages != 2 {
		t.Fatalf("person database error pages=%d", pages)
	}
}

func TestFavoriteAllowedPolicy(t *testing.T) {
	ctx := context.Background()
	viewer := &core.User{ID: 1, MaxContentRating: "PG"}
	vis := Visibility{SectionFor: func(core.MediaType) (int, bool) { return 2, true }}
	movie := core.MovieKey(5)
	sources := FavoriteSources{
		Title: func(_ context.Context, key core.RatingKey) (*core.Title, error) {
			if key != movie {
				t.Fatalf("title key %s", key)
			}
			return &core.Title{Key: movie, Type: core.TypeMovie, ContentRating: "PG"}, nil
		},
	}
	channels := 0
	sources.ChannelAllowed = func(context.Context, *core.User, string) (bool, error) {
		channels++
		return false, nil
	}
	if !FavoriteAllowed(ctx, viewer, FavoriteSubject{Kind: FavoriteKindTitle, ID: movie.String()}, vis, sources) {
		t.Fatal("visible title denied")
	}
	if channels != 0 {
		t.Fatal("title decision read live TV")
	}
	sources.ChannelAllowed = nil
	sources.Title = func(context.Context, core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: movie, Type: core.TypeMovie, Adult: true}, nil
	}
	if FavoriteAllowed(ctx, viewer, FavoriteSubject{Kind: FavoriteKindTitle, ID: movie.String()}, vis, sources) {
		t.Fatal("adult title allowed")
	}
	viewer.AllowedSections = []int{1}
	sources.Title = func(context.Context, core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: movie, Type: core.TypeMovie, ContentRating: "G"}, nil
	}
	if FavoriteAllowed(ctx, viewer, FavoriteSubject{Kind: FavoriteKindTitle, ID: movie.String()}, vis, sources) {
		t.Fatal("revoked section allowed")
	}
	viewer.AllowedSections = nil
	collectionKey := core.CollectionKey(8)
	sources.Collection = func(_ context.Context, got *core.User, key core.RatingKey) (*core.Collection, error) {
		if got != viewer || key != collectionKey {
			t.Fatalf("collection viewer/key %v %s", got, key)
		}
		return &core.Collection{Key: collectionKey, Type: core.TypeMovie}, nil
	}
	if !FavoriteAllowed(ctx, viewer, FavoriteSubject{Kind: FavoriteKindCollection, ID: collectionKey.String()}, vis, sources) {
		t.Fatal("visible collection denied")
	}
	viewer.AllowedSections = []int{1}
	if FavoriteAllowed(ctx, viewer, FavoriteSubject{Kind: FavoriteKindCollection, ID: collectionKey.String()}, vis, sources) {
		t.Fatal("hidden collection allowed")
	}
	viewer.AllowedSections = nil
	sources.ChannelAllowed = func(_ context.Context, _ *core.User, id string) (bool, error) {
		if id != "news" && id != "secret" {
			t.Fatalf("channel %s", id)
		}
		return id == "news", nil
	}
	sources.ChannelIDs = func(context.Context, *core.User) ([]string, error) {
		t.Fatal("listed every channel")
		return nil, nil
	}
	if !FavoriteAllowed(ctx, viewer, FavoriteSubject{Kind: FavoriteKindChannel, ID: "news"}, vis, sources) {
		t.Fatal("one channel denied")
	}
	if FavoriteAllowed(ctx, viewer, FavoriteSubject{Kind: FavoriteKindChannel, ID: "secret"}, vis, sources) {
		t.Fatal("other channel allowed")
	}
	sources.ChannelAllowed = nil
	sources.ChannelIDs = func(context.Context, *core.User) ([]string, error) {
		return []string{"sports", "news"}, nil
	}
	if !FavoriteAllowed(ctx, viewer, FavoriteSubject{Kind: FavoriteKindChannel, ID: "news"}, vis, sources) {
		t.Fatal("listed channel denied")
	}
	if FavoriteAllowed(ctx, viewer, FavoriteSubject{Kind: FavoriteKindChannel, ID: "secret"}, vis, sources) {
		t.Fatal("unlisted channel allowed")
	}

	show := core.ShowKey(4)
	episode := core.EpisodeKey(4, 1, 1)
	var looked []core.RatingKey
	vis.GetTitle = func(_ context.Context, key core.RatingKey) (*core.Title, error) {
		looked = append(looked, key)
		if key != show {
			return nil, core.ErrNotFound
		}
		return &core.Title{Key: show, Type: core.TypeShow, ContentRating: "TV-MA"}, nil
	}
	sources.PersonPage = func(_ context.Context, person int64, offset, limit int) ([]core.Title, error) {
		if person != 15 || offset != 0 || limit != favoritePersonPageSize {
			t.Fatalf("person query %d %d %d", person, offset, limit)
		}
		return []core.Title{{Key: episode, Type: core.TypeEpisode, ShowKey: show, ContentRating: "TV-G"}}, nil
	}
	if FavoriteAllowed(ctx, viewer, FavoriteSubject{Kind: FavoriteKindPerson, ID: "tmdb:15"}, vis, sources) || len(looked) != 1 || looked[0] != show {
		t.Fatalf("hidden parent allowed, lookups %+v", looked)
	}
	vis.GetTitle = func(context.Context, core.RatingKey) (*core.Title, error) {
		return nil, core.ErrNotFound
	}
	if FavoriteAllowed(ctx, viewer, FavoriteSubject{Kind: FavoriteKindPerson, ID: "tmdb:15"}, vis, sources) {
		t.Fatal("missing parent allowed")
	}
	vis.GetTitle = func(context.Context, core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: show, Type: core.TypeShow, ContentRating: "TV-G"}, nil
	}
	if !FavoriteAllowed(ctx, viewer, FavoriteSubject{Kind: FavoriteKindPerson, ID: "tmdb:15"}, vis, sources) {
		t.Fatal("visible credit denied")
	}
}

func TestFavoritePersonBrowseBound(t *testing.T) {
	ctx := context.Background()
	viewer := &core.User{ID: 1}
	vis := Visibility{}
	var offsets []int
	sources := FavoriteSources{PersonPage: func(_ context.Context, person int64, offset, limit int) ([]core.Title, error) {
		if person != 42 || limit != 100 {
			t.Fatalf("query %d offset %d limit %d", person, offset, limit)
		}
		if offset >= 1000 {
			t.Fatal("scan passed 1000")
		}
		offsets = append(offsets, offset)
		items := make([]core.Title, 100)
		for i := range items {
			items[i] = core.Title{Key: core.MovieKey(int64(offset + i + 1)), Type: core.TypeMovie, Adult: true}
		}
		if offset == 900 {
			items[99].Adult = false
		}
		return items, nil
	}}
	if !FavoriteAllowed(ctx, viewer, FavoriteSubject{Kind: FavoriteKindPerson, ID: "tmdb:42"}, vis, sources) {
		t.Fatal("last examined credit denied")
	}
	if len(offsets) != 10 || offsets[0] != 0 || offsets[9] != 900 {
		t.Fatalf("offsets %+v", offsets)
	}

	offsets = nil
	sources.PersonPage = func(_ context.Context, person int64, offset, limit int) ([]core.Title, error) {
		offsets = append(offsets, offset)
		if offset >= 1000 {
			t.Fatal("scan passed 1000")
		}
		items := make([]core.Title, 100)
		for i := range items {
			items[i] = core.Title{Key: core.MovieKey(int64(offset + i + 1)), Type: core.TypeMovie, Adult: true}
		}
		return items, nil
	}
	if FavoriteAllowed(ctx, viewer, FavoriteSubject{Kind: FavoriteKindPerson, ID: "tmdb:42"}, vis, sources) {
		t.Fatal("credits beyond the bound were required")
	}
	if len(offsets) != 10 || offsets[9] != 900 {
		t.Fatalf("full scan offsets %+v", offsets)
	}

	calls := 0
	sources.PersonPage = func(context.Context, int64, int, int) ([]core.Title, error) {
		calls++
		items := make([]core.Title, 99)
		for i := range items {
			items[i].Adult = true
		}
		return items, nil
	}
	if FavoriteAllowed(ctx, viewer, FavoriteSubject{Kind: FavoriteKindPerson, ID: "tmdb:42"}, vis, sources) || calls != 1 {
		t.Fatalf("short page calls=%d", calls)
	}
}

func TestFavoriteTitlesCachedFailClosed(t *testing.T) {
	ctx := context.Background()
	viewer := &core.User{ID: 1, MaxContentRating: "PG"}
	show := core.Title{Key: core.ShowKey(4), Type: core.TypeShow, ContentRating: "TV-MA"}
	episode := core.Title{Key: core.EpisodeKey(4, 1, 1), Type: core.TypeEpisode, ShowKey: show.Key, ContentRating: "TV-G"}
	movie := core.Title{Key: core.MovieKey(2), Type: core.TypeMovie, ContentRating: "PG"}
	lookup := map[core.RatingKey]core.Title{episode.Key: episode, show.Key: show, movie.Key: movie}
	calls := 0
	cached := func(_ context.Context, keys []core.RatingKey) (map[core.RatingKey]core.Title, error) {
		calls++
		out := map[core.RatingKey]core.Title{}
		for _, key := range keys {
			if title, ok := lookup[key]; ok {
				out[key] = title
			}
		}
		return out, nil
	}
	remote := 0
	vis := Visibility{
		SectionFor: func(core.MediaType) (int, bool) { return 2, true },
		GetTitle: func(context.Context, core.RatingKey) (*core.Title, error) {
			remote++
			t.Fatal("cached path used the remote title lookup")
			return nil, nil
		},
	}
	if ok, err := FavoriteTitlesCached(ctx, nil, []core.RatingKey{episode.Key}, vis, cached); err != nil || ok || calls != 0 {
		t.Fatalf("nil viewer %v %v calls %d", ok, err, calls)
	}
	if ok, err := FavoriteTitlesCached(ctx, viewer, []core.RatingKey{episode.Key}, vis, nil); err != nil || ok {
		t.Fatalf("nil cache %v %v", ok, err)
	}
	if ok, err := FavoriteTitlesCached(ctx, viewer, []core.RatingKey{episode.Key}, vis, cached); err != nil || ok || remote != 0 {
		t.Fatalf("hidden parent %v %v remote %d", ok, err, remote)
	}
	delete(lookup, show.Key)
	if ok, err := FavoriteTitlesCached(ctx, viewer, []core.RatingKey{episode.Key}, vis, cached); err != nil || ok {
		t.Fatalf("missing parent %v %v", ok, err)
	}
	show.ContentRating = "TV-G"
	lookup[show.Key] = show
	if ok, err := FavoriteTitlesCached(ctx, viewer, []core.RatingKey{episode.Key}, vis, cached); err != nil || !ok {
		t.Fatalf("visible episode %v %v", ok, err)
	}
	viewer.AllowedSections = []int{1}
	if ok, err := FavoriteTitlesCached(ctx, viewer, []core.RatingKey{episode.Key}, vis, cached); err != nil || ok {
		t.Fatalf("revoked section %v %v", ok, err)
	}
	viewer.AllowedSections = nil
	calls = 0
	if ok, err := FavoriteTitlesCached(ctx, viewer, []core.RatingKey{episode.Key, core.MovieKey(9)}, vis, cached); err != nil || ok || calls != 1 {
		t.Fatalf("missing title %v %v calls %d", ok, err, calls)
	}
	if ok, err := FavoriteTitlesCached(ctx, viewer, nil, vis, cached); err != nil || !ok {
		t.Fatalf("empty keys %v %v", ok, err)
	}
	dbErr := errors.New("select failed")
	calls = 0
	failFirst := func(context.Context, []core.RatingKey) (map[core.RatingKey]core.Title, error) {
		calls++
		return nil, dbErr
	}
	if ok, err := FavoriteTitlesCached(ctx, viewer, []core.RatingKey{movie.Key}, vis, failFirst); ok || !errors.Is(err, dbErr) || calls != 1 {
		t.Fatalf("first cache error %v %v calls %d", ok, err, calls)
	}
	calls = 0
	failParent := func(_ context.Context, keys []core.RatingKey) (map[core.RatingKey]core.Title, error) {
		calls++
		if calls == 1 {
			return map[core.RatingKey]core.Title{episode.Key: episode}, nil
		}
		return nil, dbErr
	}
	if ok, err := FavoriteTitlesCached(ctx, viewer, []core.RatingKey{episode.Key}, vis, failParent); ok || !errors.Is(err, dbErr) || calls != 2 {
		t.Fatalf("parent cache error %v %v calls %d", ok, err, calls)
	}
	if remote != 0 {
		t.Fatalf("remote lookups %d", remote)
	}
}
