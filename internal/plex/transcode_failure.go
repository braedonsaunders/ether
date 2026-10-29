package plex

import (
	"context"
	"time"

	"github.com/braedonsaunders/ether/internal/sessions"
)

// RecoverTranscode maps the client's encoder ID to its playback session.
// Playback rechecks the input generation before attempting recovery.
func (s *Server) RecoverTranscode(ctx context.Context, id, inputURL string, at time.Duration) error {
	return sessions.Recover(ctx, s.transcodes.table, s.d.Playback, id, inputURL, at)
}
