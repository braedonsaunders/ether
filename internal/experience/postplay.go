package experience

import "github.com/braedonsaunders/ether/internal/core"

// PostPlayRow is a viewer-scoped set of ranked recommendations.
// The presentation and protocol-specific identifiers belong to the frontend.
type PostPlayRow struct {
	Purpose core.RowPurpose
	Items   []core.Title
}

// PostPlay constructs a recommendation row in the supplied rank order.
func PostPlay(picks []core.PostPlayPick, viewer *core.User, visibility Visibility) PostPlayRow {
	items := make([]core.Title, 0, len(picks))
	for _, pick := range picks {
		if visibility.TitleAllowed(viewer, &pick.Title) {
			items = append(items, pick.Title)
		}
	}
	return PostPlayRow{Purpose: core.RowPurpose("recommendation"), Items: items}
}

// UniqueRelated excludes previously served titles, recording all newly seen
// keys in row order so later rows cannot repeat them.
func UniqueRelated(items []core.Title, seen map[core.RatingKey]bool) []core.Title {
	out := items[:0:0]
	for _, item := range items {
		if !seen[item.Key] {
			seen[item.Key] = true
			out = append(out, item)
		}
	}
	return out
}
