package mediabrowser

import (
	"net/http"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

func nextUpSeries(r *http.Request) frontend.RatingKey {
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
	case frontend.KindShow:
		return key
	case frontend.KindSeason, frontend.KindEpisode:
		return frontend.ShowKey(key.TMDB())
	default:
		return 0
	}
}

func (i *instance) seriesNextUp(r *http.Request, v *frontend.Viewer, series frontend.RatingKey) ([]frontend.Title, error) {
	title, err := frontend.LoadSeriesNextUp(r.Context(), i.host.Experience, v, device(r), series, queryBool(r, "EnableResumable", true))
	if err != nil || title == nil {
		return nil, err
	}
	return []frontend.Title{*title}, nil
}

// nextUpScoped reports whether a Next Up request names a plugin series or
// season (seriesId/parentId), such as a sports league. Next Up there is about
// that one item; Ether has no per-series Next Up for plugin items, so the
// answer is an empty row, never Next Up from every other show. A library id
// in parentId (the home row) is not a plugin key and keeps the full row.
func nextUpScoped(r *http.Request) bool {
	for _, name := range []string{"seriesId", "parentId"} {
		raw := firstQuery(r, name)
		if raw == "" {
			continue
		}
		if key, err := ids.ParseRatingKeyGUID(raw); err == nil && key.IsPluginKey() {
			return true
		}
	}
	return false
}
