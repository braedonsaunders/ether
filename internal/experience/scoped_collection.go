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
