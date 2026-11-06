package resolver

import "testing"

// A trace recorded by an older build carries the "height" wording; the checks
// that classify a rejection must treat it like the current wording.
func TestNormalizeReasonAcceptsTheEarlierHeightWording(t *testing.T) {
	for old, want := range map[string]string{
		"above profile max height": ReasonAboveProfileMaxResolution,
		"above client max height":  ReasonAboveClientMaxResolution,
		"below profile min height": ReasonBelowProfileMinResolution,
		// Current wording and unrelated reasons are unchanged.
		ReasonAboveProfileMaxResolution:       ReasonAboveProfileMaxResolution,
		"dolby vision not allowed for client": "dolby vision not allowed for client",
		"":                                    "",
	} {
		if got := NormalizeReason(old); got != want {
			t.Errorf("NormalizeReason(%q) = %q, want %q", old, got, want)
		}
	}
	for _, reason := range []string{"above profile max height", "above client max height", ReasonAboveProfileMaxResolution, ReasonAboveClientMaxResolution} {
		if !ceilingRejection(reason) {
			t.Errorf("ceilingRejection(%q) = false, want true", reason)
		}
	}
	if ceilingRejection("below profile min height") || ceilingRejection(ReasonBelowProfileMinResolution) {
		t.Error("a minimum-resolution rejection is not a ceiling rejection")
	}
}
