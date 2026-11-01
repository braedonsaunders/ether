package mediabrowser

import (
	"context"
	"net/http"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

func nextUpSeries(r *http.Request) core.RatingKey {
	raw := firstQuery(r, "seriesId")
	if raw == "" {
		raw = firstQuery(r, "parentId")
	}
	if raw == "" {
		return 0
	}
	key, err := ids.ParseRatingKeyGUID(raw)
	if err != nil {
		return 0
	}
	switch key.Kind() {
	case core.KindShow:
		return key
	case core.KindSeason, core.KindEpisode:
		return core.ShowKey(key.TMDB())
	default:
		return 0
	}
}

func (i *instance) seriesNextUp(r *http.Request, v *frontend.Viewer, series core.RatingKey) ([]frontend.Title, error) {
	title, err := experience.LoadSeriesNextUp(r.Context(), series, queryBool(r, "EnableResumable", true),
		func(ctx context.Context, key core.RatingKey, q frontend.ItemQuery) (core.Page, error) {
			return i.host.Experience.Children(ctx, v, device(r), key, q)
		},
		func(ctx context.Context, keys []core.RatingKey) (map[core.RatingKey]core.WatchStatus, error) {
			return i.host.Experience.UserData(ctx, v, keys)
		},
	)
	if err != nil || title == nil {
		return nil, err
	}
	return []frontend.Title{*title}, nil
}
