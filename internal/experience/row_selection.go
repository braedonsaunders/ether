package experience

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	"strings"
)

// Apply source suitability to discovery, including mixed Home shelves. Personal
// saved lists and playback state must never disappear because sources change.
func DiscoveryAvailabilityRow(id string) bool {
	for _, keep := range []string{"continue", "inprogress", "ondeck", "upnext", "watchlist", "playlist", "collection"} {
		if strings.Contains(strings.ToLower(id), keep) {
			return false
		}
	}
	return strings.HasPrefix(id, "home.") || strings.HasPrefix(id, "movie.") || strings.HasPrefix(id, "availability.") || strings.Contains(id, "recommend") || strings.Contains(id, "trending") || strings.Contains(id, "popular")
}

// RowSelection applies product policy before wire rendering, preserving source order.
type RowSelection struct {
	Visibility          Visibility
	PrioritizeAvailable func(context.Context, []core.Title) []core.Title
	SameDayUnavailable  func(context.Context, *core.Title) bool
}

func (s RowSelection) Items(ctx context.Context, viewer *core.User, row core.Hub) []core.Title {
	items := s.Visibility.TitlesAllowed(viewer, row.Items)
	if s.PrioritizeAvailable != nil && DiscoveryAvailabilityRow(row.ID) {
		items = s.PrioritizeAvailable(ctx, items)
	}
	if s.SameDayUnavailable != nil && (strings.HasSuffix(row.ID, ".ondeck") || row.ID == "tv.upnext") {
		kept := items[:0:0]
		for i := range items {
			if !s.SameDayUnavailable(ctx, &items[i]) {
				kept = append(kept, items[i])
			}
		}
		items = kept
	}
	return items
}
