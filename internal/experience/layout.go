package experience

import "github.com/braedonsaunders/ether/internal/core"

// LayoutCandidate is a row available to a layout engine. ItemCount includes
// non-title entries such as live channels or playlists.
type LayoutCandidate struct {
	ID        string
	Title     string
	Type      core.MediaType
	Purpose   core.RowPurpose
	ItemCount int
}

// ArrangeLayout returns indices into candidates in the engine's final order.
// Duplicate identifiers retain the last candidate, matching existing layout
// rendering. Unknown identifiers are ignored.
func ArrangeLayout(candidates []LayoutCandidate, finalize func([]LayoutCandidate) ([]string, error)) ([]int, error) {
	ids, err := finalize(candidates)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]int, len(candidates))
	for i, row := range candidates {
		byID[row.ID] = i
	}
	out := make([]int, 0, len(ids))
	for _, id := range ids {
		if i, ok := byID[id]; ok {
			out = append(out, i)
		}
	}
	return out, nil
}
