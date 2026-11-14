package experience

import (
	"context"
	"strconv"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// ArrangeNeutralHome returns candidate indices in the shared layout's order.
// Indices point at the caller's original sidecars; duplicate producer IDs use
// the last sidecar, matching the established Home layout behavior.
func ArrangeNeutralHome(ctx context.Context, source core.NeutralHomeLayoutSource, viewer *core.User, situation core.HomeSituation, section string, rows []core.NeutralHomeRow) ([]int, error) {
	indices := make(map[string]int, len(rows))
	for i, row := range rows {
		indices[row.ID] = i
	}
	ordered, err := source.FinalizeNeutralHome(ctx, viewer, situation, section, rows)
	if err != nil {
		return nil, err
	}
	out := make([]int, 0, len(ordered))
	for _, row := range ordered {
		if i, ok := indices[row.ID]; ok {
			out = append(out, i)
		}
	}
	return out, nil
}

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

// LayoutRows applies shared Home placement to neutral frontend rows.
func LayoutRows(ctx context.Context, source core.NeutralHomeLayoutSource, libraries []Library, req frontend.RowRequest, sit core.HomeSituation, rows []frontend.Row) ([]frontend.Row, error) {
	section := "home"
	if req.Placement == frontend.PlacementLibrary {
		section = "movies"
		for _, l := range libraries {
			if strconv.Itoa(l.ID) == req.ViewID && l.Type == core.TypeShow {
				section = "shows"
			}
		}
	}
	if req.Placement == frontend.PlacementDetail {
		return rows, nil
	}
	candidates := make([]core.NeutralHomeRow, 0, len(rows))
	indices := make(map[string]int, len(rows))
	for i, row := range rows {
		indices[row.ID] = i
		n := core.NeutralHomeRow{ID: row.ID, Title: row.Title, Kind: row.Kind, Purpose: row.Purpose, ItemCount: len(row.Entries), Always: row.Layout.Always, PreviewOnFocus: row.PreviewOnFocus, RankedTop10: row.RankedTop10}
		for _, entry := range row.Entries {
			if entry.Title != nil {
				n.Items = append(n.Items, *entry.Title)
				if n.Type == "" {
					n.Type = entry.Title.Type
				}
			}
		}
		candidates = append(candidates, n)
	}
	ordered, err := source.FinalizeNeutralHome(ctx, req.Viewer, sit, section, candidates)
	if err != nil {
		return nil, err
	}
	out := make([]frontend.Row, 0, len(ordered))
	for _, arranged := range ordered {
		index, ok := indices[arranged.ID]
		if !ok {
			continue
		}
		row := rows[index]
		rank := len(out)
		row.Rank = rank
		row.Layout.Order = rank
		row.Layout.Style = row.Style
		row.Layout.Always = arranged.Always
		row.PreviewOnFocus = arranged.PreviewOnFocus
		row.Layout.PreviewOnFocus = arranged.PreviewOnFocus
		row.RankedTop10 = arranged.RankedTop10
		row.Layout.RankedTop10 = arranged.RankedTop10
		out = append(out, row)
	}
	return out, nil
}
