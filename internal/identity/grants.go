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
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	_ "github.com/braedonsaunders/ether/internal/auth"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
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
	times   *ViewerTimes
	ttl     time.Duration
	now     func() time.Time
	gen     atomic.Uint64
}

// Generation changes when a credential is revoked.
func (g *Grants) Generation() uint64 {
	if g == nil {
		return 0
	}
	return g.gen.Load()
}

func NewGrants(db *sql.DB, viewers ActiveViewer, ttl time.Duration, now func() time.Time) (*Grants, error) {
	if db == nil || viewers == nil || ttl < time.Second {
		return nil, errors.New("identity: database, active viewer lookup and positive TTL required")
	}
	if now == nil {
		now = time.Now
	}
	times, err := NewViewerTimes(db, now)
	if err != nil {
		return nil, err
	}
	return &Grants{db: db, viewers: viewers, times: times, ttl: ttl, now: now}, nil
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
	return g.issue(ctx, viewerID, identityID, surface, device, false, false)
}

// IssueLogin atomically issues a grant and records a successful explicit sign-in.
func (g *Grants) IssueLogin(ctx context.Context, viewerID int64, identityID *int64, surface string, device core.Device) (string, error) {
	return g.issue(ctx, viewerID, identityID, surface, device, true, false)
}

// IssueOwnerLogin admits only an active owner and records the sign-in atomically.
func (g *Grants) IssueOwnerLogin(ctx context.Context, viewerID int64, identityID *int64, surface string, device core.Device) (string, error) {
	return g.issue(ctx, viewerID, identityID, surface, device, true, true)
}

// ViewerTimes returns the database-backed event observer owned by this grant service.
func (g *Grants) ViewerTimes() *ViewerTimes { return g.times }

func (g *Grants) issue(ctx context.Context, viewerID int64, identityID *int64, surface string, device core.Device, login, ownerOnly bool) (string, error) {
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
	var exec interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	} = g.db
	var tx *sql.Tx
	var err error
	if login {
		tx, err = g.db.BeginTx(ctx, nil)
		if err != nil {
			return "", err
		}
		defer tx.Rollback()
		exec = tx
	}
	ownerFlag := 0
	if ownerOnly {
		ownerFlag = 1
	}
	result, err := exec.ExecContext(ctx, `INSERT INTO identity_grants(hash,viewer_id,identity_id,surface,device_id,client,product,version,created,last_used,expires) SELECT ?,?,?,?,?,?,?,?,?,?,? WHERE EXISTS (SELECT 1 FROM auth_users WHERE id=? AND active=1 AND (?=0 OR is_owner=1) AND (origin<>'dlna_guest' OR ?='dlna'))`, hash, viewerID, identityID, surface, device.ID, device.Client, device.Product, device.Version, now, now, now+g.ttl.Milliseconds(), viewerID, ownerFlag, surface)
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
	if login {
		if err := recordLogin(ctx, tx, viewerID, now); err != nil {
			return "", err
		}
		if err := tx.Commit(); err != nil {
			return "", err
		}
	}
	return token, nil
}

