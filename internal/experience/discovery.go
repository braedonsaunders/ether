package experience

import (
	"context"

	"github.com/braedonsaunders/ether/internal/core"
)

// DiscoveryCandidate contains a source row's neutral purpose and item candidates.
type DiscoveryCandidate struct {
	Index   int
	Purpose core.RowPurpose
	Items   []core.Title
}

// FilterDiscovery applies viewer visibility to each row before it is returned.
func FilterDiscovery(ctx context.Context, viewer *core.User, visibility Visibility, rows []DiscoveryCandidate) []DiscoveryCandidate {
	out := make([]DiscoveryCandidate, 0, len(rows))
	for _, row := range rows {
		items := make([]core.Title, 0, len(row.Items))
		for i := range row.Items {
			if visibility.Visible(ctx, viewer, &row.Items[i]) {
				items = append(items, row.Items[i])
			}
		}
		if len(items) != 0 {
			row.Items = items
			out = append(out, row)
		}
	}
	return out
}

// LoadDiscovery expands full candidate pages when visibility filtering leaves
// a row short. Sources with fewer results than requested signal exhaustion.
func LoadDiscovery(ctx context.Context, viewer *core.User, visibility Visibility, limit int, load func(context.Context, int) []DiscoveryCandidate) []DiscoveryCandidate {
	fetch := limit
	if fetch <= 0 {
		return FilterDiscovery(ctx, viewer, visibility, load(ctx, fetch))
	}
	for {
		rows := load(ctx, fetch)
		filtered := FilterDiscovery(ctx, viewer, visibility, rows)
		byIndex := make(map[int]int, len(filtered))
		for _, row := range filtered {
			byIndex[row.Index] = len(row.Items)
		}
		more := false
		for _, row := range rows {
			if len(row.Items) >= fetch && byIndex[row.Index] < limit {
				more = true
			}
		}
		if !more || fetch >= 1000 || ctx.Err() != nil {
			for i := range filtered {
				if len(filtered[i].Items) > limit {
					filtered[i].Items = filtered[i].Items[:limit]
				}
			}
			return filtered
		}
		fetch = min(fetch*2, 1000)
	}
}
