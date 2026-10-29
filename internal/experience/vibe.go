package experience

import (
	"context"

	"github.com/braedonsaunders/ether/internal/core"
)

// VibeSource returns a viewer-scoped ephemeral collection and candidate items.
type VibeSource interface {
	VibeCollection(context.Context, *core.User, string) (*core.Collection, []core.Title, bool, error)
}

// VibeSearch resolves an optional natural-language collection source.
type VibeSearch struct {
	Source     VibeSource
	Visibility Visibility
}

func (s VibeSearch) Find(ctx context.Context, viewer *core.User, query string) (*core.Collection, []core.Title, bool, error) {
	if s.Source == nil {
		return nil, nil, false, nil
	}
	collection, items, ok, err := s.Source.VibeCollection(ctx, viewer, query)
	if err != nil || !ok {
		return nil, nil, ok, err
	}
	if collection != nil && !s.Visibility.CollectionAllowed(viewer, collection) {
		return nil, nil, true, nil
	}
	filtered := make([]core.Title, 0, len(items))
	for i := range items {
		if s.Visibility.Visible(ctx, viewer, &items[i]) {
			filtered = append(filtered, items[i])
		}
	}
	return collection, filtered, true, nil
}
