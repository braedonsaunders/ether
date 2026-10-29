package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.RegisterAfter("identity_grants", "auth")
	store.Register("identity_grants", 1, `
CREATE TABLE identity_grants (
 hash TEXT PRIMARY KEY,
 viewer_id INTEGER NOT NULL REFERENCES auth_users(id) ON DELETE CASCADE,
 identity_id INTEGER,
 surface TEXT NOT NULL,
 device_id TEXT NOT NULL,
 client TEXT NOT NULL,
 product TEXT NOT NULL,
 version TEXT NOT NULL,
 created INTEGER NOT NULL,
 last_used INTEGER NOT NULL,
 expires INTEGER NOT NULL,
 revoked INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX identity_grants_viewer ON identity_grants(viewer_id);
CREATE INDEX identity_grants_device ON identity_grants(viewer_id, device_id);
`)
}

const grantPrefix = core.GrantCredentialPrefix

// ActiveViewer must return only currently active household members.
type ActiveViewer interface {
	ActiveUser(context.Context, int64) (*core.User, error)
}

// Grants stores only hashes of credentials in the shared database. Each grant
// is scoped to one surface and expires after a sliding period of inactivity.
type Grants struct {
	db      *sql.DB
	viewers ActiveViewer
	ttl     time.Duration
	now     func() time.Time
}

func NewGrants(db *sql.DB, viewers ActiveViewer, ttl time.Duration, now func() time.Time) (*Grants, error) {
	if db == nil || viewers == nil || ttl < time.Second {
		return nil, errors.New("identity: database, active viewer lookup and positive TTL required")
	}
	if now == nil {
		now = time.Now
	}
	return &Grants{db: db, viewers: viewers, ttl: ttl, now: now}, nil
}

// IsGrantCredential distinguishes Ether grants from credentials belonging to
// external providers. Plex authentication must reject this prefix outright.
func IsGrantCredential(credential string) bool { return core.IsGrantCredential(credential) }

func grantHash(credential string) (string, bool) {
	if !IsGrantCredential(credential) {
		return "", false
	}
	encoded := strings.TrimPrefix(credential, grantPrefix)
	b, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(b) != 32 || base64.RawURLEncoding.EncodeToString(b) != encoded {
		return "", false
	}
	h := sha256.Sum256([]byte(credential))
	return hex.EncodeToString(h[:]), true
}

// Issue creates a new independent credential for a device and surface.
func (g *Grants) Issue(ctx context.Context, viewerID int64, identityID *int64, surface string, device core.Device) (string, error) {
	if surface == "" || viewerID <= 0 {
		return "", errors.New("identity: surface and viewer required")
	}
	if _, err := g.viewers.ActiveUser(ctx, viewerID); err != nil {
		return "", err
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	token := grantPrefix + base64.RawURLEncoding.EncodeToString(raw[:])
	hash, _ := grantHash(token)
	now := g.now().UnixMilli()
	result, err := g.db.ExecContext(ctx, `INSERT INTO identity_grants(hash,viewer_id,identity_id,surface,device_id,client,product,version,created,last_used,expires) SELECT ?,?,?,?,?,?,?,?,?,?,? WHERE EXISTS (SELECT 1 FROM auth_users WHERE id=? AND active=1)`, hash, viewerID, identityID, surface, device.ID, device.Client, device.Product, device.Version, now, now, now+g.ttl.Milliseconds(), viewerID)
	if err != nil {
		return "", err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return "", err
	}
	if n != 1 {
		return "", core.ErrUnauthorized
	}
	return token, nil
}

// Resolve rejects malformed, expired, revoked, inactive and cross-surface grants.
func (g *Grants) Resolve(ctx context.Context, surface, credential string) (*core.Viewer, error) {
	hash, ok := grantHash(credential)
	if !ok || surface == "" {
		return nil, core.ErrUnauthorized
	}
	now := g.now().UnixMilli()
	var stored string
	var id int64
	var owner int
	err := g.db.QueryRowContext(ctx, `SELECT g.hash,g.viewer_id,u.is_owner FROM identity_grants g JOIN auth_users u ON u.id=g.viewer_id WHERE g.hash=? AND g.surface=? AND g.revoked=0 AND g.expires>? AND u.active=1`, hash, surface, now).Scan(&stored, &id, &owner)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, core.ErrUnauthorized
	}
	if err != nil {
		return nil, err
	}
	if subtle.ConstantTimeCompare([]byte(stored), []byte(hash)) != 1 {
		return nil, core.ErrUnauthorized
	}
	viewer, err := g.viewers.ActiveUser(ctx, id)
	if err != nil {
		return nil, err
	}
	// Owner privileges follow the shared database even if a node's viewer
	// cache has not yet received a role-change invalidation.
	copyViewer := *viewer
	copyViewer.IsOwner = owner == 1
	viewer = &copyViewer
	res, err := g.db.ExecContext(ctx, `UPDATE identity_grants SET last_used=?,expires=? WHERE hash=? AND surface=? AND revoked=0 AND expires>? AND EXISTS (SELECT 1 FROM auth_users WHERE id=? AND active=1)`, now, now+g.ttl.Milliseconds(), hash, surface, now, id)
	if err != nil {
		return nil, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if n != 1 {
		return nil, core.ErrUnauthorized
	}
	return viewer, nil
}

// RevokeGrant, RevokeDevice and RevokeViewer invalidate credentials without
// deleting audit metadata. Only a credential's hash is needed for revocation.
func (g *Grants) RevokeGrant(ctx context.Context, viewerID int64, hash string) error {
	if len(hash) != 64 {
		return fmt.Errorf("identity: invalid grant hash")
	}
	_, err := g.db.ExecContext(ctx, `UPDATE identity_grants SET revoked=1 WHERE viewer_id=? AND hash=?`, viewerID, hash)
	return err
}
func (g *Grants) RevokeDevice(ctx context.Context, viewerID int64, deviceID string) error {
	if deviceID == "" {
		return errors.New("identity: device required")
	}
	_, err := g.db.ExecContext(ctx, `UPDATE identity_grants SET revoked=1 WHERE viewer_id=? AND device_id=?`, viewerID, deviceID)
	return err
}
func (g *Grants) RevokeViewer(ctx context.Context, viewerID int64) error {
	_, err := g.db.ExecContext(ctx, `UPDATE identity_grants SET revoked=1 WHERE viewer_id=?`, viewerID)
	return err
}
