package identity

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// OIDCProvider is the identity_links provider name for single sign-on
// identities. Their subject joins the issuer and the provider's subject
// claim, so the same person at two providers is two identities.
const OIDCProvider = "oidc"

// ErrIdentityLinked means the provider account already signs in as another
// viewer.
var ErrIdentityLinked = errors.New("identity: provider account is linked to another viewer")

// ErrOIDCOwnerApproval means the viewer needs to confirm their link as owner.
var ErrOIDCOwnerApproval = errors.New("identity: single sign-on link needs owner confirmation")

func validOIDCSubject(subject string) bool {
	return subject != "" && len(subject) <= 1024 && !strings.ContainsAny(subject, "\x00\r\n")
}

// OIDCViewer resolves a verified single sign-on subject to an active member.
// Unknown subjects are refused: linking is the only way to add one.
func (l *Local) OIDCViewer(ctx context.Context, subject string) (viewerID, identityID int64, err error) {
	if !validOIDCSubject(subject) {
		return 0, 0, core.ErrUnauthorized
	}
	err = l.db.QueryRowContext(ctx, `SELECT l.viewer_id,l.id FROM identity_links l JOIN auth_users u ON u.id=l.viewer_id WHERE l.provider=? AND l.subject=? AND u.active=1 AND u.origin<>'dlna_guest'`, OIDCProvider, subject).Scan(&viewerID, &identityID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, core.ErrUnauthorized
	}
	if err != nil {
		return 0, 0, err
	}
	return viewerID, identityID, nil
}

