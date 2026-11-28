package recommend

import (
	"github.com/braedonsaunders/ether/internal/contentrating"
	"github.com/braedonsaunders/ether/internal/core"
)

// FilterTitles drops titles above the user's content-rating limit (by
// enforcement age level, see contentrating.EnforceExceeds), preserving order.
func FilterTitles(titles []core.Title, user *core.User) []core.Title {
	if user == nil || user.MaxContentRating == "" {
		return titles
	}
	out := titles[:0:0]
	for _, t := range titles {
		rating := t.EnforcementRating
		if rating == "" {
			rating = t.ContentRating
		}
		if rating == contentrating.UncertifiedAdult {
			continue
		}
		if !contentrating.EnforceExceeds(rating, user.MaxContentRating) && contentrating.Allowed(t.ContentRating, user.MaxContentRating) {
			out = append(out, t)
		}
	}
	return out
}
