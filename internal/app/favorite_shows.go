package app

import (
	"context"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/favorites"
)

// favoriteShows reads the current viewer's authorized favorite show set.
func (a *App) favoriteShows(ctx context.Context, viewer *core.User) (map[core.RatingKey]bool, error) {
	out := map[core.RatingKey]bool{}
	if a.favorites == nil || viewer == nil {
		return out, nil
	}
	entries, err := a.favorites.List(ctx, viewer, favorites.KindTitle, 0)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		key, err := core.ParseRatingKey(entry.Subject.ID)
		if err == nil && key.Kind() == core.KindShow {
			out[key] = true
		}
	}
	return out, nil
}
