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
		sources.PersonPage = func(ctx context.Context, person int64, offset, limit int) ([]core.Title, error) {
			page, err := a.catalog.Browse(ctx, core.BrowseQuery{Filter: core.Filter{Person: person}, Offset: offset, Limit: limit})
			if err != nil {
				return nil, err
			}
			return page.Items, nil
		}
	}
	if a.collections != nil {
		sources.Collection = a.collections.Get
	}
	if a.livetv != nil {
		sources.ChannelIDs = func(ctx context.Context, viewer *core.User) ([]string, error) {
			channels, err := a.livetv.AllowedChannels(ctx, viewer)
			if err != nil {
				return nil, err
			}
			ids := make([]string, len(channels))
			for i := range channels {
				ids[i] = channels[i].ID
			}
			return ids, nil
		}
	}
	return experience.FavoriteAllowed(ctx, u, experience.FavoriteSubject{Kind: experience.FavoriteKind(subject.Kind), ID: subject.ID}, visibility, sources)
}

// favoriteTitlesCached rechecks current policy through the local catalogue
// cache only. It never binds Get or Browse, so this path cannot refresh metadata.
func (a *App) favoriteTitlesCached(ctx context.Context, u *core.User, keys []core.RatingKey, visibility experience.Visibility) (bool, error) {
	if a.catalog == nil {
		return false, nil
	}
	return experience.FavoriteTitlesCached(ctx, u, keys, visibility, a.catalog.CachedGetMany)
}
