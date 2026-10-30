package plex

import (
	"context"
	"sort"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plex/model"
)

func (s *Server) scheduledEpisodes(ctx context.Context, pr *Request, season core.RatingKey) []core.Title {
	if !s.cfg().Catalog.ShowUpcomingEpisodes || season.Kind() != core.KindSeason || season.Season() <= 0 {
		return nil
	}
	schedule, ok := s.d.Catalog.(core.CatalogSchedule)
	if !ok {
		return nil
	}
	// Alternate groups have their own numbering; do not attach canonical future
	// episodes to an unrelated group or offer a detail page outside that tree.
	if s.d.EpisodeOrder != nil {
		grouped, err := s.d.EpisodeOrder.Grouped(ctx, pr.User, []core.RatingKey{core.ShowKey(season.TMDB())})
		if err != nil || grouped[core.ShowKey(season.TMDB())] {
			return nil
		}
	}
	parent, err := s.d.Catalog.Get(ctx, season)
	if err != nil || !s.visibleTitle(ctx, pr.User, parent) {
		return nil
	}
	episodes, err := schedule.ScheduledEpisodes(ctx, season)
	if err != nil {
		return nil
	}
	out := make([]core.Title, 0, len(episodes))
	for _, ep := range episodes {
		if ep.Type == core.TypeEpisode && ep.Key.Parent() == season && ep.Released.After(s.now()) && s.playableTitle(ctx, pr.User, &ep) {
			out = append(out, ep)
		}
	}
	return out
}

func (s *Server) scheduledEpisode(ctx context.Context, pr *Request, key core.RatingKey) (*core.Title, error) {
	if key.Kind() == core.KindEpisode {
		for _, ep := range s.scheduledEpisodes(ctx, pr, key.Parent()) {
			if ep.Key == key {
				return &ep, nil
			}
		}
	}
	return nil, core.ErrNotFound
}

func (s *Server) scheduledMetadata(ctx context.Context, pr *Request, t *core.Title) model.Metadata {
	md := s.metadata(ctx, pr, t, renderOpts{})
	md.Media = nil
	md.Title += " (Unaired)"
	md.Summary = "Scheduled air date: " + t.Released.Format("2006-01-02") + ". Not available to play yet. " + md.Summary
	// Base artwork uses the title-scoped handler, which repeats the schedule and
	// viewer checks. Do not invoke overlays or a custom provider for placeholders.
	md.Thumb = thumbURL(t)
	md.Art = artURL(t)
	md.Image = nil
	return md
}

func appendScheduled(aired, future []core.Title) []core.Title {
	if len(future) == 0 {
		return aired
	}
	out := append(append([]core.Title(nil), aired...), future...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Episode < out[j].Episode })
	return out
}
