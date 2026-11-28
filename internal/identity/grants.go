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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/braedonsaunders/ether/internal/auth"
	"github.com/braedonsaunders/ether/internal/config"
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

// Grants authenticates using credential hashes in the shared database. Xtream
// passwords are also sealed for owner recovery. Each grant is scoped to one
// surface and expires after a sliding period of inactivity.
type Grants struct {
	sealer         config.FieldSealer
	db             *sql.DB
	viewers        ActiveViewer
	times          *ViewerTimes
	ttl            time.Duration
	now            func() time.Time
	gen            atomic.Uint64
	touchMu        sync.Mutex
	touchPending   map[grantTouchKey]grantTouch
	touchGen       uint64
	touchRunning   atomic.Bool
	onRevoked      atomic.Pointer[func(viewerID int64, grantKind, reason string)]
	onDeviceIssued atomic.Pointer[func(viewerID int64, surface string, device core.Device, isFirstSeen bool)]
}

// SetOnRevoked installs an optional non-blocking observer for grant revocations.
func (g *Grants) SetOnRevoked(fn func(viewerID int64, grantKind, reason string)) {
	if g == nil {
		return
	}
	if fn == nil {
		g.onRevoked.Store(nil)
		return
	}
	g.onRevoked.Store(&fn)
}

func (g *Grants) emitRevoked(viewerID int64, grantKind, reason string) {
	if g == nil {
		return
	}
	if fn := g.onRevoked.Load(); fn != nil && *fn != nil {
		(*fn)(viewerID, grantKind, reason)
	}
}

// SetOnDeviceIssued installs an optional non-blocking observer notified when
// a grant is issued for a device.
func (g *Grants) SetOnDeviceIssued(fn func(viewerID int64, surface string, device core.Device, isFirstSeen bool)) {
	if g == nil {
		return
	}
	if fn == nil {
		g.onDeviceIssued.Store(nil)
		return
	}
	g.onDeviceIssued.Store(&fn)
}

// Generation changes when a credential is revoked.
func (g *Grants) Generation() uint64 {
	if g == nil {
		return 0
	}
	return g.gen.Load()
}

func NewGrants(db *sql.DB, viewers ActiveViewer, ttl time.Duration, now func() time.Time, sealers ...config.FieldSealer) (*Grants, error) {
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
	g := &Grants{db: db, viewers: viewers, times: times, ttl: ttl, now: now}
	if len(sealers) > 0 {
		g.sealer = sealers[0]
	}
	return g, nil
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
	return g.issue(ctx, viewerID, identityID, surface, device, grantIssue{})
}

// IssueLogin atomically issues a grant and records a successful explicit sign-in.
func (g *Grants) IssueLogin(ctx context.Context, viewerID int64, identityID *int64, surface string, device core.Device) (string, error) {
	return g.issue(ctx, viewerID, identityID, surface, device, grantIssue{login: true})
}

// IssueOwnerLogin admits only an active owner and records the sign-in atomically.
func (g *Grants) IssueOwnerLogin(ctx context.Context, viewerID int64, identityID *int64, surface string, device core.Device) (string, error) {
	return g.issue(ctx, viewerID, identityID, surface, device, grantIssue{login: true, ownerOnly: true})
}

// IssueOIDCLogin verifies that the link still belongs to the viewer while
// issuing the browser grant. Owners need an explicitly confirmed link on
// both browser surfaces.
func (g *Grants) IssueOIDCLogin(ctx context.Context, viewerID int64, identityID *int64, surface string, device core.Device) (string, error) {
	if identityID == nil || (surface != "owner" && surface != "viewer-portal") {
		return "", core.ErrUnauthorized
	}
	return g.issue(ctx, viewerID, identityID, surface, device, grantIssue{login: true, ownerOnly: surface == "owner", oidc: true})
}

// ViewerTimes returns the database-backed event observer owned by this grant service.
func (g *Grants) ViewerTimes() *ViewerTimes { return g.times }

type grantIssue struct {
	login, ownerOnly, oidc                bool
	token, xtreamUsername, sealedPassword string
}

