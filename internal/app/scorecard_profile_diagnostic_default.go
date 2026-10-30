//go:build !scorecard

package app

import "github.com/braedonsaunders/ether/internal/sessions"

func scorecardProfileDiagnostic(*App) func(sessions.ProfileDiagnostic) { return nil }
