package app

import (
	"context"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/favorites"
	"github.com/braedonsaunders/ether/internal/notify"
)

// favoriteEpisodeCandidates never exposes an episode without the inherited
// show and section policy. Discovery availability is deliberately not used:
// these alerts describe air dates, not playable releases.
func (a *App) favoriteEpisodeCandidates(ctx context.Context, viewer *core.User) ([]core.Title, error) {
	entries, err := a.favorites.List(ctx, viewer, favorites.KindTitle, 0)
	if err != nil {
		return nil, err
	}
	visibility := experienceVisibility(a.safety, a.catalog.Get, nil, nil, []experience.Library{{ID: 1, Name: "Movies", Type: core.TypeMovie}, {ID: 2, Name: "TV Shows", Type: core.TypeShow}})
	var result []core.Title
	for _, entry := range entries {
		key, err := core.ParseRatingKey(entry.Subject.ID)
		if err != nil || key.Kind() != core.KindShow {
			continue
		}
		show, err := a.catalog.Get(ctx, key)
		if err != nil {
			return nil, err
		}
		if show == nil || !visibility.Playable(ctx, viewer, show) {
			continue
		}
		episodes, err := a.catalog.Grandchildren(ctx, key)
		if err != nil {
			return nil, err
		}
		for i := range episodes {
			episode := &episodes[i]
			if episode.ShowKey != key || episode.Released.Before(entry.AddedAt) || !visibility.Playable(ctx, viewer, episode) {
				continue
			}
			result = append(result, *episode)
		}
	}
	return result, nil
}

func (a *App) runFavoriteEpisodeAlerts(ctx context.Context) {
	detector := notify.FavoriteEpisodeDetector{Preferences: &notify.ViewerAlerts{DB: a.db, Active: a.users.ActiveUser}, Episodes: a.favoriteEpisodeCandidates}
	ticker := time.NewTicker(15 * time.Minute)
	defer ticker.Stop()
	for {
		if err := detector.Scan(ctx); err != nil && ctx.Err() == nil {
			a.log.Warn("favorite episode alert scan failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (a *App) viewerPush() *notify.ViewerPush {
	return &notify.ViewerPush{Notifications: a.notifications, Preferences: &notify.ViewerAlerts{DB: a.db, Active: a.users.ActiveUser}, Allowed: func(ctx context.Context, viewer *core.User, key core.RatingKey) (bool, error) {
		episode, err := a.catalog.Get(ctx, key)
		if err != nil {
			return false, err
		}
		if episode == nil || episode.Type != core.TypeEpisode || episode.ShowKey == 0 {
			return false, nil
		}
		visibility := experienceVisibility(a.safety, a.catalog.Get, nil, nil, []experience.Library{{ID: 1, Name: "Movies", Type: core.TypeMovie}, {ID: 2, Name: "TV Shows", Type: core.TypeShow}})
		if !visibility.Playable(ctx, viewer, episode) {
			return false, nil
		}
		return a.favorites.Has(ctx, viewer, favorites.Subject{Kind: favorites.KindTitle, ID: episode.ShowKey.String()})
	}}
}

func (a *App) runViewerPush(ctx context.Context) {
	push := a.viewerPush()
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		if err := push.Drain(ctx); err != nil && ctx.Err() == nil {
			a.log.Warn("viewer push delivery failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
