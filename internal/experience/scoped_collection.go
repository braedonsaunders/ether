package experience

import (
	"context"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// ScopedCollection selects upcoming members only through an enabled,
// viewer-and-device-scoped collection capability.
type ScopedCollection struct {
	Collections core.Collections
	Visibility  Visibility
	Now         func() time.Time
	// Unrestricted is a trusted host capability; nil enforces inherited policy.
	Unrestricted func(*core.User) bool
}

func (s ScopedCollection) Items(ctx context.Context, viewer *core.User, key core.RatingKey, deviceClass string) ([]core.Title, error) {
	placeholders, ok := s.Collections.(core.CollectionPlaceholder)
	if !ok || !placeholders.AllowsPlaceholders(ctx, viewer, key, deviceClass) {
		return nil, core.ErrNotFound
	}
	page, err := s.Collections.Items(ctx, viewer, key, 0, 5000)
	if err != nil {
		return nil, err
	}
	if s.Unrestricted == nil || !s.Unrestricted(viewer) {
		return s.Visibility.UpcomingItems(ctx, viewer, page.Items, s.Now), nil
	}
	out := make([]core.Title, 0, len(page.Items))
	for _, title := range page.Items {
		if !s.Visibility.TitleAllowed(viewer, &title) || title.Released.IsZero() {
			continue
		}
		if !title.Released.After(s.Now()) && len(s.Visibility.Available(ctx, viewer, []core.Title{title})) == 0 {
			continue
		}
		out = append(out, title)
	}
	return out, nil
}

// Get authorizes a collection and optionally calculates its upcoming count.
func (s ScopedCollection) Get(ctx context.Context, viewer *core.User, key core.RatingKey, device string, count bool) (*core.Collection, error) {
	if s.Collections == nil {
		return nil, core.ErrNotFound
	}
	collection, err := s.Collections.Get(ctx, viewer, key)
	if err != nil {
		return nil, err
	}
	if !s.Visibility.CollectionAllowed(viewer, collection) {
		return nil, core.ErrNotFound
	}
	if count && collection.Upcoming {
		items, err := s.Items(ctx, viewer, key, device)
		if err != nil {
			return nil, err
		}
		collection.ChildCount = len(items)
	}
	return collection, nil
}

func (s ScopedCollection) Counts(ctx context.Context, viewer *core.User, collections []core.Collection, device string) []core.Collection {
	out := make([]core.Collection, 0, len(collections))
	for _, collection := range collections {
		if collection.Upcoming {
			items, err := s.Items(ctx, viewer, collection.Key, device)
			if err != nil || len(items) == 0 {
				continue
			}
			collection.ChildCount = len(items)
		}
		out = append(out, collection)
	}
	return out
}

func (s ScopedCollection) Page(ctx context.Context, viewer *core.User, collection *core.Collection, key core.RatingKey, device string, offset, size int) (core.Page, error) {
	if !collection.Upcoming {
		return s.Collections.Items(ctx, viewer, key, offset, size)
	}
	items, err := s.Items(ctx, viewer, key, device)
	if err != nil {
		return core.Page{}, err
	}
	start := min(offset, len(items))
	end := min(start+size, len(items))
	return core.Page{Items: items[start:end], Total: len(items)}, nil
}