// SignInWithGrant authenticates a client sign-in request that supplies an
// app-password grant credential instead of a local password. It verifies the
// grant is active for surface, checks that username matches the grant's viewer,
// and binds the client device to the grant.
func (g *Grants) SignInWithGrant(ctx context.Context, surface, username, credential string, device core.Device) (string, error) {
	hash, ok := grantHash(credential)
	if !ok || surface == "" {
		return "", core.ErrUnauthorized
	}
	viewer, err := g.Resolve(ctx, surface, credential)
	if err != nil || viewer == nil {
		return "", core.ErrUnauthorized
	}
	if !g.viewerMatchesUsername(ctx, viewer, username) {
		return "", core.ErrUnauthorized
	}
	var stored core.Device
	err = g.db.QueryRowContext(ctx, `SELECT device_id,client,product,version FROM identity_grants WHERE hash=? AND surface=? AND revoked=0`, hash, surface).Scan(&stored.ID, &stored.Client, &stored.Product, &stored.Version)
	if err != nil {
		return "", core.ErrUnauthorized
	}
	client := device.Client
	if client == "" {
		client = stored.Client
	}
	product := device.Product
	if product == "" {
		product = stored.Product
	}
	version := device.Version
	if version == "" {
		version = stored.Version
	}
	if device.ID == "" || stored.ID == "" || stored.ID == device.ID || strings.HasPrefix(stored.ID, surface+"-") {
		deviceID := device.ID
		if deviceID == "" {
			deviceID = stored.ID
		}
		now := g.now().UnixMilli()
		res, err := g.db.ExecContext(ctx, `UPDATE identity_grants SET device_id=?,client=?,product=?,version=?,last_used=?,expires=? WHERE hash=? AND surface=? AND revoked=0 AND expires>?`, deviceID, client, product, version, now, now+g.ttl.Milliseconds(), hash, surface, now)
		if err != nil {
			return "", err
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			return "", core.ErrUnauthorized
		}
		return credential, nil
	}
	return g.Issue(ctx, viewer.ID, nil, surface, core.Device{
		ID:         device.ID,
		DeviceName: device.DeviceName,
		Client:     client,
		Product:    product,
		Version:    version,
	})
}

