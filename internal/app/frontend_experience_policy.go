package app

import (
	"context"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
)

// experienceVisibility supplies the same policy, library and source-availability
// inputs used by Plex, without passing a Plex server into the experience layer.
func experienceVisibility(policy experience.AccessPolicy, get func(context.Context, core.RatingKey) (*core.Title, error), available func(context.Context, []core.Title) []core.Title, sessions func() []*core.PlaySession, libraries []experience.Library) experience.Visibility {
	return experience.Visibility{Policy: policy, GetTitle: get, FilterAvailable: available, Sessions: sessions,
		SectionFor: func(kind core.MediaType) (int, bool) {
			want := core.TypeMovie
			switch kind {
			case core.TypeShow, core.TypeSeason, core.TypeEpisode:
				want = core.TypeShow
			}
			for _, library := range libraries {
				if library.Type == want {
					return library.ID, true
				}
			}
			return 0, false
		},
	}
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
