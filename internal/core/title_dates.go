package core

import (
	"time"
)

// DateAdded is the owner date-added rule. A movie uses its release date and an
// episode uses its air date. A series uses the air date of its most recently
// aired episode, then its first-air date. A season uses the latest aired
// episode in that season, then the season premiere. Those series and season
// dates are carried on AddedAt after catalogue hydration; a virtual season
// with no premiere of its own uses the same field. Catalogue insertion, first
// observation, metadata refresh, file modification and release-publication
// times are never the date added. Future dates are unknown.
func (title *Title) DateAdded(now time.Time) time.Time {
	if title == nil {
		return time.Time{}
	}
	if title.Type == TypeShow || title.Type == TypeSeason {
		if date := title.AddedAt; !date.IsZero() && !date.After(now) {
			return date
		}
	}
	if title.Released.IsZero() || title.Released.After(now) {
		return time.Time{}
	}
	return title.Released
}
