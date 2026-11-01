package experience

import (
	"context"
	"errors"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestArtworkAuthorizationPropagatesCacheFailure(t *testing.T) {
	failure := errors.New("catalogue unavailable")
	s := &Service{d: ServiceDeps{
		Active:    func(_ context.Context, id int64) (*core.User, error) { return &core.User{ID: id}, nil },
		CachedGet: func(context.Context, core.RatingKey) (*core.Title, error) { return nil, failure },
		Get: func(context.Context, *core.User, core.RatingKey) (*core.Title, error) {
			t.Fatal("network-capable lookup used")
			return nil, nil
		},
	}}
	if _, err := s.AuthorizeArtwork(context.Background(), &core.User{ID: 1}, core.Device{}, core.MovieKey(1)); !errors.Is(err, failure) {
		t.Fatalf("cache error hidden: %v", err)
	}
}

func TestVirtualSeasonArtworkInheritsShowPolicy(t *testing.T) {
	show := &core.Title{Key: core.ShowKey(10), Type: core.TypeShow}
	key := core.SeasonKey(10, 9000)
	season := &core.Title{Key: key, Type: core.TypeSeason, ShowKey: show.Key}
	s := &Service{d: ServiceDeps{
		Active: func(_ context.Context, id int64) (*core.User, error) { return &core.User{ID: id}, nil },
		CachedGet: func(_ context.Context, k core.RatingKey) (*core.Title, error) {
			if k == show.Key {
				return show, nil
			}
			return nil, core.ErrNotFound
		},
		CachedVirtualSeason: func(_ context.Context, viewer *core.User, k core.RatingKey) (*core.Title, error) {
			if viewer.ID != 7 || k != key {
				return nil, core.ErrNotFound
			}
			return season, nil
		},
		Visibility: Visibility{SectionFor: func(core.MediaType) (int, bool) { return 2, true }},
	}}
	ctx := context.Background()
	viewer := &core.User{ID: 7}
	if _, err := s.AuthorizeArtwork(ctx, viewer, core.Device{}, key); err != nil {
		t.Fatal(err)
	}
	show.Adult = true
	if _, err := s.AuthorizeArtwork(ctx, viewer, core.Device{}, key); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("hidden parent allowed: %v", err)
	}
}
