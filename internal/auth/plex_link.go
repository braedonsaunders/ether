package auth

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.Register("auth", 6, `ALTER TABLE auth_users ADD COLUMN owner_enabled INTEGER NOT NULL DEFAULT 1; ALTER TABLE auth_users ADD COLUMN plex_shared INTEGER NOT NULL DEFAULT 1; UPDATE auth_users SET owner_enabled=CASE WHEN plex_id IS NULL THEN active ELSE 1 END, plex_shared=CASE WHEN plex_id IS NOT NULL THEN active ELSE 1 END;`)
	store.Register("auth", 5, `CREATE TABLE auth_membership_epoch (id INTEGER PRIMARY KEY, epoch INTEGER NOT NULL); INSERT INTO auth_membership_epoch(id,epoch) VALUES(1,0);`)
}

// upsertPlexLink records a verified Plex account without ever reassigning it
// to another viewer. It runs inside the same transaction as membership sync.
func upsertPlexLink(ctx context.Context, tx *sql.Tx, plexID int64, display string, now int64) error {
	res, err := tx.ExecContext(ctx, `INSERT INTO identity_links(viewer_id,provider,subject,display,verified_at)
 SELECT id,'plex',?,?,? FROM auth_users WHERE plex_id=?
 ON CONFLICT(provider,subject) DO UPDATE SET display=excluded.display,verified_at=excluded.verified_at
 WHERE identity_links.viewer_id=excluded.viewer_id`, strconv.FormatInt(plexID, 10), display, now, plexID)
	if err != nil {
		return fmt.Errorf("auth: link Plex identity: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("auth: Plex identity already linked to another viewer")
	}
	return nil
}

// backfillPlexLinks runs after all registered migrations, when auth_users is
// guaranteed to exist. It is idempotent and refuses conflicting ownership.
func backfillPlexLinks(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO identity_links(viewer_id,provider,subject,display,verified_at)
 SELECT id,'plex',CAST(plex_id AS TEXT),title,updated_at FROM auth_users WHERE plex_id IS NOT NULL
 ON CONFLICT(provider,subject) DO NOTHING`)
	if err != nil {
		return fmt.Errorf("auth: backfill Plex identities: %w", err)
	}
	var conflicts int
	err = tx.QueryRowContext(ctx, `SELECT count(*) FROM auth_users u JOIN identity_links l ON l.provider='plex' AND l.subject=CAST(u.plex_id AS TEXT) WHERE u.plex_id IS NOT NULL AND l.viewer_id<>u.id`).Scan(&conflicts)
	if err != nil {
		return err
	}
	if conflicts != 0 {
		return fmt.Errorf("auth: conflicting Plex identity ownership")
	}
	return tx.Commit()
}

// LinkPlex verifies a Plex account and its access to this server before
// attaching it to an existing active viewer. Existing viewers are never merged.
func (s *Service) LinkPlex(ctx context.Context, viewerID int64, token string) error {
	if viewerID <= 0 || token == "" || s.o.Validator == nil {
		return core.ErrUnauthorized
	}
	var epoch int64
	if err := s.db.QueryRowContext(ctx, `SELECT epoch FROM auth_membership_epoch WHERE id=1`).Scan(&epoch); err != nil {
		return err
	}
	account, err := s.o.Validator.Account(ctx, token)
	if err != nil {
		return err
	}
	if account == nil || account.ID <= 0 {
		return core.ErrUnauthorized
	}
	allowed, err := s.o.Validator.HasServer(ctx, token)
	if err != nil {
		return err
	}
	if !allowed {
		return core.ErrUnauthorized
	}
	s.membershipMu.Lock()
	defer s.membershipMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	lock, err := tx.ExecContext(ctx, `UPDATE auth_membership_epoch SET epoch=epoch WHERE id=1 AND epoch=?`, epoch)
	if err != nil {
		return err
	}
	locked, err := lock.RowsAffected()
	if err != nil {
		return err
	}
	if locked != 1 {
		return core.ErrUnauthorized
	}
	result, err := tx.ExecContext(ctx, `UPDATE auth_users SET plex_id=?, origin=CASE WHEN is_owner=1 THEN 'owner' ELSE 'validated' END, updated_at=? WHERE id=? AND active=1 AND plex_id IS NULL AND NOT EXISTS (SELECT 1 FROM auth_users WHERE plex_id=?)`, account.ID, s.o.Now().Unix(), viewerID, account.ID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return core.ErrUnauthorized
	}
	if err = upsertPlexLink(ctx, tx, account.ID, account.DisplayName(), s.o.Now().Unix()); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if err = s.reload(ctx); err != nil {
		return err
	}
	s.notify("")
	return nil
}
