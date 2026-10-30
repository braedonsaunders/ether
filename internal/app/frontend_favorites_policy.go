package app

import (
	"context"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/favorites"
)

// favoriteAllowed resolves each neutral identity against live viewer policy.
func (a *App) favoriteAllowed(ctx context.Context, u *core.User, subject favorites.Subject, visibility experience.Visibility) bool {
	if u == nil {
		return false
	}
	switch subject.Kind {
	case favorites.KindTitle:
		if a.catalog == nil {
			return false
		}
		k, e := core.ParseRatingKey(subject.ID)
		if e != nil {
			return false
		}
		t, e := a.catalog.Get(ctx, k)
		return e == nil && t != nil && t.Key == k && visibility.Visible(ctx, u, t)
	case favorites.KindCollection:
		if a.collections == nil {
			return false
		}
		k, e := core.ParseRatingKey(subject.ID)
		if e != nil {
			return false
		}
		c, e := a.collections.Get(ctx, u, k)
		return e == nil && c != nil && visibility.CollectionAllowed(u, c)
	case favorites.KindChannel:
		if a.livetv == nil {
			return false
		}
		channels, e := a.livetv.AllowedChannels(ctx, u)
		if e != nil {
			return false
		}
		for _, c := range channels {
			if c.ID == subject.ID {
				return true
			}
		}
		return false
	case favorites.KindPerson:
		if a.catalog == nil {
			return false
		}
		raw, ok := strings.CutPrefix(subject.ID, "tmdb:")
		if !ok {
			return false
		}
		id, e := strconv.ParseInt(raw, 10, 64)
		if e != nil || id <= 0 || strconv.FormatInt(id, 10) != raw {
			return false
		}
		// A person is visible only when credited by at least one visible title.
		for offset := 0; offset < 1000; offset += 100 {
			page, e := a.catalog.Browse(ctx, core.BrowseQuery{Filter: core.Filter{Person: id}, Offset: offset, Limit: 100})
			if e != nil {
				return false
			}
			for i := range page.Items {
				if visibility.Visible(ctx, u, &page.Items[i]) {
					return true
				}
			}
			if len(page.Items) < 100 {
				return false
			}
		}
		return false
	}
	return false
}

// favoriteTitlesCached rechecks current policy without fetching metadata.
func (a *App) favoriteTitlesCached(ctx context.Context, u *core.User, keys []core.RatingKey, visibility experience.Visibility) (bool, error) {
	if a.catalog == nil {
		return false, nil
	}
	return favoriteTitlesWithCache(ctx, u, keys, visibility, a.catalog.CachedGetMany)
}

func favoriteTitlesWithCache(ctx context.Context, u *core.User, keys []core.RatingKey, visibility experience.Visibility, cached func(context.Context, []core.RatingKey) (map[core.RatingKey]core.Title, error)) (bool, error) {
	if u == nil || cached == nil {
		return false, nil
	}
	titles, err := cached(ctx, keys)
	if err != nil {
		return false, err
	}
	var parents []core.RatingKey
	for _, key := range keys {
		t, ok := titles[key]
		if !ok {
			return false, nil
		}
		if t.ShowKey != 0 {
			parents = append(parents, t.ShowKey)
		}
	}
	shows, err := cached(ctx, parents)
	if err != nil {
		return false, err
	}
	visibility.GetTitle = func(_ context.Context, key core.RatingKey) (*core.Title, error) {
		t, ok := shows[key]
		if !ok {
			return nil, core.ErrNotFound
		}
		return &t, nil
	}
	for _, key := range keys {
		t := titles[key]
		if !visibility.Visible(ctx, u, &t) {
			return false, nil
		}
	}
	return true, nil
}
