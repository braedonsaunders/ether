package app

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/favorites"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/personalize"
)

func (a *App) favoriteSignals(ctx context.Context, viewer *core.User) ([]personalize.FavoriteSignal, error) {
	var out []personalize.FavoriteSignal
	for _, kind := range []favorites.Kind{favorites.KindTitle, favorites.KindPerson} {
		entries, err := a.favorites.List(ctx, viewer, kind, 0)
		if errors.Is(err, favorites.ErrForbidden) {
			// A viewer without server access (imported history only) has no
			// favorites to weigh; their taste still builds from history.
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			signal := personalize.FavoriteSignal{AddedAt: entry.AddedAt}
			if kind == favorites.KindTitle {
				signal.Key, _ = core.ParseRatingKey(entry.Subject.ID)
			} else {
				raw, ok := strings.CutPrefix(entry.Subject.ID, "tmdb:")
				if !ok {
					continue
				}
				signal.PersonID, _ = strconv.ParseInt(raw, 10, 64)
			}
			out = append(out, signal)
		}
	}
	return out, nil
}

func (a *App) favoriteChanged(ctx context.Context, id int64, subject favorites.Subject) {
	if a.personalize != nil {
		a.personalize.FavoritesChanged(id)
	}
	if a.sessionEvents != nil {
		event := frontend.Event{Type: "UserDataChanged", ViewerID: id, SubjectKind: string(subject.Kind), SubjectID: subject.ID}
		if subject.Kind == favorites.KindTitle {
			event.Item, _ = core.ParseRatingKey(subject.ID)
		}
		if err := a.sessionEvents.Publish(ctx, event); err != nil {
			a.log.Warn("favorites: publish failed", "err", err)
		}
	}
}
