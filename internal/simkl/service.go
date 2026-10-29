package simkl

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/braedonsaunders/ether/internal/state"
)

// Service is supplied explicit state and credential dependencies. Manual sync
// uses a lease and a deadline shorter than the lease; it never advances the
// snapshot after a partial pull.
type Service struct {
	Auth   AuthClient
	Tokens TokenStore
	State  *state.State
}
type SyncResult struct {
	Imported int `json:"imported"`
	Skipped  int `json:"skipped"`
}

func (s Service) Sync(ctx context.Context, user int64) (SyncResult, error) {
	if s.State == nil {
		return SyncResult{}, errors.New("simkl: state unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	lease, err := s.Tokens.AcquireLease(ctx, user, time.Now())
	if err != nil {
		return SyncResult{}, err
	}
	defer func() {
		releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer releaseCancel()
		_ = s.Tokens.ReleaseLease(releaseCtx, user, lease)
	}()
	grant, err := s.Tokens.Load(ctx, user)
	if err != nil {
		return SyncResult{}, err
	}
	if grant.ClientID != s.Auth.ClientID {
		return SyncResult{}, errors.New("simkl: app credentials changed; reconnect required")
	}
	if time.Until(grant.ExpiresAt) < 24*time.Hour {
		grant, err = s.Tokens.Refresh(ctx, user, lease, s.Auth)
		if err != nil {
			return SyncResult{}, s.exportFailure(ctx, user, lease, err)
		}
	}
	var sealed, snapshot string
	if err = s.Tokens.DB.QueryRowContext(ctx, `SELECT access_token,snapshot FROM simkl_tokens WHERE user_id=? AND lease_id=? AND lease_until>?`, user, lease, time.Now().UnixMilli()).Scan(&sealed, &snapshot); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SyncResult{}, ErrBusy
		}
		return SyncResult{}, err
	}
	records, next, skipped, err := s.Auth.PullHistory(ctx, user, grant.Access, snapshot)
	if err != nil {
		return SyncResult{}, s.exportFailure(ctx, user, lease, err)
	}
	if err = commitLeasedHistory(ctx, s.State, user, sealed, snapshot, next, lease, records); err != nil {
		return SyncResult{}, err
	}
	return SyncResult{Imported: len(records), Skipped: skipped}, nil
}
func commitLeasedHistory(ctx context.Context, st *state.State, user int64, sealed, previous, next, lease string, records []state.ImportRecord) error {
	if user <= 0 || sealed == "" || next == "" || lease == "" {
		return errors.New("simkl: invalid import commit")
	}
	for _, r := range records {
		if r.UserID != user {
			return errors.New("simkl: import viewer mismatch")
		}
	}
	return st.ImportTransaction(ctx, func(tx *sql.Tx) (state.ImportBatch, error) {
		res, err := tx.ExecContext(ctx, `UPDATE simkl_tokens SET snapshot=? WHERE user_id=? AND access_token=? AND snapshot=? AND lease_id=? AND lease_until>?`, next, user, sealed, previous, lease, time.Now().UnixMilli())
		if err != nil {
			return state.ImportBatch{}, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return state.ImportBatch{}, err
		}
		if n != 1 {
			return state.ImportBatch{}, ErrBusy
		}
		return state.ImportBatch{Records: records}, nil
	})
}
