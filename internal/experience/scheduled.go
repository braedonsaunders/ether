package experience

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// appendScheduled adds metadata-only future episodes to a season, or to a
// show's recursive episode list. They stay out of playback: the broker
// refuses a future premiere before it resolves a release.
func (s *Service) appendScheduled(ctx context.Context, v *core.User, key core.RatingKey, items []core.Title, q frontend.ItemQuery) ([]core.Title, error) {
	if s.d.Scheduled == nil {
		return items, nil
	}
	var seasons []core.RatingKey
	switch key.Kind() {
	case core.KindSeason:
		if key.Season() <= 0 {
			return items, nil
		}
		seasons = []core.RatingKey{key}
	case core.KindShow:
		if !q.Recursive || s.d.Children == nil {
			return items, nil
		}
		kids, err := s.d.Children(ctx, v, key)
		if err != nil {
			return nil, err
		}
		for _, kid := range kids {
			if kid.Type == core.TypeSeason && kid.Key.Season() > 0 {
				seasons = append(seasons, kid.Key)
			}
		}
	default:
		return items, nil
	}
	seen := make(map[core.RatingKey]bool, len(items))
	for _, title := range items {
		seen[title.Key] = true
	}
	now := time.Now()
	extra := []core.Title{}
	for _, season := range seasons {
		if s.scheduleBlocked(ctx, v, core.ShowKey(season.TMDB())) {
			continue
		}
		episodes, err := s.d.Scheduled(ctx, season)
		if errors.Is(err, core.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, episode := range episodes {
			if seen[episode.Key] || episode.Type != core.TypeEpisode || episode.Key.Parent() != season {
				continue
			}
			if episode.Released.IsZero() || !episode.Released.After(now) {
				continue
			}
			if !s.scheduledAllowed(ctx, v, &episode) {
				continue
			}
			ok, err := s.matchesUserQuery(ctx, v, episode, q)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
			seen[episode.Key] = true
			extra = append(extra, episode)
		}
	}
	if len(extra) == 0 {
		return items, nil
	}
	out := append(append([]core.Title{}, items...), extra...)
	if q.Sort == "" {
		sort.SliceStable(out, func(i, j int) bool {
			if out[i].Season != out[j].Season {
				return out[i].Season < out[j].Season
			}
			return out[i].Episode < out[j].Episode
		})
	}
	return out, nil
}

func (s *Service) scheduledTitle(ctx context.Context, v *core.User, key core.RatingKey) *core.Title {
	if s.d.Scheduled == nil || key.Kind() != core.KindEpisode || key.Season() <= 0 {
		return nil
	}
	season := key.Parent()
	if s.scheduleBlocked(ctx, v, core.ShowKey(season.TMDB())) {
		return nil
	}
	episodes, err := s.d.Scheduled(ctx, season)
	if err != nil {
		return nil
	}
	now := time.Now()
	for i := range episodes {
		episode := &episodes[i]
		if episode.Key != key || episode.Type != core.TypeEpisode || episode.Released.IsZero() || !episode.Released.After(now) {
			continue
		}
		if s.scheduledAllowed(ctx, v, episode) {
			return episode
		}
	}
	return nil
}

func (s *Service) scheduleBlocked(ctx context.Context, v *core.User, show core.RatingKey) bool {
	if show == 0 || s.d.Grouped == nil {
		return false
	}
	grouped, err := s.d.Grouped(ctx, v, []core.RatingKey{show})
	return err != nil || grouped[show]
}

func (s *Service) scheduledAllowed(ctx context.Context, v *core.User, title *core.Title) bool {
	if title == nil || s.d.Visibility.SectionFor == nil {
		return false
	}
	id, ok := s.d.Visibility.SectionFor(title.Type)
	if !ok || !SectionAllowed(v, id) || !s.d.Visibility.Playable(ctx, v, title) {
		return false
	}
	return true
}

func (s *Service) vibeMembers(ctx context.Context, v *core.User, query string, q frontend.ItemQuery) ([]core.Title, error) {
	_, members, handled, err := s.d.Search.Vibe.Find(ctx, v, query)
	if err != nil || !handled {
		return nil, err
	}
	out := make([]core.Title, 0, len(members))
	for _, title := range members {
		if !s.visible(ctx, v, &title) {
			continue
		}
		ok, err := s.matchesUserQuery(ctx, v, title, q)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, title)
		}
	}
	return out, nil
}
