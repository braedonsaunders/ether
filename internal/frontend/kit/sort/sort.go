// Package sort translates protocol sort keys into Ether browse orders.
package sort

import (
	"github.com/braedonsaunders/ether/internal/core"
	"strings"
)

// Order recognizes shared Jellyfin/Emby sort names. Unknown values return false
// so the caller can choose a protocol-specific default or reject the request.
func Order(value string) (core.SortOrder, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "sortname", "name", "title":
		return core.SortTitle, true
	case "datecreated", "dateadded":
		return core.SortAdded, true
	case "premieredate", "productionyear":
		return core.SortReleased, true
	case "communityrating":
		return core.SortRating, true
	case "popularity":
		return core.SortPopularity, true
	default:
		return "", false
	}
}
