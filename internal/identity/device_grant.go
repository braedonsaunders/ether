package identity

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// ResolveDevice validates the stored grant binding without extending expiry or
// changing last-used/activity data. It returns the actual current expiry.
func (g *Grants) ResolveDevice(ctx context.Context, surface, credential string, device core.Device) (*core.Viewer, time.Time, error) {
	hash, ok := grantHash(credential)
	if !ok || surface == "" || device.ID == "" {
		return nil, time.Time{}, core.ErrUnauthorized
	}
	var id, expires int64
	var owner int
	err := g.db.QueryRowContext(ctx, `SELECT g.viewer_id,u.is_owner,g.expires FROM identity_grants g JOIN auth_users u ON u.id=g.viewer_id
 WHERE g.hash=? AND g.surface=? AND g.device_id=? AND g.device_id<>'' AND g.revoked=0 AND g.expires>? AND u.active=1 AND (u.origin<>'dlna_guest' OR g.surface='dlna')`, hash, surface, device.ID, g.now().UnixMilli()).Scan(&id, &owner, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, time.Time{}, core.ErrUnauthorized
	}
	if err != nil {
		return nil, time.Time{}, err
	}
	viewer, err := g.viewers.ActiveUser(ctx, id)
	if err != nil {
		return nil, time.Time{}, err
	}
	if viewer == nil || viewer.ID != id {
		return nil, time.Time{}, core.ErrUnauthorized
	}
	copyViewer := *viewer
	copyViewer.IsOwner = owner == 1
	return &copyViewer, time.UnixMilli(expires).UTC(), nil
}
