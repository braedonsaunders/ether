// These helpers support offline acceptance tests across package boundaries.
// Runtime callers use the production APIs in the other files.
package experience

import (
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// HubRows converts trusted recommender candidates to neutral rows;
// wire contexts stay on the producer side. Authorization and pagination are
// performed by experience.RowsService before returning these candidates.
func HubRows(hubs []core.Hub) []frontend.Row {
	out := make([]frontend.Row, 0, len(hubs))
	for _, h := range hubs {
		purpose := CandidatePurpose(h)
		row := frontend.Row{ID: h.ID, Title: h.Title, PreviewOnFocus: h.PreviewOnFocus, RankedTop10: h.RankedTop10, Purpose: purpose, Style: h.Style, Promoted: h.Promoted, More: h.More, Collection: h.Key}
		for i := range h.Items {
			item := h.Items[i]
			row.Entries = append(row.Entries, frontend.RowEntry{Kind: frontend.RowEntryTitle, Title: &item})
		}
		out = append(out, row)
	}
	return out
}
