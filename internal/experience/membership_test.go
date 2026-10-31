package experience

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

func TestMembershipChecksViewerAndInheritedPolicyBeforeLookup(t *testing.T) {
	ctx := context.Background()
	show := core.Title{Key: core.ShowKey(7), Type: core.TypeShow, ContentRating: "PG"}
	episode := core.Title{Key: core.EpisodeKey(7, 1, 1), Type: core.TypeEpisode, ShowKey: show.Key, ContentRating: "PG"}
	allowed := true
	active := true
	revokeDuringRead := false
	hideDuringRead := false
	calls := 0
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := &Service{d: ServiceDeps{Active: func(_ context.Context, id int64) (*core.User, error) {
		if id != 1 || !active {
			return nil, core.ErrUnauthorized
		}
		return &core.User{ID: id, MaxContentRating: "PG"}, nil
	}, Get: func(_ context.Context, _ *core.User, k core.RatingKey) (*core.Title, error) {
		if k == episode.Key {
			return &episode, nil
		}
		return nil, core.ErrNotFound
	}, Visibility: Visibility{SectionFor: func(core.MediaType) (int, bool) { return 2, true }, GetTitle: func(_ context.Context, k core.RatingKey) (*core.Title, error) {
		if k == show.Key {
			return &show, nil
		}
		return nil, core.ErrNotFound
	}}, FavoriteMembership: func(context.Context, *core.User, core.RatingKey) (frontend.Membership, error) {
		calls++
		if revokeDuringRead {
			active = false
		}
		if hideDuringRead {
			show.ContentRating = "R"
		}
		return frontend.Membership{Present: allowed, AddedAt: &at}, nil
	}, WatchlistMembership: func(context.Context, *core.User, core.RatingKey) (frontend.Membership, error) {
		calls++
		return frontend.Membership{Present: true, AddedAt: &at}, nil
	}}}
	u := &core.User{ID: 1}
	d := core.Device{ID: "tv"}
	f, err := s.FavoriteItem(ctx, u, d, episode.Key)
	if err != nil || !f.Present || f.AddedAt == nil || !f.AddedAt.Equal(at) {
		t.Fatal(f, err)
	}
	w, err := s.WatchlistItem(ctx, u, d, episode.Key)
	if err != nil || !w.Present || w.AddedAt == nil {
		t.Fatal(w, err)
	}
	s.d.WatchlistMembership = nil
	if _, err = s.WatchlistItem(ctx, u, d, episode.Key); !errors.Is(err, core.ErrNotAvailable) {
		t.Fatal("unavailable store reported absence", err)
	}
	allowed = false
	f, err = s.FavoriteItem(ctx, u, d, episode.Key)
	if err != nil || f.Present || f.AddedAt != nil {
		t.Fatal("absent", f, err)
	}
	before := calls
	show.ContentRating = "R"
	if _, err = s.FavoriteItem(ctx, u, d, episode.Key); !errors.Is(err, core.ErrNotFound) || calls != before {
		t.Fatal("hidden title queried", err)
	}
	if _, err = s.WatchlistItem(ctx, &core.User{ID: 2}, d, episode.Key); !errors.Is(err, core.ErrNotFound) || calls != before {
		t.Fatal("inactive viewer queried", err)
	}
	show.ContentRating = "PG"
	hideDuringRead = true
	if _, err = s.FavoriteItem(ctx, u, d, episode.Key); !errors.Is(err, core.ErrNotFound) {
		t.Fatal("policy change during read exposed membership", err)
	}
	hideDuringRead = false
	show.ContentRating = "PG"
	revokeDuringRead = true
	if _, err = s.FavoriteItem(ctx, u, d, episode.Key); !errors.Is(err, core.ErrNotFound) {
		t.Fatal("viewer revocation during read exposed membership", err)
	}
}
