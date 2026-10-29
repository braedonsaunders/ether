package experience

import (
	"context"

	"github.com/braedonsaunders/ether/internal/core"
)

// RelatedCandidate contains protocol-neutral item candidates for one row.
// Index identifies the source row for its rendering adapter.
type RelatedCandidate struct {
	Index int
	Items []core.Title
}

// LoadRelated filters before deduplication and limits, expanding candidate
// requests while a full source page leaves a row short.
func LoadRelated(ctx context.Context, viewer *core.User, visibility Visibility, count int, seen map[core.RatingKey]bool, load func(context.Context, int) ([]RelatedCandidate, error)) ([]RelatedCandidate, error) {
	fetch := count
	for {
		rows, err := load(ctx, fetch)
		if err != nil {
			return nil, err
		}
		trial := make(map[core.RatingKey]bool, len(seen))
		for k, v := range seen {
			trial[k] = v
		}
		out := make([]RelatedCandidate, 0, len(rows))
		more := false
		for _, row := range rows {
			items := UniqueRelated(ctx, viewer, visibility, row.Items, trial, count)
			if len(items) < count && len(row.Items) >= fetch {
				more = true
			}
			if len(items) > 0 {
				row.Items = items
				out = append(out, row)
			}
		}
		if more && fetch < 1000 && ctx.Err() == nil {
			fetch = min(fetch*2, 1000)
			continue
		}
		for k, v := range trial {
			seen[k] = v
		}
		return out, nil
	}
}
