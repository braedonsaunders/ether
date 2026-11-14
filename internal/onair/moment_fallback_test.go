package onair

import (
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

// A channel can turn on "Upcoming shows" or "Up next" independent of
// whether the active look actually defines that scene (e.g. a custom look
// built before the role existed, like a user-made template that only ships
// ident/break/back). planRaw used to drop the title card outright in that
// case; it must now fall back the same way standby already does.
func TestPlanRawFallsBackWhenTonightSceneMissing(t *testing.T) {
	look := familyLook("clean", "Clean")
	look.ID = "no-tonight"
	var kept []StyleScene
	for _, sc := range look.Scenes {
		if sc.Role != "tonight" && sc.Role != "upnext" {
			kept = append(kept, sc)
		}
	}
	look.Scenes = kept
	s := &Service{looks: map[string]Look{"no-tonight": look}}

	if _, ok := pickScene(look, core.ClipRequest{LookID: "no-tonight", Moment: "tonight"}); ok {
		t.Fatal("fixture still has a tonight scene")
	}

	job, ok := s.planRaw(core.ClipRequest{LookID: "no-tonight", Moment: "tonight"})
	if !ok {
		t.Fatal("expected a fallback scene, got no plan at all")
	}
	if job.Doc.Moment != "ident" {
		t.Fatalf("expected the ident fallback (upnext is also missing), got moment %q", job.Doc.Moment)
	}
}

// With an "upnext" scene available, "tonight" should prefer it over
// skipping straight to "ident".
func TestPlanRawPrefersUpNextOverIdentWhenTonightMissing(t *testing.T) {
	look := familyLook("clean", "Clean")
	look.ID = "upnext-only"
	var kept []StyleScene
	for _, sc := range look.Scenes {
		if sc.Role != "tonight" {
			kept = append(kept, sc)
		}
	}
	look.Scenes = kept
	s := &Service{looks: map[string]Look{"upnext-only": look}}

	job, ok := s.planRaw(core.ClipRequest{LookID: "upnext-only", Moment: "tonight"})
	if !ok {
		t.Fatal("expected a fallback scene, got no plan at all")
	}
	if job.Doc.Moment != "upnext" {
		t.Fatalf("expected the upnext fallback, got moment %q", job.Doc.Moment)
	}
}
