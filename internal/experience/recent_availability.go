package experience

import (
	"context"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// SameDayUnavailable preserves the release-evidence check used for next-up.
func SameDayUnavailable(ctx context.Context, title *core.Title, now time.Time, filter func(context.Context, []core.Title) []core.Title, source core.AvailabilitySource) bool {
	if title == nil || title.Type != core.TypeEpisode || title.Released.IsZero() {
		return false
	}
	if filter != nil {
		return len(filter(ctx, []core.Title{*title})) == 0
	}
	if age := now.Sub(title.Released); age < 0 || age >= 24*time.Hour {
		return false
	}
	if source != nil {
		known, available := source.Available(ctx, title.Key)
		return known && !available
	}
	return false
}
