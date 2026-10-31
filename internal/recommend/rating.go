package recommend

import (
	"github.com/braedonsaunders/ether/internal/contentrating"
	"github.com/braedonsaunders/ether/internal/core"
)

// FilterTitles drops titles above the user's content-rating limit (by age
// level, see contentrating.Exceeds), preserving order.
func FilterTitles(titles []core.Title, user *core.User) []core.Title {
	if user == nil || user.MaxContentRating == "" {
		return titles
	}
	out := titles[:0:0]
	for _, t := range titles {
		if contentrating.Allowed(t.ContentRating, user.MaxContentRating) {
			out = append(out, t)
		}
	}
	return out
}
