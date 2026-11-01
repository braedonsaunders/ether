package app

import (
	"context"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/favorites"
)

// favoriteAllowed binds current catalogue, collection, and channel dependencies.
// Active-viewer validation stays with the caller. The decision itself is
// experience.FavoriteAllowed.
func (a *App) favoriteAllowed(ctx context.Context, u *core.User, subject favorites.Subject, visibility experience.Visibility) bool {
	var sources experience.FavoriteSources
	if a.catalog != nil {
		sources.Title = a.catalog.Get
		sources.PersonPage = a.favoritePersonPage
	}
	if a.collections != nil {
		sources.Collection = a.collections.Get
	}
	if a.livetv != nil && subject.Kind == favorites.KindChannel {
		sources.ChannelAllowed = func(ctx context.Context, viewer *core.User, id string) (bool, error) {
			return a.livetv.ChannelPermitted(ctx, viewer, id)
		}
	}
	return experience.FavoriteAllowed(ctx, u, experience.FavoriteSubject{Kind: experience.FavoriteKind(subject.Kind), ID: subject.ID}, visibility, sources)
}

// favoriteAllowedStored is the row-receipt admission check. It reads the
// catalogue mirror and does not refresh a season or show from a provider,
// so issuing home receipts cannot wait on the network.
func (a *App) favoriteAllowedStored(ctx context.Context, u *core.User, subject favorites.Subject, visibility experience.Visibility) bool {
	var sources experience.FavoriteSources
	if a.catalog != nil {
		sources.Title = a.catalog.CachedGet
		sources.PersonPage = a.favoritePersonPage
		visibility.GetTitle = a.catalog.CachedGet
	}
	if a.collections != nil {
		sources.Collection = a.collections.Get
	}
	if a.livetv != nil && subject.Kind == favorites.KindChannel {
		sources.ChannelAllowed = func(ctx context.Context, viewer *core.User, id string) (bool, error) {
			return a.livetv.ChannelPermitted(ctx, viewer, id)
		}
	}
	return experience.FavoriteAllowed(ctx, u, experience.FavoriteSubject{Kind: experience.FavoriteKind(subject.Kind), ID: subject.ID}, visibility, sources)
}

func (a *App) favoritePersonPage(ctx context.Context, person int64, offset, limit int) ([]core.Title, error) {
	page, err := a.catalog.Browse(ctx, core.BrowseQuery{Filter: core.Filter{Person: person}, Offset: offset, Limit: limit})
	if err != nil {
		return nil, err
	}
	return page.Items, nil
}

// favoriteTitlesCached rechecks current policy through the local catalogue
// cache only. It never binds Get or Browse, so this path cannot refresh metadata.
func (a *App) favoriteTitlesCached(ctx context.Context, u *core.User, keys []core.RatingKey, visibility experience.Visibility) (bool, error) {
	if a.catalog == nil {
		return false, nil
	}
	return experience.FavoriteTitlesCached(ctx, u, keys, visibility, a.catalog.CachedGetMany)
}
