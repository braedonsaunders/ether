//go:build scorecard && !plexparity

package app

import "github.com/braedonsaunders/ether/internal/sessions"

// scorecardProfileDiagnostic admits only the broker's closed diagnostic
// categories. The disposable fixture must isolate requests before using these
// process-wide observations as evidence.
func scorecardProfileDiagnostic(a *App) func(sessions.ProfileDiagnostic) {
	return func(d sessions.ProfileDiagnostic) {
		category := ""
		switch d {
		case sessions.ProfileRequiredConditionUnknown:
			category = "required_condition_unknown"
		case sessions.ProfileDirectRejected:
			category = "direct_rejected"
		case sessions.ProfileDeliveryRejected:
			category = "delivery_rejected"
		case sessions.ProfileFallbackTranscode:
			category = "fallback_transcode"
		case sessions.ProfileFallbackRemux:
			category = "fallback_remux"
		case sessions.ProfileNoDelivery:
			category = "no_delivery"
		}
		if category != "" {
			a.log.Info("scorecard profile decision", "category", category)
		}
	}
}
