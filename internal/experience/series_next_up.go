package experience

import (
	"context"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// SeriesNextUpLimit is the indexed window for one series' next-up listing.
// The lookup is keyed by the series, never the library.
const SeriesNextUpLimit = frontend.SeriesNextUpLimit

// SeriesNextUpQuery is the bounded children request for one series' episodes.
func SeriesNextUpQuery() frontend.ItemQuery {
	return frontend.SeriesNextUpQuery()
}

// SeriesNextUp selects that series' next regular episode. Specials are skipped.
// When resumable, an in-progress episode wins; otherwise the first unwatched
// episode after the last completed one is returned, or S1E1 when none are watched.
// Episodes of any other series are ignored.
func SeriesNextUp(eps []core.Title, st map[core.RatingKey]core.WatchStatus, series core.RatingKey, resumable bool, unavailable func(*core.Title) bool) *core.Title {
	return frontend.SeriesNextUp(eps, st, series, resumable, unavailable)
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
