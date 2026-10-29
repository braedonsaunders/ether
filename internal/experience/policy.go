// Package experience contains protocol-independent viewer experience logic.
package experience

import (
	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/core"
)

// AccessPolicy adds owner-defined restrictions to the mandatory title guard.
type AccessPolicy interface {
	Allowed(*core.User, *core.Title) bool
}

// TitleAllowed applies the mandatory content guards and the owner's policy.
func TitleAllowed(policy AccessPolicy, viewer *core.User, title *core.Title) bool {
	if title == nil || title.Adult {
		return false
	}
	if viewer != nil && catalog.RatingExceeds(title.ContentRating, viewer.MaxContentRating) {
		return false
	}
	return policy == nil || policy.Allowed(viewer, title)
}

// SectionAllowed applies a viewer's library restrictions to an existing section.
// An empty restriction list means all sections, as it does for an owner.
func SectionAllowed(viewer *core.User, id int) bool {
	if viewer == nil || viewer.IsOwner || len(viewer.AllowedSections) == 0 {
		return true
	}
	for _, allowed := range viewer.AllowedSections {
		if allowed == id {
			return true
		}
	}
	return false
}
