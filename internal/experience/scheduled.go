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

// futureSeasonEpisodes is the metadata-only tail of one season. It does not
// walk the show's other seasons.
func (s *Service) futureSeasonEpisodes(ctx context.Context, v *core.User, show core.RatingKey, season int, q frontend.ItemQuery) ([]core.Title, error) {
	if s.d.Scheduled == nil || season <= 0 {
		return nil, nil
	}
	if s.scheduleBlocked(ctx, v, show) {
		return nil, nil
	}
	seasonKey := core.SeasonKey(show.TMDB(), season)
	episodes, err := s.d.Scheduled(ctx, seasonKey)
	if errors.Is(err, core.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	now := time.Now()
	extra := make([]core.Title, 0, len(episodes))
	for _, episode := range episodes {
		if episode.Type != core.TypeEpisode || episode.Key.Parent() != seasonKey || episode.Released.IsZero() || !episode.Released.After(now) {
			continue
		}
		if !s.scheduledAllowed(ctx, v, &episode) {
			continue
		}
		ok, err := s.matchesUserQuery(ctx, v, episode, q)
		if err != nil {
			return nil, err
		}
		if ok {
			extra = append(extra, episode)
		}
	}
	sort.SliceStable(extra, func(i, j int) bool { return extra[i].Episode < extra[j].Episode })
	return extra, nil
}

func (s *Service) pagedSeason(ctx context.Context, v *core.User, key core.RatingKey, q frontend.ItemQuery) (core.Page, bool, error) {
	if s.d.SeasonEpisodes == nil || !q.Recursive || key.Kind() != core.KindShow || q.Season == nil || q.Limit < 1 || q.Limit > 100 || q.Offset < 0 || q.AdjacentTo != 0 || q.StartAt != 0 || q.Watched != nil || q.Favorite != nil || q.Desc || q.Sort != "" || !plainItemFilter(q.Filter) {
		return core.Page{}, false, nil
	}
	if len(q.Types) > 0 && !containsMediaType(q.Types, core.TypeEpisode) {
		return core.Page{Items: []core.Title{}}, true, nil
	}
	aired, ok, err := s.d.SeasonEpisodes(ctx, v, key, *q.Season, q.Offset, q.Limit)
	if err != nil {
		// A failed season page stays empty. Falling through would list the
		// show and can paint another season under this one.
		return core.Page{Items: []core.Title{}}, true, nil
	}
	if !ok {
		return core.Page{}, false, nil
	}
	future, err := s.futureSeasonEpisodes(ctx, v, key, *q.Season, q)
	if err != nil {
		future = nil
	}
	items := aired.Items
	if items == nil {
		items = []core.Title{}
	}
	if q.Offset >= aired.Total {
		start := q.Offset - aired.Total
		if start >= len(future) {
			items = []core.Title{}
		} else {
			items = append([]core.Title{}, future[start:min(start+q.Limit, len(future))]...)
		}
	} else if q.Offset+len(items) >= aired.Total && len(future) > 0 {
		if room := q.Limit - len(items); room > 0 {
			items = append(items, future[:min(room, len(future))]...)
		}
	}
	season := *q.Season
	matched := make([]core.Title, 0, len(items))
	for _, item := range items {
		if item.Type == core.TypeEpisode && item.Season == season {
			matched = append(matched, item)
		}
	}
	if len(items) > 0 && len(matched) == 0 {
		return core.Page{Items: []core.Title{}}, true, nil
	}
	out := make([]core.Title, 0, len(matched))
	for i := range matched {
		if s.visible(ctx, v, &matched[i]) {
			out = append(out, matched[i])
		}
	}
	return core.Page{Items: out, Total: aired.Total + len(future)}, true, nil
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
