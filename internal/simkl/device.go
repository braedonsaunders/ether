package simkl

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/retryafter"
)

// DeviceManager stores each short-lived device credential sealed in shared
// storage. Its generation and poll claim bind it to a viewer across nodes.
type DeviceManager struct {
	Auth       AuthClient
	AuthConfig func() AuthClient
	Tokens     TokenStore
}
type DeviceInfo struct {
	UserCode                string    `json:"userCode"`
	VerificationURI         string    `json:"verificationUri"`
	VerificationURIComplete string    `json:"verificationUriComplete"`
	ExpiresAt               time.Time `json:"expiresAt"`
	Interval                int       `json:"interval"`
}

var ErrNoPending = errors.New("simkl: no pending authorization")
var ErrPollEarly = errors.New("simkl: poll interval has not elapsed")
var ErrExpired = errors.New("simkl: authorization expired")

func (m *DeviceManager) auth() AuthClient {
	if m.AuthConfig != nil {
		return m.AuthConfig()
	}
	return m.Auth
}
func nonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
func (m *DeviceManager) Start(ctx context.Context, user int64) (DeviceInfo, error) {
	if user <= 0 || m.Tokens.DB == nil || m.Tokens.Sealer == nil {
		return DeviceInfo{}, errors.New("simkl: device storage unavailable")
	}
	auth := m.auth()
	generation, err := nonce()
	if err != nil {
		return DeviceInfo{}, err
	}
	_, err = m.Tokens.DB.ExecContext(ctx, `INSERT INTO simkl_devices(user_id,generation,client_id) VALUES(?,?,?) ON CONFLICT(user_id) DO UPDATE SET generation=excluded.generation,client_id=excluded.client_id,device_code='',expires_at=0,next_poll=0,interval_seconds=0,claim_id='',claim_until=0`, user, generation, auth.ClientID)
	if err != nil {
		return DeviceInfo{}, err
	}
	d, err := auth.Start(ctx)
	if err != nil {
		_, _ = m.Tokens.DB.ExecContext(ctx, `DELETE FROM simkl_devices WHERE user_id=? AND generation=?`, user, generation)
		return DeviceInfo{}, err
	}
	sealed, err := m.Tokens.Sealer.SealField(d.DeviceCode, "simkl_devices", "device_code", config.FieldContext(user), auth.ClientID)
	if err != nil {
		return DeviceInfo{}, err
	}
	if !config.IsSealed(sealed) {
		return DeviceInfo{}, errors.New("simkl: credential sealing is not enabled")
	}
	now := time.Now()
	expires := now.Add(time.Duration(d.ExpiresIn) * time.Second)
	res, err := m.Tokens.DB.ExecContext(ctx, `UPDATE simkl_devices SET device_code=?,expires_at=?,next_poll=?,interval_seconds=? WHERE user_id=? AND generation=?`, sealed, expires.UnixMilli(), now.Add(time.Duration(d.Interval)*time.Second).UnixMilli(), d.Interval, user, generation)
	if err != nil {
		return DeviceInfo{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return DeviceInfo{}, err
	}
	if n != 1 {
		return DeviceInfo{}, ErrNoPending
	}
	return DeviceInfo{UserCode: d.UserCode, VerificationURI: d.VerificationURI, VerificationURIComplete: d.VerificationURIComplete, ExpiresAt: expires, Interval: d.Interval}, nil
}
func (m *DeviceManager) Poll(ctx context.Context, user int64) (string, error) {
	if user <= 0 || m.Tokens.DB == nil || m.Tokens.Sealer == nil {
		return "", ErrNoPending
	}
	auth := m.auth()
	claim, err := nonce()
	if err != nil {
		return "", err
	}
	now := time.Now().UnixMilli()
	res, err := m.Tokens.DB.ExecContext(ctx, `UPDATE simkl_devices SET claim_id=?,claim_until=?,next_poll=?+interval_seconds*1000 WHERE user_id=? AND client_id=? AND device_code<>'' AND expires_at>? AND next_poll<=? AND claim_until<?`, claim, now+30000, now, user, auth.ClientID, now, now, now)
	if err != nil {
		return "", err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return "", err
	}
	if n != 1 {
		var expires int64
		var cid string
		e := m.Tokens.DB.QueryRowContext(ctx, `SELECT expires_at,client_id FROM simkl_devices WHERE user_id=?`, user).Scan(&expires, &cid)
		if errors.Is(e, sql.ErrNoRows) || cid != auth.ClientID {
			return "", ErrNoPending
		}
		if e != nil {
			return "", e
		}
		if expires <= now {
			_, _ = m.Tokens.DB.ExecContext(ctx, `DELETE FROM simkl_devices WHERE user_id=? AND expires_at<=?`, user, now)
			return "", ErrExpired
		}
		return "", ErrPollEarly
	}
	var sealed string
	if err = m.Tokens.DB.QueryRowContext(ctx, `SELECT device_code FROM simkl_devices WHERE user_id=? AND claim_id=?`, user, claim).Scan(&sealed); err != nil {
		return "", ErrNoPending
	}
	code, _, err := m.Tokens.Sealer.OpenField(sealed, "simkl_devices", "device_code", config.FieldContext(user), auth.ClientID)
	if err != nil {
		return "", err
	}
	token, err := auth.Poll(ctx, code)
	if err != nil {
		var api *APIError
		if errors.As(err, &api) {
			if api.Status == 429 {
				now := time.Now()
				delay, ok := retryafter.Parse(api.RetryAfter, now, 0)
				if !ok || delay < time.Second {
					delay = time.Minute
				}
				_, updateErr := m.Tokens.DB.ExecContext(ctx, `UPDATE simkl_devices SET next_poll=?,claim_id='',claim_until=0 WHERE user_id=? AND claim_id=?`, now.Add(delay).UnixMilli(), user, claim)
				if updateErr != nil {
					return "", updateErr
				}
				return "pending", nil
			}
			switch api.Code {
			case "authorization_pending":
				_, _ = m.Tokens.DB.ExecContext(ctx, `UPDATE simkl_devices SET claim_id='',claim_until=0 WHERE user_id=? AND claim_id=?`, user, claim)
				return "pending", nil
			case "slow_down":
				_, _ = m.Tokens.DB.ExecContext(ctx, `UPDATE simkl_devices SET interval_seconds=interval_seconds+5,next_poll=?+(interval_seconds+5)*1000,claim_id='',claim_until=0 WHERE user_id=? AND claim_id=?`, time.Now().UnixMilli(), user, claim)
				return "pending", nil
			case "expired_token", "access_denied", "invalid_client":
				_, _ = m.Tokens.DB.ExecContext(ctx, `DELETE FROM simkl_devices WHERE user_id=? AND claim_id=?`, user, claim)
				return "", err
			}
		}
		_, _ = m.Tokens.DB.ExecContext(ctx, `UPDATE simkl_devices SET claim_id='',claim_until=0 WHERE user_id=? AND claim_id=?`, user, claim)
		return "", err
	}
	return m.complete(ctx, user, claim, token)
}
func (m *DeviceManager) complete(ctx context.Context, user int64, claim string, token Token) (string, error) {
	seal := func(col, value string) (string, error) {
		v, err := m.Tokens.Sealer.SealField(value, "simkl_tokens", col, config.FieldContext(user), token.ClientID)
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
		return "", err
	}
	r, err := seal("refresh_token", token.RefreshToken)
	if err != nil {
		return "", err
	}
	tx, err := m.Tokens.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if err := lockViewer(ctx, m.Tokens.DB, tx, user); err != nil {
		return "", err
	}
	now := time.Now()
	res, err := tx.ExecContext(ctx, `DELETE FROM simkl_devices WHERE user_id=? AND claim_id=? AND client_id=? AND expires_at>?`, user, claim, token.ClientID, now.UnixMilli())
	if err != nil {
		return "", err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return "", err
	}
	if n != 1 {
		return "", ErrNoPending
	}
	res, err = tx.ExecContext(ctx, `INSERT INTO simkl_tokens(user_id,client_id,access_token,refresh_token,expires_at) VALUES(?,?,?,?,?) ON CONFLICT(user_id) DO UPDATE SET client_id=excluded.client_id,access_token=excluded.access_token,refresh_token=excluded.refresh_token,expires_at=excluded.expires_at,snapshot='',backoff_until=0,next_post=0,last_warning='' WHERE simkl_tokens.lease_until<?`, user, token.ClientID, a, r, now.Add(time.Duration(token.ExpiresIn)*time.Second).Unix(), now.UnixMilli())
	if err != nil {
		return "", err
	}
	n, err = res.RowsAffected()
	if err != nil {
		return "", err
	}
	if n != 1 {
		return "", ErrBusy
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM simkl_outbox WHERE user_id=?`, user); err != nil {
		return "", err
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	return "authorized", nil
}
func (m *DeviceManager) Disconnect(ctx context.Context, user int64) error {
	// Delete the flow before the grant so a cross-node in-flight poll cannot
	// save its result after disconnect. A busy grant requires a retry.
	if _, err := m.Tokens.DB.ExecContext(ctx, `DELETE FROM simkl_devices WHERE user_id=?`, user); err != nil {
		return err
	}
	grant, err := m.Tokens.Pop(ctx, user)
	if err != nil {
		return err
	}
	if grant.Refresh != "" {
		auth := m.auth()
		if auth.ClientID != grant.ClientID {
			return errors.New("simkl: disconnected locally; previous app credentials are unavailable for revocation")
		}
		return auth.Revoke(ctx, grant.Refresh)
	}
	return nil
}
