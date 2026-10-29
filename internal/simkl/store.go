package simkl

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.Register("simkl", 1, `CREATE TABLE simkl_tokens (
 user_id INTEGER PRIMARY KEY,
 client_id TEXT NOT NULL,
 access_token TEXT NOT NULL,
 refresh_token TEXT NOT NULL,
 expires_at INTEGER NOT NULL,
 snapshot TEXT NOT NULL DEFAULT '',
 lease_id TEXT NOT NULL DEFAULT '',
 lease_until INTEGER NOT NULL DEFAULT 0,
 backoff_until INTEGER NOT NULL DEFAULT 0,
 next_post INTEGER NOT NULL DEFAULT 0,
 last_warning TEXT NOT NULL DEFAULT ''
 );
 CREATE TABLE simkl_post_pacing (
 id INTEGER PRIMARY KEY,
 next_post INTEGER NOT NULL DEFAULT 0
 );
 INSERT INTO simkl_post_pacing(id,next_post) VALUES(1,0);
 CREATE TABLE simkl_devices (
 user_id INTEGER PRIMARY KEY,
 generation TEXT NOT NULL,
 client_id TEXT NOT NULL,
 device_code TEXT NOT NULL DEFAULT '',
 expires_at INTEGER NOT NULL DEFAULT 0,
 next_poll INTEGER NOT NULL DEFAULT 0,
 interval_seconds INTEGER NOT NULL DEFAULT 0,
 claim_id TEXT NOT NULL DEFAULT '',
 claim_until INTEGER NOT NULL DEFAULT 0
 );
 CREATE TABLE simkl_outbox (
 user_id INTEGER NOT NULL,
 rating_key INTEGER NOT NULL,
 watched_at INTEGER NOT NULL,
 PRIMARY KEY(user_id,rating_key,watched_at),
 FOREIGN KEY(user_id) REFERENCES simkl_tokens(user_id) ON DELETE CASCADE
 );`)
}

// TokenStore stores a grant per viewer, with both fields bound to their row.
// A functioning field sealer is mandatory: this component never stores a
// new grant in cleartext, including during the legacy credential upgrade.
type TokenStore struct {
	DB     *sql.DB
	Sealer config.FieldSealer
}
type Grant struct {
	ClientID, Access, Refresh string
	ExpiresAt                 time.Time
}

func (s TokenStore) Save(ctx context.Context, user int64, token Token, now time.Time) error {
	if user <= 0 || s.DB == nil || s.Sealer == nil {
		return errors.New("simkl: token storage unavailable")
	}
	if token.ClientID == "" {
		return errors.New("simkl: client ID required")
	}
	if err := validateToken(token); err != nil {
		return err
	}
	seal := func(column, value string) (string, error) {
		v, err := s.Sealer.SealField(value, "simkl_tokens", column, config.FieldContext(user), token.ClientID)
		if err != nil {
			return "", err
		}
		if !config.IsSealed(v) {
			return "", errors.New("simkl: credential sealing is not enabled")
		}
		return v, nil
	}
	a, err := seal("access_token", token.AccessToken)
	if err != nil {
		return err
	}
	r, err := seal("refresh_token", token.RefreshToken)
	if err != nil {
		return err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := lockViewer(ctx, s.DB, tx, user); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM simkl_outbox WHERE user_id=?`, user); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO simkl_tokens(user_id,client_id,access_token,refresh_token,expires_at) VALUES(?,?,?,?,?) ON CONFLICT(user_id) DO UPDATE SET client_id=excluded.client_id,access_token=excluded.access_token,refresh_token=excluded.refresh_token,expires_at=excluded.expires_at,snapshot='' WHERE simkl_tokens.lease_until<?`, user, token.ClientID, a, r, now.Add(time.Duration(token.ExpiresIn)*time.Second).Unix(), now.UnixMilli())
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrBusy
	}
	return tx.Commit()
}
func (s TokenStore) Load(ctx context.Context, user int64) (Grant, error) {
	var clientID, a, r string
	var expiry int64
	if user <= 0 || s.DB == nil || s.Sealer == nil {
		return Grant{}, errors.New("simkl: token storage unavailable")
	}
	if err := s.DB.QueryRowContext(ctx, `SELECT client_id,access_token,refresh_token,expires_at FROM simkl_tokens WHERE user_id=?`, user).Scan(&clientID, &a, &r, &expiry); err != nil {
		return Grant{}, err
	}
	open := func(column, value string) (string, error) {
		if !config.IsSealed(value) {
			return "", errors.New("simkl: unsealed token")
		}
		v, _, err := s.Sealer.OpenField(value, "simkl_tokens", column, config.FieldContext(user), clientID)
		if err != nil {
			return "", fmt.Errorf("simkl: cannot open stored token: %w", err)
		}
		return v, nil
	}
	a, err := open("access_token", a)
	if err != nil {
		return Grant{}, err
	}
	r, err = open("refresh_token", r)
	if err != nil {
		return Grant{}, err
	}
	return Grant{ClientID: clientID, Access: a, Refresh: r, ExpiresAt: time.Unix(expiry, 0)}, nil
}
func (s TokenStore) Delete(ctx context.Context, user int64) error {
	if user <= 0 || s.DB == nil {
		return errors.New("simkl: token storage unavailable")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := lockViewer(ctx, s.DB, tx, user); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM simkl_tokens WHERE user_id=? AND lease_until<?`, user, time.Now().UnixMilli())
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		var present int
		err = tx.QueryRowContext(ctx, `SELECT 1 FROM simkl_tokens WHERE user_id=?`, user).Scan(&present)
		if err == nil {
			return ErrBusy
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM simkl_outbox WHERE user_id=?`, user); err != nil {
		return err
	}
	return tx.Commit()
}

