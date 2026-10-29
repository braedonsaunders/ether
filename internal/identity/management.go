package identity

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	"strconv"
	"time"
)

type LinkedIdentity struct {
	ID       int64  `json:"id"`
	Provider string `json:"provider"`
	Display  string `json:"display"`
}

func (l *Local) Links(ctx context.Context, viewerID int64) ([]LinkedIdentity, error) {
	rows, err := l.db.QueryContext(ctx, `SELECT id,provider,display FROM identity_links WHERE viewer_id=? ORDER BY id`, viewerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []LinkedIdentity{}
	for rows.Next() {
		var v LinkedIdentity
		if err = rows.Scan(&v.ID, &v.Provider, &v.Display); err != nil {
			return nil, err
		}
		result = append(result, v)
	}
	return result, rows.Err()
}
func (l *Local) UpsertPassword(ctx context.Context, viewerID int64, username, password string) error {
	var count int
	if err := l.db.QueryRowContext(ctx, `SELECT count(*) FROM identity_links WHERE viewer_id=? AND provider='local' AND subject=?`, viewerID, username).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return l.SetPassword(ctx, viewerID, username, password)
	}
	return l.Create(ctx, viewerID, username, password)
}

type GrantInfo struct {
	Hash     string `json:"hash"`
	Surface  string `json:"surface"`
	DeviceID string `json:"deviceId"`
	Client   string `json:"client"`
	Product  string `json:"product"`
	Version  string `json:"version"`
	LastUsed int64  `json:"lastUsed"`
}

func (g *Grants) List(ctx context.Context, viewerID int64) ([]GrantInfo, error) {
	rows, err := g.db.QueryContext(ctx, `SELECT hash,surface,device_id,client,product,version,last_used FROM identity_grants WHERE viewer_id=? AND revoked=0 AND expires>? ORDER BY last_used DESC`, viewerID, g.now().UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []GrantInfo{}
	for rows.Next() {
		var v GrantInfo
		if err = rows.Scan(&v.Hash, &v.Surface, &v.DeviceID, &v.Client, &v.Product, &v.Version, &v.LastUsed); err != nil {
			return nil, err
		}
		result = append(result, v)
	}
	return result, rows.Err()
}

// LinkPlex records an already verified Plex identity. A provider subject may
// belong to only one viewer; conflicts never move it between viewers.
func (l *Local) LinkPlex(ctx context.Context, viewerID, plexID int64, display string) error {
	if viewerID <= 0 || plexID <= 0 {
		return core.ErrUnauthorized
	}
	subject := strconv.FormatInt(plexID, 10)
	_, err := l.db.ExecContext(ctx, `INSERT INTO identity_links(viewer_id,provider,subject,display,verified_at) VALUES(?,'plex',?,?,?) ON CONFLICT(provider,subject) DO NOTHING`, viewerID, subject, display, time.Now().Unix())
	if err != nil {
		return err
	}
	var existing int64
	if err = l.db.QueryRowContext(ctx, `SELECT viewer_id FROM identity_links WHERE provider='plex' AND subject=?`, subject).Scan(&existing); err != nil {
		return err
	}
	if existing != viewerID {
		return core.ErrUnauthorized
	}
	return nil
}
