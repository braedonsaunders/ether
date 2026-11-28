package identity

import (
	"context"
	"database/sql"
	"errors"
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
