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
			if visibility.TitleAllowed(viewer, &row.Items[i]) {
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
