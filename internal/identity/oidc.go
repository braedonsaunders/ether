package identity

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// OIDCProvider is the identity_links provider name for single sign-on
// identities. Their subject joins the issuer and the provider's subject
// claim, so the same person at two providers is two identities.
const OIDCProvider = "oidc"

// ErrIdentityLinked means the provider account already signs in as another
// viewer.
var ErrIdentityLinked = errors.New("identity: provider account is linked to another viewer")

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
// same account again refreshes its display name. An account that already
// signs in as someone else is refused with ErrIdentityLinked.
func (l *Local) LinkOIDC(ctx context.Context, viewerID int64, subject, display string) error {
	if viewerID <= 0 || !validOIDCSubject(subject) {
		return errors.New("identity: invalid single sign-on identity")
	}
	display = strings.TrimSpace(display)
	result, err := l.db.ExecContext(ctx, `INSERT INTO identity_links (viewer_id,provider,subject,display,verified_at)
		SELECT id, ?, ?, ?, ? FROM auth_users WHERE id=? AND active=1 AND origin<>'dlna_guest'
		ON CONFLICT(provider, subject) DO UPDATE SET display=excluded.display, verified_at=excluded.verified_at
		WHERE identity_links.viewer_id=excluded.viewer_id`,
		OIDCProvider, subject, display, time.Now().Unix(), viewerID)
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
	var owner int64
	err = l.db.QueryRowContext(ctx, `SELECT viewer_id FROM identity_links WHERE provider=? AND subject=?`, OIDCProvider, subject).Scan(&owner)
	if err == nil && owner != viewerID {
		return ErrIdentityLinked
	}
	return core.ErrUnauthorized
}

// HasOIDC reports whether an active viewer can sign in with single sign-on.
func (l *Local) HasOIDC(ctx context.Context, viewerID int64) (bool, error) {
	var found bool
	err := l.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM identity_links i JOIN auth_users u ON u.id=i.viewer_id WHERE i.viewer_id=? AND i.provider=? AND u.active=1 AND u.origin<>'dlna_guest')`, viewerID, OIDCProvider).Scan(&found)
	return found, err
}

// UnlinkOIDC removes one of the viewer's single sign-on identities. Other
// providers' links are never removed here.
func (l *Local) UnlinkOIDC(ctx context.Context, viewerID, identityID int64) error {
	result, err := l.db.ExecContext(ctx, `DELETE FROM identity_links WHERE id=? AND viewer_id=? AND provider=?`, identityID, viewerID, OIDCProvider)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return core.ErrNotFound
	}
	return nil
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

// MatchOIDCViewer finds the single active viewer that m names. The owner is
// never matched: ownership is only reached through a link the owner made
// while signed in. Viewers already linked to another account at the same
// provider are skipped, so a second account cannot take one over. When e-mail
// and username both match, they must name the same viewer.
func (l *Local) MatchOIDCViewer(ctx context.Context, m OIDCMatch) (int64, error) {
	email := strings.ToLower(strings.TrimSpace(m.Email))
	username := strings.ToLower(strings.TrimSpace(m.Username))
	if email == "" && username == "" {
		return 0, ErrNoMatch
	}
	rows, err := l.db.QueryContext(ctx, `SELECT DISTINCT u.id FROM auth_users u
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
