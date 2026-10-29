package simkl

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
)

var ErrBusy = errors.New("simkl: account is busy")

// AcquireLease reserves a viewer grant across nodes for bounded remote work.
// Operations must end before the deadline; every update must also check the
// lease id, since a paused process may wake after losing its reservation.
func (s TokenStore) AcquireLease(ctx context.Context, user int64, now time.Time) (string, error) {
	if user <= 0 || s.DB == nil {
		return "", errors.New("simkl: token storage unavailable")
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	id := hex.EncodeToString(b[:])
	res, err := s.DB.ExecContext(ctx, `UPDATE simkl_tokens SET lease_id=?,lease_until=? WHERE user_id=? AND lease_until<? AND backoff_until<=?`, id, now.Add(90*time.Second).UnixMilli(), user, now.UnixMilli(), now.UnixMilli())
	if err != nil {
		return "", err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return "", err
	}
	if n != 1 {
		return "", ErrBusy
	}
	return id, nil
}
func (s TokenStore) ReleaseLease(ctx context.Context, user int64, id string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE simkl_tokens SET lease_id='',lease_until=0 WHERE user_id=? AND lease_id=?`, user, id)
	return err
}

// Refresh under a viewer lease prevents two nodes invalidating each other's
// access token. The conditional update cannot recreate a disconnected grant.
func (s TokenStore) Refresh(ctx context.Context, user int64, lease string, auth AuthClient) (Grant, error) {
	if lease == "" {
		return Grant{}, ErrBusy
	}
	var count int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM simkl_tokens WHERE user_id=? AND lease_id=? AND lease_until>?`, user, lease, time.Now().UnixMilli()).Scan(&count); err != nil {
		return Grant{}, err
	}
	if count != 1 {
		return Grant{}, ErrBusy
	}
	grant, err := s.Load(ctx, user)
	if err != nil {
		return Grant{}, err
	}
	if grant.ClientID != auth.ClientID {
		return Grant{}, errors.New("simkl: app credentials changed; reconnect required")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	token, err := auth.Refresh(ctx, grant.Refresh)
	if err != nil {
		return Grant{}, err
	}
	access, err := s.Sealer.SealField(token.AccessToken, "simkl_tokens", "access_token", config.FieldContext(user), grant.ClientID)
	if err != nil {
		return Grant{}, err
	}
	refresh, err := s.Sealer.SealField(token.RefreshToken, "simkl_tokens", "refresh_token", config.FieldContext(user), grant.ClientID)
	if err != nil {
		return Grant{}, err
	}
	if !config.IsSealed(access) || !config.IsSealed(refresh) {
		return Grant{}, errors.New("simkl: credential sealing is not enabled")
	}
	expires := time.Now().Add(time.Duration(token.ExpiresIn) * time.Second)
	res, err := s.DB.ExecContext(ctx, `UPDATE simkl_tokens SET access_token=?,refresh_token=?,expires_at=? WHERE user_id=? AND lease_id=? AND lease_until>?`, access, refresh, expires.Unix(), user, lease, time.Now().UnixMilli())
	if err != nil {
		return Grant{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Grant{}, err
	}
	if n != 1 {
		return Grant{}, ErrBusy
	}
	return Grant{ClientID: grant.ClientID, Access: token.AccessToken, Refresh: token.RefreshToken, ExpiresAt: expires}, nil
}