func (g *Grants) viewerMatchesUsername(ctx context.Context, viewer *core.Viewer, username string) bool {
	name := strings.TrimSpace(username)
	if name == "" || viewer == nil {
		return false
	}
	if strings.EqualFold(name, strings.TrimSpace(viewer.Username)) ||
		strings.EqualFold(name, strings.TrimSpace(viewer.Title)) ||
		name == strconv.FormatInt(viewer.ID, 10) {
		return true
	}
	var linked bool
	_ = g.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM identity_links WHERE viewer_id=? AND (LOWER(subject)=LOWER(?) OR LOWER(display)=LOWER(?)))`, viewer.ID, name, name).Scan(&linked)
	return linked
}

// Resolve rejects malformed, expired, revoked, inactive and cross-surface grants.
func (g *Grants) Resolve(ctx context.Context, surface, credential string) (*core.Viewer, error) {
	hash, ok := grantHash(credential)
	if !ok || surface == "" {
		return nil, core.ErrUnauthorized
	}
	now := g.now().UnixMilli()
	var stored string
	var id, lastUsed, expires int64
	var owner int
	err := g.db.QueryRowContext(ctx, `SELECT g.hash,g.viewer_id,u.is_owner,g.last_used,g.expires FROM identity_grants g JOIN auth_users u ON u.id=g.viewer_id WHERE g.hash=? AND g.surface=? AND g.revoked=0 AND g.expires>? AND u.active=1 AND (u.origin<>'dlna_guest' OR g.surface='dlna')`, hash, surface, now).Scan(&stored, &id, &owner, &lastUsed, &expires)
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
	// The SELECT already rejected a revoked, expired, or inactive grant.
	// Extending a grant that was touched moments ago is bookkeeping, and a
	// play must not take a write lock to do it. Activity is still observed:
	// a fresh grant is always inside this window, and that first resolve is
	// the authenticated request the activity time records.
	window := int64((30 * time.Second).Milliseconds())
	if now-lastUsed < window && expires >= now+g.ttl.Milliseconds()-window {
		if !frontend.QuietIdentity(ctx) {
			_ = g.times.Observe(ctx, viewer.ID)
		}
		return viewer, nil
	}
	res, err := g.db.ExecContext(ctx, `UPDATE identity_grants SET last_used=?,expires=? WHERE hash=? AND surface=? AND revoked=0 AND expires>? AND EXISTS (SELECT 1 FROM auth_users WHERE id=? AND active=1 AND (origin<>'dlna_guest' OR ?='dlna'))`, now, now+g.ttl.Milliseconds(), hash, surface, now, id, surface)
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
	// Activity is an observation, never a reason to reject a valid credential.
	if !frontend.QuietIdentity(ctx) {
		_ = g.times.Observe(ctx, viewer.ID)
	}
	return viewer, nil
}

// GrantReference validates a surface credential and returns an internal,
// non-secret reference plus its active viewer. The reference is not itself an
// authentication credential and must never be exposed to a client.
func (g *Grants) GrantReference(ctx context.Context, surface, credential string) (string, *core.Viewer, error) {
	hash, ok := grantHash(credential)
	if !ok {
		return "", nil, core.ErrUnauthorized
	}
	viewer, err := g.Resolve(ctx, surface, credential)
	if err != nil {
		return "", nil, err
	}
	return hash, viewer, nil
}

// ValidateGrantReference rechecks the shared grant and active membership for
// a scoped capability. Unlike Resolve, it does not extend the grant lifetime.
func (g *Grants) ValidateGrantReference(ctx context.Context, surface, reference string, viewerID int64) error {
	_, err := g.GrantReferenceExpiry(ctx, surface, reference, viewerID)
	return err
}

// GrantReferenceExpiry validates a reference without extending the underlying
// grant and returns its current absolute expiry.
func (g *Grants) GrantReferenceExpiry(ctx context.Context, surface, reference string, viewerID int64) (time.Time, error) {
	decoded, err := hex.DecodeString(reference)
	if err != nil || len(decoded) != sha256.Size || hex.EncodeToString(decoded) != reference || surface == "" || viewerID <= 0 {
		return time.Time{}, core.ErrUnauthorized
	}
	var id, expires int64
	err = g.db.QueryRowContext(ctx, `SELECT g.viewer_id,g.expires FROM identity_grants g JOIN auth_users u ON u.id=g.viewer_id WHERE g.hash=? AND g.surface=? AND g.viewer_id=? AND g.revoked=0 AND g.expires>? AND u.active=1 AND (u.origin<>'dlna_guest' OR g.surface='dlna')`, reference, surface, viewerID, g.now().UnixMilli()).Scan(&id, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, core.ErrUnauthorized
	}
	if err != nil {
		return time.Time{}, err
	}
	viewer, err := g.viewers.ActiveUser(ctx, id)
	if err != nil {
		return time.Time{}, err
	}
	if viewer == nil || viewer.ID != viewerID {
		return time.Time{}, core.ErrUnauthorized
	}
	_ = g.times.Observe(ctx, viewer.ID)
	return time.UnixMilli(expires), nil
}

// RevokeGrant, RevokeDevice and RevokeViewer invalidate credentials without
// deleting audit metadata. Only a credential's hash is needed for revocation.
func (g *Grants) RevokeGrant(ctx context.Context, viewerID int64, hash string) error {
	if len(hash) != 64 {
		return fmt.Errorf("identity: invalid grant hash")
	}
	_, err := g.db.ExecContext(ctx, `UPDATE identity_grants SET revoked=1 WHERE viewer_id=? AND hash=?`, viewerID, hash)
	if err == nil {
		g.gen.Add(1)
	}
	return err
}
func (g *Grants) RevokeDevice(ctx context.Context, viewerID int64, deviceID string) error {
	if deviceID == "" {
		return errors.New("identity: device required")
	}
	_, err := g.db.ExecContext(ctx, `UPDATE identity_grants SET revoked=1 WHERE viewer_id=? AND device_id=?`, viewerID, deviceID)
	if err == nil {
		g.gen.Add(1)
	}
	return err
}
func (g *Grants) RevokeViewer(ctx context.Context, viewerID int64) error {
	_, err := g.db.ExecContext(ctx, `UPDATE identity_grants SET revoked=1 WHERE viewer_id=?`, viewerID)
	if err == nil {
		g.gen.Add(1)
	}
	return err
}
