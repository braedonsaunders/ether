package experience

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
)

// PostPlayRow is a viewer-scoped set of ranked recommendations.
// The presentation and protocol-specific identifiers belong to the frontend.
type PostPlayRow struct {
	Purpose core.RowPurpose
	Items   []core.Title
}

// PostPlay constructs a recommendation row in the supplied rank order.
func PostPlay(ctx context.Context, picks []core.PostPlayPick, viewer *core.User, visibility Visibility, seen map[core.RatingKey]bool, limit int) PostPlayRow {
	items := make([]core.Title, 0, len(picks))
	for _, pick := range picks {
		if !seen[pick.Title.Key] && visibility.Visible(ctx, viewer, &pick.Title) {
			seen[pick.Title.Key] = true
			items = append(items, pick.Title)
			if limit > 0 && len(items) >= limit {
				break
			}
		}
	}
	return PostPlayRow{Purpose: core.RowPurpose("recommendation"), Items: items}
}

// UniqueRelated excludes previously served titles, recording all newly seen
// keys in row order so later rows cannot repeat them.
func UniqueRelated(ctx context.Context, viewer *core.User, visibility Visibility, items []core.Title, seen map[core.RatingKey]bool, limit int) []core.Title {
	out := items[:0:0]
	for _, item := range items {
		if !seen[item.Key] && visibility.Visible(ctx, viewer, &item) {
			seen[item.Key] = true
			out = append(out, item)
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	return out
}
