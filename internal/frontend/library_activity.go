package frontend

import (
	"context"
	"time"
)

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
