package identity

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
)

// GrantScope revalidates access and returns the stable refresh-family binding.
func (g *Grants) GrantScope(ctx context.Context, surface, token string, device core.Device) (string, error) {
	if _, _, err := g.ResolveDevice(ctx, surface, token, device); err != nil {
		return "", err
	}
	digest, ok := grantHash(token)
	if !ok {
		return "", core.ErrUnauthorized
	}
	now := g.now().UnixMilli()
	rows, err := g.db.QueryContext(ctx, `SELECT id FROM identity_refresh_families WHERE access_hash=? AND surface=? AND device_id=? AND revoked=0 AND absolute_expiry>? AND idle_expiry>? LIMIT 2`, digest, surface, device.ID, now, now)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	scope := ""
	for rows.Next() {
		if scope != "" {
			return "", core.ErrUnauthorized
		}
		if err = rows.Scan(&scope); err != nil {
			return "", err
		}
	}
	if err = rows.Err(); err != nil {
		return "", err
	}
	if scope == "" {
		return "", core.ErrUnauthorized
	}
	return scope, nil
}

// GrantScopeCurrent follows valid rotations, while rejecting expired families,
// current-grant logout/device revocation and inactive or restricted viewers.
func (g *Grants) GrantScopeCurrent(ctx context.Context, surface, scope string, device core.Device) (*core.Viewer, error) {
	if len(scope) != 45 || !strings.HasPrefix(scope, "f_") || surface == "" || device.ID == "" {
		return nil, core.ErrUnauthorized
	}
	now := g.now().UnixMilli()
	var viewerID int64
	err := g.db.QueryRowContext(ctx, `SELECT f.viewer_id FROM identity_refresh_families f JOIN identity_grants g ON g.hash=f.access_hash AND g.viewer_id=f.viewer_id AND g.surface=f.surface AND g.device_id=f.device_id JOIN auth_users u ON u.id=f.viewer_id WHERE f.id=? AND f.surface=? AND f.device_id=? AND f.revoked=0 AND f.absolute_expiry>? AND f.idle_expiry>? AND g.revoked=0 AND g.expires>? AND u.active=1 AND (u.origin<>'dlna_guest' OR f.surface='dlna')`, scope, surface, device.ID, now, now, now).Scan(&viewerID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, core.ErrUnauthorized
	}
	if err != nil {
		return nil, err
	}
	return g.viewers.ActiveUser(ctx, viewerID)
}
