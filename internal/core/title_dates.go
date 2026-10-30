package core

import (
	"time"
)

// AddedDate is the title's release/air date. Series and seasons carry the
// catalogue's latest-aired descendant date, including its air-date fallbacks.
// Publication, observation and metadata refresh times are independent facts.
func (title *Title) DateAdded(now time.Time) time.Time {
	if title == nil {
		return time.Time{}
	}
	date := title.Released
	if (title.Type == TypeShow || title.Type == TypeSeason) && !title.AddedAt.IsZero() {
		date = title.AddedAt
	}
	if date.After(now) {
		return time.Time{}
	}
	return date
}
