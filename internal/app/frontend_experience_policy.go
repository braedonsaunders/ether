package app

import (
	"context"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
)

// experienceVisibility binds host sources. Library selection lives in experience.BindVisibility.
func experienceVisibility(policy experience.AccessPolicy, get func(context.Context, core.RatingKey) (*core.Title, error), available func(context.Context, []core.Title) []core.Title, sessions func() []*core.PlaySession, libraries []experience.Library) experience.Visibility {
	return experience.BindVisibility(policy, get, available, sessions, libraries)
}

// resolveBrowseParents preserves catalogue Get's refresh and admission behavior.
// Missing or denied parents are omitted, matching Visibility.Playable's guard.
func resolveBrowseParents(ctx context.Context, keys []core.RatingKey, get func(context.Context, core.RatingKey) (*core.Title, error)) map[core.RatingKey]core.Title {
	out := make(map[core.RatingKey]core.Title, len(keys))
	seen := make(map[core.RatingKey]bool, len(keys))
	for _, key := range keys {
		if seen[key] {
			continue
		}
		seen[key] = true
		title, err := get(ctx, key)
		if err == nil && title != nil && title.Key == key {
			out[key] = *title
		}
	}
	return out
}
