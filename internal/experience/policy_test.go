package experience

import (
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

type denyPolicy struct{}

func (denyPolicy) Allowed(*core.User, *core.Title) bool { return false }

func TestTitleAllowed(t *testing.T) {
	viewer := &core.User{}
	if TitleAllowed(nil, viewer, nil) || TitleAllowed(nil, viewer, &core.Title{Adult: true}) {
		t.Fatal("missing or adult titles must be denied")
	}
	title := &core.Title{}
	if !TitleAllowed(nil, viewer, title) || TitleAllowed(denyPolicy{}, viewer, title) {
		t.Fatal("owner policy must restrict otherwise allowed titles")
	}
}

func TestSectionAllowed(t *testing.T) {
	viewer := &core.User{AllowedSections: []int{2}}
	if !SectionAllowed(viewer, 2) || SectionAllowed(viewer, 3) {
		t.Fatal("section restrictions were not applied")
	}
	viewer.IsOwner = true
	if !SectionAllowed(viewer, 3) || !SectionAllowed(nil, 3) || !SectionAllowed(&core.User{}, 3) {
		t.Fatal("owner and unrestricted viewers must see sections")
	}
}
