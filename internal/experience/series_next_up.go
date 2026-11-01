package experience

import (
	"context"
	"sort"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// SeriesNextUpLimit is the indexed window for one series' next-up listing.
// The lookup is keyed by the series, never the library.
const SeriesNextUpLimit = 1000

// SeriesNextUpQuery is the bounded children request for one series' episodes.
func SeriesNextUpQuery() frontend.ItemQuery {
	return frontend.ItemQuery{Recursive: true, Types: []core.MediaType{core.TypeEpisode}, Limit: SeriesNextUpLimit}
}

// SeriesNextUp selects that series' next regular episode. Specials are skipped.
// When resumable, an in-progress episode wins; otherwise the first unwatched
// episode after the last completed one is returned, or S1E1 when none are watched.
// Episodes of any other series are ignored.
func SeriesNextUp(eps []core.Title, st map[core.RatingKey]core.WatchStatus, series core.RatingKey, resumable bool, unavailable func(*core.Title) bool) *core.Title {
	if unavailable == nil {
		unavailable = func(*core.Title) bool { return false }
	}
	if st == nil {
		st = map[core.RatingKey]core.WatchStatus{}
	}
	eps = regularSeriesEpisodes(eps, series)
	if resumable {
		return ShowOnDeck(eps, st, unavailable)
	}
	lastWatched := -1
	for i, e := range eps {
		if st[e.Key].ViewCount > 0 {
			lastWatched = i
		}
	}
	for i := lastWatched + 1; i < len(eps); i++ {
		if eps[i].Season > 0 && st[eps[i].Key].ViewCount == 0 && !unavailable(&eps[i]) {
			return &eps[i]
		}
	}
	if lastWatched < 0 {
		for i := range eps {
			if eps[i].Season > 0 && !unavailable(&eps[i]) {
				return &eps[i]
			}
		}
	}
	return nil
}

// LoadSeriesNextUp loads one series' episodes through Children and selects
// the next episode. The children callback is invoked only with that series key.
func LoadSeriesNextUp(ctx context.Context, series core.RatingKey, resumable bool, children func(context.Context, core.RatingKey, frontend.ItemQuery) (core.Page, error), userData func(context.Context, []core.RatingKey) (map[core.RatingKey]core.WatchStatus, error)) (*core.Title, error) {
	if series == 0 || series.Kind() != core.KindShow || children == nil || userData == nil {
		return nil, nil
	}
	page, err := children(ctx, series, SeriesNextUpQuery())
	if err != nil {
		return nil, err
	}
	keys := make([]core.RatingKey, 0, len(page.Items))
	for _, title := range page.Items {
		keys = append(keys, title.Key)
	}
	state, err := userData(ctx, keys)
	if err != nil {
		return nil, err
	}
	return SeriesNextUp(page.Items, state, series, resumable, nil), nil
}

func regularSeriesEpisodes(eps []core.Title, series core.RatingKey) []core.Title {
	out := make([]core.Title, 0, len(eps))
	for _, ep := range eps {
		if ep.Key.Kind() != core.KindEpisode {
			continue
		}
		if ep.Type != "" && ep.Type != core.TypeEpisode {
			continue
		}
		show := ep.ShowKey
		if show == 0 {
			show = core.ShowKey(ep.Key.TMDB())
		}
		if series != 0 && show != series {
			continue
		}
		if ep.Season == 0 {
			ep.Season = ep.Key.Season()
		}
		if ep.Episode == 0 {
			ep.Episode = ep.Key.Episode()
		}
		if ep.Type == "" {
			ep.Type = core.TypeEpisode
		}
		out = append(out, ep)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Season != out[j].Season {
			return out[i].Season < out[j].Season
		}
		return out[i].Episode < out[j].Episode
	})
	return out
}