func (g *Grants) issue(ctx context.Context, viewerID int64, identityID *int64, surface string, device core.Device, opts grantIssue) (string, error) {
	if surface == "" || viewerID <= 0 {
		return "", errors.New("identity: surface and viewer required")
	}
	if _, err := g.viewers.ActiveUser(ctx, viewerID); err != nil {
		return "", err
	}
	token := opts.token
	if token == "" {
		var raw [32]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return "", err
		}
		token = grantPrefix + base64.RawURLEncoding.EncodeToString(raw[:])
	}
	hash, _ := grantHash(token)
	now := g.now().UnixMilli()
	var exec interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	} = g.db
	var tx *sql.Tx
	var err error
	if opts.login {
		tx, err = g.db.BeginTx(ctx, nil)
		if err != nil {
			return "", err
		}
		defer tx.Rollback()
		exec = tx
	}
	if opts.oidc {
		if err = lockViewer(ctx, tx, viewerID); err != nil {
			return "", err
		}
		var owner, approved bool
		err = tx.QueryRowContext(ctx, `SELECT u.is_owner,i.oidc_owner_approved FROM identity_links i JOIN auth_users u ON u.id=i.viewer_id
			WHERE i.id=? AND i.viewer_id=? AND i.provider=?`, identityID, viewerID, OIDCProvider).Scan(&owner, &approved)
		if errors.Is(err, sql.ErrNoRows) {
			return "", core.ErrUnauthorized
		}
		if err != nil {
			return "", err
		}
		if owner && !approved {
			return "", ErrOIDCOwnerApproval
		}
	}
	result, err := exec.ExecContext(ctx, `INSERT INTO identity_grants(hash,viewer_id,identity_id,surface,device_id,client,product,version,created,last_used,expires,xtream_username,xtream_password) SELECT ?,?,?,?,?,?,?,?,?,?,?,?,? WHERE EXISTS (SELECT 1 FROM auth_users WHERE id=? AND active=1 AND (?=0 OR is_owner=1) AND (origin<>'dlna_guest' OR ?='dlna')) ON CONFLICT(xtream_username) DO NOTHING`,
		hash, viewerID, identityID, surface, device.ID, device.Client, device.Product, device.Version, now, now, now+g.ttl.Milliseconds(), nullableXtreamUsername(opts.xtreamUsername), opts.sealedPassword, viewerID, store.BoolInt(opts.ownerOnly), surface)
	if err != nil {
		return "", err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return "", err
	}
	if n != 1 {
		if opts.xtreamUsername != "" {
			return "", errXtreamNameUnavailable
		}
		return "", core.ErrUnauthorized
	}
	if opts.login {
		if err := recordLogin(ctx, tx, viewerID, now); err != nil {
			return "", err
		}
		if err := tx.Commit(); err != nil {
			return "", err
		}
	}
	if fn := g.onDeviceIssued.Load(); fn != nil && *fn != nil && strings.TrimSpace(device.ID) != "" {
		var prior int
		_ = g.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM identity_grants WHERE viewer_id=? AND device_id=? AND hash<>?`, viewerID, device.ID, hash).Scan(&prior)
		(*fn)(viewerID, surface, device, prior == 0)
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
	return g.resolve(ctx, surface, credential, surface == "stremio" || surface == "xtream" || surface == "viewer-portal")
}

func (g *Grants) resolve(ctx context.Context, surface, credential string, asyncActivity bool) (*core.Viewer, error) {
	hash, ok := grantHash(credential)
	if !ok || surface == "" {
		return nil, core.ErrUnauthorized
	}
	now := g.now().UnixMilli()
	var stored string
	var id, lastUsed, expires int64
	var owner int
	// Expiry is checked in Go, not SQL: a queued sliding refresh that
	// has not reached the writer already extends this grant (#648). Read
	// the queue before the row: the drainer drops a touch only after its
	// write commits, so a touch missing from the queue is in the row.
	touch, pending := g.pendingTouch(grantTouchKey{hash: hash, surface: surface})
	err := g.db.QueryRowContext(ctx, `SELECT g.hash,g.viewer_id,u.is_owner,g.last_used,g.expires FROM identity_grants g JOIN auth_users u ON u.id=g.viewer_id
		WHERE g.hash=? AND g.surface=? AND g.revoked=0 AND u.active=1 AND (u.origin<>'dlna_guest' OR g.surface='dlna')
		AND (u.is_owner=0 OR NOT EXISTS(SELECT 1 FROM identity_links i
			WHERE i.id=g.identity_id AND i.viewer_id=g.viewer_id AND i.provider=? AND i.oidc_owner_approved=0))`,
		hash, surface, OIDCProvider).Scan(&stored, &id, &owner, &lastUsed, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, core.ErrUnauthorized
	}
	if err != nil {
		return nil, err
	}
	if pending {
		if touch.now > lastUsed {
			lastUsed = touch.now
		}
		if touch.expires > expires {
			expires = touch.expires
		}
	}
	if expires <= now {
		return nil, core.ErrUnauthorized
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
	window := int64((30 * time.Second).Milliseconds())
	if now-lastUsed >= window || expires < now+g.ttl.Milliseconds()-window {
		// Sliding expiry is bookkeeping. Queue it for the coalescing
		// drainer so a valid grant never waits on the SQLite writer (#648).
		g.enqueueTouch(
			grantTouchKey{hash: hash, surface: surface},
			grantTouch{viewerID: id, now: now, expires: now + g.ttl.Milliseconds()},
		)
	}
	if !frontend.QuietIdentity(ctx) {
		if asyncActivity {
			g.times.observeAsync(viewer.ID)
		} else {
			_ = g.times.Observe(ctx, viewer.ID)
		}
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
	queued := g.queuedExpiry(reference, surface)
	var id, expires int64
	err = g.db.QueryRowContext(ctx, `SELECT g.viewer_id,g.expires FROM identity_grants g JOIN auth_users u ON u.id=g.viewer_id WHERE g.hash=? AND g.surface=? AND g.viewer_id=? AND g.revoked=0 AND u.active=1 AND (u.origin<>'dlna_guest' OR g.surface='dlna')`, reference, surface, viewerID).Scan(&id, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, core.ErrUnauthorized
	}
	if err != nil {
		return time.Time{}, err
	}
	if queued > expires {
		expires = queued
	}
	if expires <= g.now().UnixMilli() {
		return time.Time{}, core.ErrUnauthorized
	}
	viewer, err := g.viewers.ActiveUser(ctx, id)
	if err != nil {
		return time.Time{}, err
	}
	if viewer == nil || viewer.ID != viewerID {
		return time.Time{}, core.ErrUnauthorized
	}
	if !frontend.QuietIdentity(ctx) {
		_ = g.times.Observe(ctx, viewer.ID)
	}
	return time.UnixMilli(expires), nil
}

// RevokeGrant accepts the exact stored identifier returned by List. A prior
// access grant still identifies its family if the session rotated after listing.
func (g *Grants) RevokeGrant(ctx context.Context, viewerID int64, hash string) error {
	if hash == "" {
		return errors.New("identity: grant identifier required")
	}
	_, err := g.revoke(ctx, viewerID, "grant", "revoked", "hash=?", "id IN (SELECT family_id FROM identity_refresh_tokens WHERE access_hash=?)", hash)
	return err
}

func (g *Grants) RevokeDevice(ctx context.Context, viewerID int64, deviceID string) error {
	if deviceID == "" {
		return errors.New("identity: device required")
	}
	_, err := g.revoke(ctx, viewerID, "device", "revoked", "device_id=?", "device_id=?", deviceID)
	return err
}

func (g *Grants) RevokeViewer(ctx context.Context, viewerID int64) error {
	_, err := g.revoke(ctx, viewerID, "viewer", "revoked", "1=1", "1=1")
	return err
}

// SignOutViewer revokes every live device grant (including app passwords)
// and refresh family the viewer holds, returning how many were revoked.
func (g *Grants) SignOutViewer(ctx context.Context, viewerID int64) (int, error) {
	return g.revoke(ctx, viewerID, "viewer", "sign_out", "1=1", "1=1")
}

// The filters are internal SQL predicates with bound values. The viewer
// fence serializes this mutation with family creation and rotation.
func (g *Grants) revoke(ctx context.Context, viewerID int64, kind, reason, grantFilter, familyFilter string, filterArgs ...any) (int, error) {
	tx, err := g.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if err = lockViewer(ctx, tx, viewerID); err != nil {
		return 0, err
	}
	args := append([]any{viewerID}, filterArgs...)
	families, err := tx.ExecContext(ctx, `UPDATE identity_refresh_families SET revoked=1 WHERE viewer_id=? AND revoked=0 AND (`+familyFilter+`)`, args...)
	if err != nil {
		return 0, err
	}
	grants, err := tx.ExecContext(ctx, `UPDATE identity_grants SET revoked=1 WHERE viewer_id=? AND revoked=0 AND ((`+grantFilter+`) OR hash IN
		(SELECT access_hash FROM identity_refresh_families WHERE viewer_id=? AND (`+familyFilter+`)))`, append(args, args...)...)
	if err != nil {
		return 0, err
	}
	familyCount, err := families.RowsAffected()
	if err != nil {
		return 0, err
	}
	grantCount, err := grants.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	total := int(familyCount + grantCount)
	if total > 0 {
		g.gen.Add(1)
		g.emitRevoked(viewerID, kind, reason)
	}
	return total, nil
}
