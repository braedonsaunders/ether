package frontend

import (
	"context"
	"sort"

	"github.com/braedonsaunders/ether/internal/core"
)

// SeriesNextUpLimit is the indexed window for one series' next-up listing.
// The lookup is keyed by the series, never the library.
const SeriesNextUpLimit = 1000

// SeriesNextUpQuery is the bounded children request for one series' episodes.
func SeriesNextUpQuery() ItemQuery {
	return ItemQuery{Recursive: true, Types: []MediaType{core.TypeEpisode}, Limit: SeriesNextUpLimit}
}

// SeriesNextUp selects that series' next regular episode. Specials are skipped.
// When resumable, an in-progress episode wins; otherwise the first unwatched
// episode after the last completed one is returned, or S1E1 when none are watched.
// Episodes of any other series are ignored.
func SeriesNextUp(eps []Title, st map[RatingKey]WatchStatus, series RatingKey, resumable bool, unavailable func(*Title) bool) *Title {
	if unavailable == nil {
		unavailable = func(*Title) bool { return false }
	}
	if st == nil {
		st = map[RatingKey]WatchStatus{}
	}
	eps = regularSeriesEpisodes(eps, series)
	lastWatched := -1
	for i, e := range eps {
		ws := st[e.Key]
		if resumable && ws.ViewOffset > 0 && ws.ViewCount == 0 {
			return &eps[i]
		}
		if ws.ViewCount > 0 {
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

// LoadSeriesNextUp loads one series' episodes through exp.Children and selects
// the next episode. Children is invoked only with that series key.
func LoadSeriesNextUp(ctx context.Context, exp Experience, viewer *Viewer, device Device, series RatingKey, resumable bool) (*Title, error) {
	if exp == nil || series == 0 || series.Kind() != KindShow {
		return nil, nil
	}
	page, err := exp.Children(ctx, viewer, device, series, SeriesNextUpQuery())
	if err != nil {
		return nil, err
	}
	keys := make([]RatingKey, 0, len(page.Items))
	for _, title := range page.Items {
		keys = append(keys, title.Key)
	}
	state, err := exp.UserData(ctx, viewer, keys)
	if err != nil {
		return nil, err
	}
	return SeriesNextUp(page.Items, state, series, resumable, nil), nil
}

func regularSeriesEpisodes(eps []Title, series RatingKey) []Title {
	out := make([]Title, 0, len(eps))
	for _, ep := range eps {
		if ep.Key.Kind() != KindEpisode {
			continue
		}
		if ep.Type != "" && ep.Type != core.TypeEpisode {
			continue
		}
		show := ep.ShowKey
		if show == 0 {
			show = ShowKey(ep.Key.TMDB())
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
