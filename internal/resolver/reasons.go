package resolver

// Rejection reasons for the resolution ceiling and floor. These strings are
// shown in decision traces and the explain output, and a trace or scorecard
// recorded by an older build may carry the earlier "height" wording, so the
// matchers accept both (NormalizeReason).
const (
	ReasonAboveProfileMaxResolution = "above profile max resolution"
	ReasonAboveClientMaxResolution  = "above client max resolution"
	ReasonBelowProfileMinResolution = "below profile min resolution"
)

// legacyReasons maps the earlier "height" wording to the current reason.
var legacyReasons = map[string]string{
	"above profile max height": ReasonAboveProfileMaxResolution,
	"above client max height":  ReasonAboveClientMaxResolution,
	"below profile min height": ReasonBelowProfileMinResolution,
}

// NormalizeReason returns the current spelling of a rejection reason, so a
// trace written by an older build matches the same checks as a new one.
// Reasons it does not know are returned unchanged.
func NormalizeReason(reason string) string {
	if current, ok := legacyReasons[reason]; ok {
		return current
	}
	return reason
}
