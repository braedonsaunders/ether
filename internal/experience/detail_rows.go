package experience

import (
	"context"

	"github.com/braedonsaunders/ether/internal/core"
)

// DetailRows composes policy-filtered next-up, post-play and related shelves.
// Related supplies neutral candidates; no renderer-specific context is exposed.
func DetailRows(ctx context.Context, viewer *core.User, key core.RatingKey, limit int, visibility Visibility,
	next func(context.Context, core.RatingKey) (*core.Title, error), postPlay func(context.Context, int) []core.PostPlayPick,
	related func(context.Context, int) ([]RankedCandidate, error)) ([]RankedCandidate, error) {
	if limit <= 0 {
		limit = 12
	}
	seen := map[core.RatingKey]bool{key: true}
	var out []RankedCandidate
	if next != nil {
		row := NextUp(ctx, viewer, key, next, visibility)
		if len(row.Items) > 0 && !seen[row.Items[0].Key] {
			seen[row.Items[0].Key] = true
			out = append(out, RankedCandidate{ID: "tv.upnext", Purpose: core.RowPurpose("nextUp"), Title: "Up Next", Type: core.TypeEpisode, Items: row.Items})
		}
	}
	if postPlay != nil {
		row := LoadPostPlay(ctx, viewer, visibility, limit, seen, postPlay)
		if len(row.Items) > 0 {
			prefix := "movie"
			if k := key.Kind(); k == core.KindShow || k == core.KindSeason || k == core.KindEpisode {
				prefix = "tv"
			}
			out = append(out, RankedCandidate{ID: prefix + ".postplay", Purpose: core.RowPurpose("recommendation"), Title: "Recommended For You", Style: "shelf", Items: row.Items})
		}
	}
	if related == nil {
		return out, nil
	}
	var candidates []RankedCandidate
	rows, err := LoadRelated(ctx, viewer, visibility, limit, seen, func(ctx context.Context, fetch int) ([]RelatedCandidate, error) {
		var err error
		candidates, err = related(ctx, fetch)
		if err != nil {
			return nil, err
		}
		rows := make([]RelatedCandidate, len(candidates))
		for i, c := range candidates {
			rows[i] = RelatedCandidate{Index: i, Items: c.Items}
		}
		return rows, nil
	})
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		candidate := candidates[row.Index]
		candidate.Items = row.Items
		out = append(out, candidate)
	}
	return out, nil
}