// LinkOIDC lets a verified provider account sign in as viewerID. Linking the
// same account again refreshes its display name. An owner confirmation
// requires that the viewer still owns the server when the link is written.
func (l *Local) LinkOIDC(ctx context.Context, viewerID int64, subject, display string, owner bool) error {
	if viewerID <= 0 || !validOIDCSubject(subject) {
		return errors.New("identity: invalid single sign-on identity")
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = lockViewer(ctx, tx, viewerID); err != nil {
		return err
	}
	if owner {
		// Confirming a former viewer's link must not restore sessions
		// that were blocked when that viewer became the owner.
		var id int64
		err := tx.QueryRowContext(ctx, `SELECT id FROM identity_links
			WHERE viewer_id=? AND provider=? AND subject=? AND oidc_owner_approved=0`, viewerID, OIDCProvider, subject).Scan(&id)
		if err == nil {
			if _, err := revokeOIDCGrants(ctx, tx, viewerID, id); err != nil {
				return err
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	if err = linkOIDC(ctx, tx, viewerID, subject, display, owner); err != nil {
		return err
	}
	return tx.Commit()
}

func linkOIDC(ctx context.Context, tx *sql.Tx, viewerID int64, subject, display string, owner bool) error {
	ownerFlag := store.BoolInt(owner)
	result, err := tx.ExecContext(ctx, `INSERT INTO identity_links (viewer_id,provider,subject,display,verified_at,oidc_owner_approved)
		SELECT id, ?, ?, ?, ?, is_owner FROM auth_users WHERE id=? AND active=1 AND origin<>'dlna_guest' AND is_owner=?
		ON CONFLICT(provider, subject) DO UPDATE SET display=excluded.display, verified_at=excluded.verified_at,
			oidc_owner_approved=excluded.oidc_owner_approved
		WHERE identity_links.viewer_id=excluded.viewer_id`,
		OIDCProvider, subject, strings.TrimSpace(display), time.Now().Unix(), viewerID, ownerFlag)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 1 {
		return nil
	}
	var linkedViewer int64
	err = tx.QueryRowContext(ctx, `SELECT viewer_id FROM identity_links WHERE provider=? AND subject=?`, OIDCProvider, subject).Scan(&linkedViewer)
	if err == nil && linkedViewer != viewerID {
		return ErrIdentityLinked
	}
	return core.ErrUnauthorized
}

// HasOIDC reports whether an active viewer can sign in with single sign-on.
func (l *Local) HasOIDC(ctx context.Context, viewerID int64) (bool, error) {
	var found bool
	err := l.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM identity_links i JOIN auth_users u ON u.id=i.viewer_id
		WHERE i.viewer_id=? AND i.provider=? AND u.active=1 AND u.origin<>'dlna_guest'
		AND (u.is_owner=0 OR i.oidc_owner_approved=1))`, viewerID, OIDCProvider).Scan(&found)
	return found, err
}

// UnlinkOIDC removes the viewer's single sign-on link and its sessions in
// one transaction, using the same viewer lock as issuance.
func (g *Grants) UnlinkOIDC(ctx context.Context, viewerID, identityID int64) error {
	tx, err := g.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = lockViewer(ctx, tx, viewerID); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM identity_links WHERE id=? AND viewer_id=? AND provider=?`, identityID, viewerID, OIDCProvider)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return core.ErrNotFound
	}
	revoked, err := revokeOIDCGrants(ctx, tx, viewerID, identityID)
	if err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if revoked > 0 {
		g.gen.Add(1)
		g.emitRevoked(viewerID, "grant", "identity_unlinked")
	}
	return nil
}

func revokeOIDCGrants(ctx context.Context, tx *sql.Tx, viewerID, identityID int64) (int64, error) {
	result, err := tx.ExecContext(ctx, `UPDATE identity_grants SET revoked=1
		WHERE viewer_id=? AND identity_id=? AND revoked=0`, viewerID, identityID)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// ErrNoMatch means automatic linking found no viewer, or more than one.
var ErrNoMatch = errors.New("identity: no single matching viewer")

// OIDCMatch says how a first-time provider account may be matched to an
// existing viewer. Empty fields do not match.
type OIDCMatch struct {
	// Issuer limits matches to viewers with no other link at this provider.
	Issuer string
	// Email is compared case-insensitively with the viewer's stored e-mail.
	Email string
	// Username is compared case-insensitively with the viewer's sign-in
	// name or account username.
	Username string
}

// AutoLinkOIDC links the single matching viewer. Recheck the match under
// the viewer lock so another sign-in cannot claim the same viewer between
// matching and writing the link.
func (l *Local) AutoLinkOIDC(ctx context.Context, m OIDCMatch, subject, display string) (int64, error) {
	if !validOIDCSubject(subject) || m.Issuer == "" || !strings.HasPrefix(subject, m.Issuer+"|") {
		return 0, errors.New("identity: invalid single sign-on identity")
	}
	id, err := matchOIDCViewer(ctx, l.db, m)
	if err != nil {
		return 0, err
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if err = lockViewer(ctx, tx, id); err != nil {
		return 0, err
	}
	match, err := matchOIDCViewer(ctx, tx, m)
	if err != nil {
		return 0, err
	}
	if match != id {
		return 0, ErrNoMatch
	}
	if err = linkOIDC(ctx, tx, id, subject, display, false); err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

// Only a single active non-owner without a link at this issuer can match.
func matchOIDCViewer(ctx context.Context, db interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, m OIDCMatch) (int64, error) {
	email := strings.ToLower(strings.TrimSpace(m.Email))
	username := strings.ToLower(strings.TrimSpace(m.Username))
	if email == "" && username == "" {
		return 0, ErrNoMatch
	}
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT u.id FROM auth_users u
		WHERE u.active=1 AND u.is_owner=0 AND u.origin<>'dlna_guest'
		AND NOT EXISTS (SELECT 1 FROM identity_links o WHERE o.viewer_id=u.id AND o.provider=? AND o.subject LIKE ? ESCAPE '\')
		AND ((?<>'' AND LOWER(u.email)=?)
			OR (?<>'' AND (LOWER(u.username)=? OR EXISTS (SELECT 1 FROM identity_links s WHERE s.viewer_id=u.id AND s.provider='local' AND LOWER(s.subject)=?))))
		LIMIT 2`,
		OIDCProvider, likePrefix(m.Issuer+"|"), email, email, username, username, username)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(ids) != 1 {
		return 0, ErrNoMatch
	}
	return ids[0], nil
}

func likePrefix(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s) + "%"
}

// ProvisionOIDCViewer creates an active, non-owner viewer for a provider
// account and links it, in one transaction. The viewer also gets a local
// sign-in name without a password (the provider username, made unique), so
// it can later set a password for the Jellyfin and Emby apps.
func (l *Local) ProvisionOIDCViewer(ctx context.Context, subject, display, username, title, email string) (int64, error) {
	if !validOIDCSubject(subject) {
		return 0, errors.New("identity: invalid single sign-on identity")
	}
	base := provisionName(username, email)
	if strings.TrimSpace(title) == "" {
		title = base
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	name := ""
	for i := 1; i <= 50 && name == ""; i++ {
		candidate := base
		if i > 1 {
			candidate = fmt.Sprintf("%s%d", base, i)
		}
		var taken int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM identity_links WHERE provider='local' AND LOWER(subject)=LOWER(?)`, candidate).Scan(&taken); err != nil {
			return 0, err
		}
		if taken == 0 {
			name = candidate
		}
	}
	if name == "" {
		return 0, ErrUsernameExists
	}
	now := time.Now().Unix()
	var id int64
	if err = tx.QueryRowContext(ctx, `INSERT INTO auth_users(plex_id,username,title,email,is_owner,active,origin,created_at,updated_at) VALUES(NULL,?,?,?,0,1,'local',?,?) RETURNING id`,
		name, strings.TrimSpace(title), strings.TrimSpace(email), now, now).Scan(&id); err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO identity_links(viewer_id,provider,subject,display,verified_at,password_hash) VALUES(?,'local',?,?,?,'')`, id, name, name, now); err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO identity_links(viewer_id,provider,subject,display,verified_at) VALUES(?,?,?,?,?)`, id, OIDCProvider, subject, strings.TrimSpace(display), now); err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	l.emitViewerCreated(id, name, "local", "oidc")
	return id, nil
}

// provisionName derives a sign-in name from the provider username or the
// e-mail's local part: lower case letters, digits, dot, dash and underscore.
func provisionName(username, email string) string {
	src := strings.TrimSpace(username)
	if src == "" {
		src, _, _ = strings.Cut(strings.TrimSpace(email), "@")
	}
	var b strings.Builder
	for _, r := range strings.ToLower(src) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '-' || r == '_' {
			b.WriteRune(r)
		}
		if b.Len() >= 32 {
			break
		}
	}
	if b.Len() == 0 {
		return "viewer"
	}
	return b.String()
}
