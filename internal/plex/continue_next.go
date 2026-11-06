package plex

import (
	"context"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
)

func (s *Server) continueExperience(pr *Request) experience.Continue {
	c := experience.Continue{ShowOnDeck: func(ctx context.Context, show core.RatingKey) (*core.Title, map[core.RatingKey]core.WatchStatus) {
		return s.showOnDeck(ctx, pr, show, nil)
	}, State: s.d.State, Now: s.now, FavoriteShows: s.d.FavoriteShows,
		Grandchildren: func(ctx context.Context, k core.RatingKey) ([]core.Title, error) {
			return s.metadataGrandchildren(ctx, pr, k)
		},
		Unrestricted: func(u *core.User) bool { return !s.viewerRestricted(u) },
		NextEpisode: func(ctx context.Context, k core.RatingKey) (*core.Title, error) {
			if local, ok := s.d.Catalog.(interface {
				ContinueEpisodes(context.Context, []core.RatingKey) (map[core.RatingKey]*core.Title, error)
			}); ok {
				next, err := local.ContinueEpisodes(ctx, []core.RatingKey{k})
				if err != nil {
					return nil, err
				}
				if t := next[k]; t != nil {
					return t, nil
				}
				return nil, core.ErrNotFound
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
	if hidden, ok := s.d.State.(interface {
		ContinueHidden(context.Context, int64, []core.RatingKey) (map[core.RatingKey]bool, error)
	}); ok {
		personal := c.Filter
		c.Filter = func(ctx context.Context, viewer *core.User, items []core.Title) []core.Title {
			if viewer == nil {
				return nil
			}
			keys := make([]core.RatingKey, 0, len(items))
			for _, item := range items {
				key := item.Key
				if item.Type == core.TypeEpisode {
					key = core.ShowKey(item.Key.TMDB())
				}
				keys = append(keys, key)
			}
			blocked, err := hidden.ContinueHidden(ctx, viewer.ID, keys)
			if err != nil {
				return nil
			}
			out := items[:0]
			for i, item := range items {
				if !blocked[keys[i]] {
					out = append(out, item)
				}
			}
			if personal != nil {
				out = personal(ctx, viewer, out)
			}
			return out
		}
	}
	if s.d.EpisodeOrder != nil {
		c.EpisodeOrder = s.d.EpisodeOrder
	}
	if local, ok := s.d.Catalog.(interface {
		ContinueEpisodes(context.Context, []core.RatingKey) (map[core.RatingKey]*core.Title, error)
	}); ok {
		c.NextEpisodes = local.ContinueEpisodes
	}
	return c
}
