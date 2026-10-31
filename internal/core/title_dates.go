package core

import (
	"time"
)

// DateAdded is the title's own release or air date. A known Released value
// always wins. Shows and seasons with no premiere of their own may still
// carry a derived air date in AddedAt, such as a virtual season built from
// aired episodes. Catalogue insertion, first observation, metadata refresh,
// file modification, descendant air dates and release-publication times are
// never the date added. Future dates are unknown.
func (title *Title) DateAdded(now time.Time) time.Time {
	if title == nil {
		return time.Time{}
	}
	date := title.Released
	if date.IsZero() && (title.Type == TypeShow || title.Type == TypeSeason) {
		date = title.AddedAt
	}
	if date.IsZero() || date.After(now) {
		return time.Time{}
	}
	return date
}
