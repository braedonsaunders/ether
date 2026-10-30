package experience

import (
	"context"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
)

const (
	// favoritePersonPageSize is the catalogue page requested for a person.
	favoritePersonPageSize = 100
	// favoritePersonScanBound is the most credited titles a person decision
	// may examine. The next page, at offset 1000, is never requested.
	favoritePersonScanBound = 1000
)

// FavoriteKind is the neutral subject class for a favorite decision.
type FavoriteKind string

const (
	FavoriteKindTitle      FavoriteKind = "title"
	FavoriteKindPerson     FavoriteKind = "person"
	FavoriteKindCollection FavoriteKind = "collection"
	FavoriteKindChannel    FavoriteKind = "channel"
)

// FavoriteSubject is a favorite identity with no protocol or storage attached.
type FavoriteSubject struct {
	Kind FavoriteKind
	ID   string
}

// FavoriteSources are the current-policy lookups. A nil function is a missing
// dependency and fails closed. These functions are not used by the cached path.
type FavoriteSources struct {
	Title      func(context.Context, core.RatingKey) (*core.Title, error)
	Collection func(context.Context, *core.User, core.RatingKey) (*core.Collection, error)
	ChannelIDs func(context.Context, *core.User) ([]string, error)
	PersonPage func(context.Context, int64, int, int) ([]core.Title, error)
}

// CanonicalTMDBPerson accepts only a "tmdb:" prefix plus the canonical base-10
// form of a positive int64. Leading zeros, signs, and surrounding space fail.
func CanonicalTMDBPerson(id string) (int64, bool) {
	raw, ok := strings.CutPrefix(id, "tmdb:")
	if !ok {
		return 0, false
	}
	parsed, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || parsed <= 0 || strconv.FormatInt(parsed, 10) != raw {
		return 0, false
	}
	return parsed, true
}

// FavoriteAllowed decides whether the viewer may favorite this subject from
// current dependencies. Callers still validate the active viewer. Person
// credits use the supplied Visibility, including whatever title lookup it has.
func FavoriteAllowed(ctx context.Context, viewer *core.User, subject FavoriteSubject, visibility Visibility, sources FavoriteSources) bool {
	if viewer == nil {
		return false
	}
	switch subject.Kind {
	case FavoriteKindTitle:
		if sources.Title == nil {
			return false
		}
		key, err := core.ParseRatingKey(subject.ID)
		if err != nil {
			return false
		}
		title, err := sources.Title(ctx, key)
		return err == nil && title != nil && title.Key == key && visibility.Visible(ctx, viewer, title)
	case FavoriteKindCollection:
		if sources.Collection == nil {
			return false
		}
		key, err := core.ParseRatingKey(subject.ID)
		if err != nil {
			return false
		}
		collection, err := sources.Collection(ctx, viewer, key)
		return err == nil && collection != nil && visibility.CollectionAllowed(viewer, collection)
	case FavoriteKindChannel:
		if sources.ChannelIDs == nil {
			return false
		}
		ids, err := sources.ChannelIDs(ctx, viewer)
		if err != nil {
			return false
		}
		for _, id := range ids {
			if id == subject.ID {
				return true
			}
		}
		return false
	case FavoriteKindPerson:
		if sources.PersonPage == nil {
			return false
		}
		id, ok := CanonicalTMDBPerson(subject.ID)
		if !ok {
			return false
		}
		// A person is visible only when credited by at least one visible title.
		for offset := 0; offset < favoritePersonScanBound; offset += favoritePersonPageSize {
			items, err := sources.PersonPage(ctx, id, offset, favoritePersonPageSize)
			if err != nil {
				return false
			}
			for i := range items {
				if visibility.Visible(ctx, viewer, &items[i]) {
					return true
				}
			}
			if len(items) < favoritePersonPageSize {
				return false
			}
		}
		return false
	}
	return false
}

// FavoriteTitlesCached rechecks title policy using only the injected local
// catalogue read. It does not call FavoriteSources and replaces Visibility's
// title lookup for this call so a parent read cannot leave the cache. A cache
// error is returned and the decision is false, so the caller fails closed. A missing title
// or a hidden or missing parent is a deny without an error.
func FavoriteTitlesCached(ctx context.Context, viewer *core.User, keys []core.RatingKey, visibility Visibility, cached func(context.Context, []core.RatingKey) (map[core.RatingKey]core.Title, error)) (bool, error) {
	if viewer == nil || cached == nil {
		return false, nil
	}
	titles, err := cached(ctx, keys)
	if err != nil {
		return false, err
	}
	var parents []core.RatingKey
	for _, key := range keys {
		title, ok := titles[key]
		if !ok {
			return false, nil
		}
		if title.ShowKey != 0 {
			parents = append(parents, title.ShowKey)
		}
	}
	shows, err := cached(ctx, parents)
	if err != nil {
		return false, err
	}
	visibility.GetTitle = func(_ context.Context, key core.RatingKey) (*core.Title, error) {
		show, ok := shows[key]
		if !ok {
			return nil, core.ErrNotFound
		}
		return &show, nil
	}
	for _, key := range keys {
		title := titles[key]
		if !visibility.Visible(ctx, viewer, &title) {
			return false, nil
		}
	}
	return true, nil
}
