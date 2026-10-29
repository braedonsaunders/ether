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
