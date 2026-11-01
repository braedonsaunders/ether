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
	if device.ID == "" {
		return nil, time.Time{}, core.ErrUnauthorized
	}
	viewer, _, expires, err := g.ResolveGrantDevice(ctx, surface, credential, device.ID)
	return viewer, expires, err
}

// ResolveGrantDevice returns the verified stored device binding. An omitted
// claim is allowed; a supplied claim must match. No grant or activity writes
// occur, and missing device bindings fail closed.
func (g *Grants) ResolveGrantDevice(ctx context.Context, surface, credential, claim string) (*core.Viewer, core.Device, time.Time, error) {
	hash, ok := grantHash(credential)
	if !ok || surface == "" {
		return nil, core.Device{}, time.Time{}, core.ErrUnauthorized
	}
	var id, expires int64
	var owner int
	var device core.Device
	err := g.db.QueryRowContext(ctx, `SELECT g.viewer_id,u.is_owner,g.expires,g.device_id,g.client,g.product,g.version FROM identity_grants g JOIN auth_users u ON u.id=g.viewer_id
 WHERE g.hash=? AND g.surface=? AND g.device_id<>'' AND (?='' OR g.device_id=?) AND g.revoked=0 AND g.expires>? AND u.active=1 AND (u.origin<>'dlna_guest' OR g.surface='dlna')`, hash, surface, claim, claim, g.now().UnixMilli()).Scan(&id, &owner, &expires, &device.ID, &device.Client, &device.Product, &device.Version)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, core.Device{}, time.Time{}, core.ErrUnauthorized
	}
	if err != nil {
		return nil, core.Device{}, time.Time{}, err
	}
	viewer, err := g.viewers.ActiveUser(ctx, id)
	if err != nil {
		return nil, core.Device{}, time.Time{}, err
	}
	if viewer == nil || viewer.ID != id {
		return nil, core.Device{}, time.Time{}, core.ErrUnauthorized
	}
	copyViewer := *viewer
	copyViewer.IsOwner = owner == 1
	return &copyViewer, device, time.UnixMilli(expires).UTC(), nil
}
