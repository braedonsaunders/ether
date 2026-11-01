//go:build !scorecard

package app

import "github.com/braedonsaunders/ether/internal/frontend"

func scorecardSessions(_ *App, s frontend.Sessions) frontend.Sessions { return s }
