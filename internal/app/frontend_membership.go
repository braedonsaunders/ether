package app

import (
	"context"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/favorites"
	"github.com/braedonsaunders/ether/internal/frontend"
)

func (a *App) favoriteMembership(ctx context.Context, viewer *core.User, key core.RatingKey) (frontend.Membership, error) {
	if a.favorites == nil {
		return frontend.Membership{}, core.ErrNotAvailable
	}
	present, at, err := a.favorites.Membership(ctx, viewer, favorites.Subject{Kind: favorites.KindTitle, ID: key.String()})
	return frontend.Membership{Present: present, AddedAt: at}, err
}

func (a *App) watchlistMembership(ctx context.Context, viewer *core.User, key core.RatingKey) (frontend.Membership, error) {
	if a.watchlist == nil {
		return frontend.Membership{}, core.ErrNotAvailable
	}
	present, at, err := a.watchlist.Membership(ctx, viewer.ID, key)
	return frontend.Membership{Present: present, AddedAt: at}, err
}
