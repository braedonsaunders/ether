package experience

import (
	"context"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

func (s *Service) membership(ctx context.Context, viewer *core.Viewer, key core.RatingKey, source func(context.Context, *core.User, core.RatingKey) (frontend.Membership, error)) (frontend.Membership, error) {
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return frontend.Membership{}, err
	}
	if _, err = s.item(ctx, v, key); err != nil {
		return frontend.Membership{}, core.ErrNotFound
	}
	if source == nil {
		return frontend.Membership{}, core.ErrNotAvailable
	}
	result, err := source(ctx, v, key)
	if err != nil {
		return frontend.Membership{}, err
	}
	// Membership is viewer-private: do not return a stored fact if policy or
	// membership was revoked while the store lookup was in flight.
	current, err := s.viewer(ctx, viewer)
	if err != nil {
		return frontend.Membership{}, err
	}
	if _, err = s.item(ctx, current, key); err != nil {
		return frontend.Membership{}, core.ErrNotFound
	}
	if !result.Present {
		result.AddedAt = nil
	}
	return result, nil
}

func (s *Service) FavoriteItem(ctx context.Context, viewer *core.Viewer, _ core.Device, key core.RatingKey) (frontend.Membership, error) {
	return s.membership(ctx, viewer, key, s.d.FavoriteMembership)
}

func (s *Service) WatchlistItem(ctx context.Context, viewer *core.Viewer, _ core.Device, key core.RatingKey) (frontend.Membership, error) {
	return s.membership(ctx, viewer, key, s.d.WatchlistMembership)
}

var _ frontend.MembershipReader = (*Service)(nil)