// Pop removes a grant and its queue under the same viewer lock used for
// account replacement and enqueue, returning only the grant actually removed.
func (s TokenStore) Pop(ctx context.Context, user int64) (Grant, error) {
	if user <= 0 || s.DB == nil || s.Sealer == nil {
		return Grant{}, errors.New("simkl: token storage unavailable")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Grant{}, err
	}
	defer tx.Rollback()
	if err = lockViewer(ctx, s.DB, tx, user); err != nil {
		return Grant{}, err
	}
	var clientID, a, r string
	var expiry, leaseUntil int64
	err = tx.QueryRowContext(ctx, `SELECT client_id,access_token,refresh_token,expires_at,lease_until FROM simkl_tokens WHERE user_id=?`, user).Scan(&clientID, &a, &r, &expiry, &leaseUntil)
	if errors.Is(err, sql.ErrNoRows) {
		if err = tx.Commit(); err != nil {
			return Grant{}, err
		}
		return Grant{}, nil
	}
	if err != nil {
		return Grant{}, err
	}
	if leaseUntil >= time.Now().UnixMilli() {
		return Grant{}, ErrBusy
	}
	if !config.IsSealed(a) || !config.IsSealed(r) {
		return Grant{}, errors.New("simkl: unsealed token")
	}
	sealedAccess, sealedRefresh := a, r
	a, _, err = s.Sealer.OpenField(a, "simkl_tokens", "access_token", config.FieldContext(user), clientID)
	if err != nil {
		return Grant{}, err
	}
	r, _, err = s.Sealer.OpenField(r, "simkl_tokens", "refresh_token", config.FieldContext(user), clientID)
	if err != nil {
		return Grant{}, err
	}
	if err = deleteUnleasedGrant(ctx, tx, user, clientID, sealedAccess, sealedRefresh, time.Now()); err != nil {
		return Grant{}, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM simkl_outbox WHERE user_id=?`, user); err != nil {
		return Grant{}, err
	}
	if err = tx.Commit(); err != nil {
		return Grant{}, err
	}
	return Grant{ClientID: clientID, Access: a, Refresh: r, ExpiresAt: time.Unix(expiry, 0)}, nil
}

// A lease may be acquired and released between the read and delete, including
// a refresh that rotates credentials. Delete only the exact grant we read.
func deleteUnleasedGrant(ctx context.Context, tx *sql.Tx, user int64, clientID, access, refresh string, now time.Time) error {
	res, err := tx.ExecContext(ctx, `DELETE FROM simkl_tokens WHERE user_id=? AND lease_until<? AND client_id=? AND access_token=? AND refresh_token=?`, user, now.UnixMilli(), clientID, access, refresh)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrBusy
	}
	return nil
}
