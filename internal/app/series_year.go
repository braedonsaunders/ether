package app

import (
	"context"
	"errors"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/releaseparse"
)

// seriesYearConflict answers the resolver: does the catalogue (local mirror,
// then TMDB through the same search the console uses) hold a different show
// with this title that first aired in year? A reboot or namesake does; a show
// an indexer merely listed under the wrong year does not.
func (a *App) seriesYearConflict(ctx context.Context, show core.RatingKey, title string, year int) (bool, error) {
	if a.catalog == nil {
		return false, errors.New("catalogue unavailable")
	}
	shows, err := a.catalog.SearchAll(ctx, title, []core.MediaType{core.TypeShow}, 20)
	if err != nil {
		return false, err
	}
	for _, t := range shows {
		if t.Key == show || t.Year != year {
			continue
		}
		if releaseparse.TitlesMatch(t.Title, title) || (t.OriginalTitle != "" && releaseparse.TitlesMatch(t.OriginalTitle, title)) {
			return true, nil
		}
	}
	return false, nil
}
