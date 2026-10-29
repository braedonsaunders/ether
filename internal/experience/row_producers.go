package experience

import (
	"context"

	"github.com/braedonsaunders/ether/internal/core"
)

// NextUp selects a permitted successor to an episode. Missing successors and
// lookup failures produce no row, matching the optional nature of next-up.
func NextUp(ctx context.Context, viewer *core.User, key core.RatingKey, next func(context.Context, core.RatingKey) (*core.Title, error), visibility Visibility) PostPlayRow {
	row := PostPlayRow{Purpose: core.RowPurpose("nextUp")}
	if key.Kind() != core.KindEpisode {
		return row
	}
	title, err := next(ctx, key)
	if err == nil && title != nil && visibility.Visible(ctx, viewer, title) {
		row.Items = []core.Title{*title}
	}
	return row
}

// LoadPostPlay expands the candidate prefix when policy or deduplication
// leaves the requested row short. The source signals exhaustion by returning
// fewer candidates than requested. Work is capped at 1000 candidates.
func LoadPostPlay(ctx context.Context, viewer *core.User, visibility Visibility, count int, seen map[core.RatingKey]bool, load func(context.Context, int) []core.PostPlayPick) PostPlayRow {
	fetch := count
	for {
		picks := load(ctx, fetch)
		trial := make(map[core.RatingKey]bool, len(seen))
		for k, v := range seen {
			trial[k] = v
		}
		row := PostPlay(ctx, picks, viewer, visibility, trial, count)
		if len(row.Items) >= count || len(picks) < fetch || fetch >= 1000 || ctx.Err() != nil {
			for k, v := range trial {
				seen[k] = v
			}
			return row
		}
		fetch = min(fetch*2, 1000)
	}
}
