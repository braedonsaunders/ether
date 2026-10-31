package plex

import (
	"context"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
)

func (s *Server) continueExperience(pr *Request) experience.Continue {
	c := experience.Continue{State: s.d.State, Now: s.now, FavoriteShows: s.d.FavoriteShows,
		Grandchildren: func(ctx context.Context, k core.RatingKey) ([]core.Title, error) {
			return s.metadataGrandchildren(ctx, pr, k)
		},
		Unrestricted: func(u *core.User) bool { return !s.viewerRestricted(u) },
		NextEpisode: func(ctx context.Context, k core.RatingKey) (*core.Title, error) {
			// Home continue uses the episode already in the catalogue. The
			// provider lookup stays on post-play, which is one show.
			if local, ok := s.d.Catalog.(interface {
				NextStoredEpisode(context.Context, core.RatingKey) (*core.Title, error)
			}); ok {
				return local.NextStoredEpisode(ctx, k)
			}
			return s.d.Catalog.NextEpisode(ctx, k)
		}, Visible: s.visibleTitle,
		SameDayUnavailable: func(ctx context.Context, t *core.Title) bool { return s.sameDayUnavailable(ctx, pr, t) },
		GetTitles:          s.getTitles, SectionAllowed: func(u *core.User, t core.MediaType) bool {
			sec, ok := s.sectionFor(t)
			return !ok || s.canSeeSection(u, sec.ID)
		},
		OnError: func(err error) { s.log.Warn("load in-progress", "err", err) }}
	if s.d.Discovery != nil && s.d.Discovery.Personal != nil {
		c.Filter = s.d.Discovery.Personal.FilterContinue
	}
	if s.d.EpisodeOrder != nil {
		c.EpisodeOrder = s.d.EpisodeOrder
	}
	return c
}
