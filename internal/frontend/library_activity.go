package frontend

import (
	"context"
	"time"
)

// LatestItem carries the genuine stored catalogue addition time. Nil means
// unknown; adapters must not substitute premiere or modification dates.
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
// LatestAdded currently covers movie/show library roots, ordered by their
// stored catalogue addition time. Episode grouping is a separate capability.
type LibraryActivity interface {
	LatestAdded(context.Context, *Viewer, Device, string, int) ([]LatestItem, error)
	// requireExact requests a complete filtered count; an implementation must
	// return an error if it cannot determine one within its resource bounds.
	Resume(context.Context, *Viewer, Device, string, []MediaType, int, int, bool) (ActivityPage, error)
}
