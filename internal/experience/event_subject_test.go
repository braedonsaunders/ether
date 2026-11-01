package experience

import (
	"context"
	"errors"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

func TestResolveEventSubjectRechecksVisibilityAndCurrentState(t *testing.T) {
	ctx := context.Background()
	v := &core.User{ID: 1}
	title := core.Title{Key: core.MovieKey(1), Type: core.TypeMovie}
	allowed := true
	favorite := true
	s := &Service{d: ServiceDeps{Active: func(_ context.Context, id int64) (*core.User, error) {
		if id != 1 {
			return nil, core.ErrUnauthorized
		}
		return v, nil
	}, Get: func(_ context.Context, _ *core.User, k core.RatingKey) (*core.Title, error) {
		if k == title.Key {
			return &title, nil
		}
		return nil, core.ErrNotFound
	}, Visibility: Visibility{SectionFor: func(core.MediaType) (int, bool) { return 1, true }}, State: serviceState{}, IsFavorite: func(context.Context, *core.User, core.RatingKey) (bool, error) { return favorite, nil }, CanFavoriteEntity: func(context.Context, *core.User, frontend.FavoriteRef) bool { return allowed }, EntityFavoritesSource: func(_ context.Context, _ *core.User, refs []frontend.FavoriteRef) (map[frontend.FavoriteRef]bool, error) {
		return map[frontend.FavoriteRef]bool{refs[0]: favorite}, nil
	}}}
	for _, ref := range []frontend.FavoriteRef{{Kind: frontend.FavoriteTitle, ID: title.Key.String()}, {Kind: frontend.FavoritePerson, ID: "tmdb:42"}, {Kind: frontend.FavoriteCollection, ID: core.CollectionKey(1).String()}, {Kind: frontend.FavoriteChannel, ID: "channel-1"}} {
		got, err := s.ResolveEventSubject(ctx, v, core.Device{}, ref)
		if err != nil || !got.Favorite || got.Ref != ref {
			t.Fatalf("%v: %+v %v", ref, got, err)
		}
		if ref.Kind == frontend.FavoritePerson && got.PersonFacetID != facetID("person", "42") {
			t.Fatal("wrong person facet", got)
		}
		favorite = false
		got, err = s.ResolveEventSubject(ctx, v, core.Device{}, ref)
		if err != nil || got.Favorite {
			t.Fatalf("unset %v: %+v %v", ref, got, err)
		}
		favorite = true
	}
	allowed = false
	if _, err := s.ResolveEventSubject(ctx, v, core.Device{}, frontend.FavoriteRef{Kind: frontend.FavoritePerson, ID: "tmdb:42"}); !errors.Is(err, core.ErrNotFound) {
		t.Fatal("revoked policy", err)
	}
	title.Adult = true
	if _, err := s.ResolveEventSubject(ctx, v, core.Device{}, frontend.FavoriteRef{Kind: frontend.FavoriteTitle, ID: title.Key.String()}); !errors.Is(err, core.ErrNotFound) {
		t.Fatal("hidden title", err)
	}
	if _, err := s.ResolveEventSubject(ctx, &core.User{ID: 2}, core.Device{}, frontend.FavoriteRef{Kind: frontend.FavoriteChannel, ID: "channel-1"}); !errors.Is(err, core.ErrNotFound) {
		t.Fatal("inactive viewer", err)
	}
	for _, ref := range []frontend.FavoriteRef{{Kind: frontend.FavoritePerson, ID: "tmdb:042"}, {Kind: frontend.FavoriteCollection, ID: title.Key.String()}, {Kind: frontend.FavoriteChannel, ID: ""}} {
		if _, err := s.ResolveEventSubject(ctx, v, core.Device{}, ref); !errors.Is(err, core.ErrNotFound) {
			t.Fatalf("invalid %v %v", ref, err)
		}
	}
}
