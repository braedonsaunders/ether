package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.RegisterAfter("identity_web_codes", "auth")
	store.Register("identity_web_codes", 2, `ALTER TABLE identity_web_codes ADD COLUMN surface TEXT NOT NULL DEFAULT ''; ALTER TABLE identity_web_codes ADD COLUMN origin TEXT NOT NULL DEFAULT '';`)
	store.Register("identity_web_codes", 1, `CREATE TABLE identity_web_codes (hash TEXT PRIMARY KEY, viewer_id INTEGER NOT NULL REFERENCES auth_users(id) ON DELETE CASCADE, expires BIGINT NOT NULL); CREATE INDEX identity_web_codes_expiry ON identity_web_codes(expires);`)
}

// IssueWebCode stores only a digest, scoped to the authenticated console viewer.
func (g *Grants) IssueWebCode(ctx context.Context, viewerID int64) (string, error) {
	return g.issueWebCode(ctx, viewerID, "", "")
}

// IssueOriginWebCode uses the same one-time issuer as the console handoff,
// with an additional exact origin and surface binding.
func (g *Grants) IssueOriginWebCode(ctx context.Context, viewerID int64, surface, origin string) (string, error) {
	if surface != "vela" || origin == "" {
		return "", core.ErrUnauthorized
	}
	return g.issueWebCode(ctx, viewerID, surface, origin)
}
func (g *Grants) issueWebCode(ctx context.Context, viewerID int64, surface, origin string) (string, error) {
	if _, err := g.viewers.ActiveUser(ctx, viewerID); err != nil {
		return "", err
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	code := base64.RawURLEncoding.EncodeToString(raw[:])
	hash := sha256.Sum256([]byte(code))
	now := g.now().UnixMilli()
	if _, err := g.db.ExecContext(ctx, `DELETE FROM identity_web_codes WHERE expires<=? OR viewer_id=?`, now, viewerID); err != nil {
		return "", err
	}
	_, err := g.db.ExecContext(ctx, `INSERT INTO identity_web_codes(hash,viewer_id,expires,surface,origin) VALUES(?,?,?,?,?)`, hex.EncodeToString(hash[:]), viewerID, now+time.Minute.Milliseconds(), surface, origin)
	return code, err
}

// RedeemWebCode atomically consumes a code, then issues an ordinary revocable
// Jellyfin device grant. An existing matching grant avoids device churn.
func (g *Grants) RedeemWebCode(ctx context.Context, code, browser, existing string) (string, int64, error) {
	return g.RedeemDeviceWebCode(ctx, code, "jellyfin", core.Device{ID: browser, Client: "Jellyfin Web", Product: "Ether web player"}, existing)
}

// RedeemDeviceWebCode consumes a console handoff for a surface-bound device grant.
func (g *Grants) RedeemDeviceWebCode(ctx context.Context, code, surface string, dev core.Device, existing string) (string, int64, error) {
	return g.redeemOriginWebCode(ctx, code, surface, dev, existing, "")
}

func (g *Grants) RedeemOriginWebCode(ctx context.Context, code, surface string, dev core.Device, origin string) (string, int64, error) {
	return g.redeemOriginWebCode(ctx, code, surface, dev, "", origin)
}

func (g *Grants) redeemOriginWebCode(ctx context.Context, code, surface string, dev core.Device, existing, origin string) (string, int64, error) {
	browser := dev.ID
	if surface == "" {
		return "", 0, core.ErrUnauthorized
	}
	if len(code) != 43 || len(browser) < 16 || len(browser) > 128 {
		return "", 0, core.ErrUnauthorized
	}
	hash := sha256.Sum256([]byte(code))
	key := hex.EncodeToString(hash[:])
	var stored string
	var viewerID int64
	err := g.db.QueryRowContext(ctx, `DELETE FROM identity_web_codes WHERE hash=? AND expires>? AND (surface='' OR (surface=? AND origin=? AND origin<>'')) RETURNING hash,viewer_id`, key, g.now().UnixMilli(), surface, origin).Scan(&stored, &viewerID)
	if err != nil || subtle.ConstantTimeCompare([]byte(stored), []byte(key)) != 1 {
		return "", 0, core.ErrUnauthorized
	}
	if v, _, err := g.ResolveDevice(ctx, surface, existing, dev); err == nil && v.ID == viewerID {
		return existing, viewerID, nil
	}
	if err := g.RevokeDevice(ctx, viewerID, dev.ID); err != nil {
		return "", 0, err
	}
	token, err := g.IssueLogin(ctx, viewerID, nil, surface, dev)
	return token, viewerID, err
}
