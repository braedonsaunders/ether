package auth

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	"time"
)

// SetOwnerEnabled changes the owner's membership decision. Plex sharing
// remains independently authoritative and can keep a viewer inactive.
func (s *Service) SetOwnerEnabled(ctx context.Context, viewerID int64, enabled bool) error {
	s.membershipMu.Lock()
	defer s.membershipMu.Unlock()
	state := 0
	if enabled {
		state = 1
	}
	res, err := s.db.ExecContext(ctx, `UPDATE auth_users SET owner_enabled=?, active=CASE WHEN ?=1 AND (plex_id IS NULL OR plex_shared=1) THEN 1 ELSE 0 END,updated_at=? WHERE id=? AND is_owner=0`, state, state, time.Now().Unix(), viewerID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return core.ErrUnauthorized
	}
	if err = s.reload(ctx); err != nil {
		return err
	}
	s.notify("")
	return nil
}
