package experience

import (
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// adjacentTitles runs after authorization, filters, and ordering; a hidden or
// filtered anchor is indistinguishable from a missing one.
func adjacentTitles(items []core.Title, q frontend.ItemQuery) (core.Page, error) {
	index := -1
	for i := range items {
		if items[i].Key == q.AdjacentTo {
			index = i
			break
		}
	}
	if index < 0 {
		return core.Page{}, core.ErrNotFound
	}
	window := items[max(index-1, 0):min(index+2, len(items))]
	total := len(window)
	start := min(max(q.Offset, 0), total)
	end := total
	if q.Limit > 0 && q.Limit < total-start {
		end = start + q.Limit
	}
	return core.Page{Items: window[start:end], Total: total}, nil
}
