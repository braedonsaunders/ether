package frontend

import (
	"context"
	"time"
)

type latestPlayedFilterKey struct{}

// WithLatestPlayed records the Latest IsPlayed filter. False keeps only
// unplayed items. Jellyfin and Emby both do that when the query omits
// IsPlayed and the user still has the default HidePlayedInLatest setting.
func WithLatestPlayed(ctx context.Context, played bool) context.Context {
	return context.WithValue(ctx, latestPlayedFilterKey{}, played)
}

// LatestPlayedFilter reports whether Latest should filter on played state.
func LatestPlayedFilter(ctx context.Context) (played bool, ok bool) {
	if ctx == nil {
		return false, false
	}
	played, ok = ctx.Value(latestPlayedFilterKey{}).(bool)
	return played, ok
}

// LatestItem carries date added. For a grouped series that is the latest
// aired episode. Nil means unknown. It is not a catalogue insertion time.
type LatestItem struct {
	Title   Title
	AddedAt *time.Time
}

// ActivityPage reports an exact total only when TotalExact is true; otherwise
// Total is a lower bound and More indicates that another page is available.
type ActivityPage struct {
	Items      []Title
	Total      int
	TotalExact bool
	More       bool
}

// LibraryActivity provides home-independent library and watch-state queries.
// LatestAdded returns movie/show library roots; TV defaults to grouped series.
type LibraryActivity interface {
	LatestAdded(context.Context, *Viewer, Device, string, int) ([]LatestItem, error)
	// requireExact requests a complete filtered count; an implementation must
	// return an error if it cannot determine one within its resource bounds.
	Resume(context.Context, *Viewer, Device, string, []MediaType, int, int, bool) (ActivityPage, error)
}

// LibraryActivityGrouping selects grouped series or individual aired episodes.
type LibraryActivityGrouping interface {
	LatestAddedGrouped(context.Context, *Viewer, Device, string, int, bool) ([]LatestItem, error)
}
