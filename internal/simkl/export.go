package simkl

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/retryafter"
)

// FlushOne delivers at most one queued event under the per-viewer lease. The
// caller runs it only on the cluster leader. Simkl's default history write is
// idempotent for already-watched items, including retries after an uncertain
// response; rewatch creation is deliberately not enabled.
func (s Service) FlushOne(ctx context.Context, user int64) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	lease, err := s.Tokens.AcquireLease(ctx, user, time.Now())
	if err != nil {
		return err
	}
	defer func() {
		c, done := context.WithTimeout(context.Background(), 2*time.Second)
		defer done()
		_ = s.Tokens.ReleaseLease(c, user, lease)
	}()
	grant, err := s.Tokens.Load(ctx, user)
	if err != nil {
		return err
	}
	if grant.ClientID != s.Auth.ClientID {
		return s.exportFailure(ctx, user, lease, errors.New("simkl: app credentials changed; reconnect required"))
	}
	if time.Until(grant.ExpiresAt) < 24*time.Hour {
		grant, err = s.Tokens.Refresh(ctx, user, lease, s.Auth)
		if err != nil {
			return s.exportFailure(ctx, user, lease, err)
		}
	}
	var key, at, next int64
	err = s.Tokens.DB.QueryRowContext(ctx, `SELECT o.rating_key,o.watched_at,t.next_post FROM simkl_outbox o JOIN simkl_tokens t ON t.user_id=o.user_id WHERE o.user_id=? AND t.lease_id=? ORDER BY o.watched_at LIMIT 1`, user, lease).Scan(&key, &at, &next)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if time.Now().UnixMilli() < next {
		return ErrBusy
	}
	rating := core.RatingKey(key)
	verified := true
	if rating.Kind() == core.KindEpisode {
		verified, err = s.Auth.IsOrdinaryTV(ctx, rating.TMDB())
		if err != nil {
			return s.exportFailure(ctx, user, lease, err)
		}
	}
	if !verified {
		return s.dropUnsupported(ctx, user, lease, key, at, "anime_mapping")
	}
	// Persist pacing before the POST so a crash cannot cause another node to
	// immediately repeat it and trigger Simkl's POST throttling.
	tx, err := s.Tokens.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	res, err := tx.ExecContext(ctx, `UPDATE simkl_post_pacing SET next_post=? WHERE id=1 AND next_post<=?`, now+1000, now)
	if err != nil {
		tx.Rollback()
		return err
	}
	n, err := res.RowsAffected()
	if err != nil || n != 1 {
		tx.Rollback()
		if err != nil {
			return err
		}
		return ErrBusy
	}
	res, err = tx.ExecContext(ctx, `UPDATE simkl_tokens SET next_post=? WHERE user_id=? AND lease_id=? AND lease_until>?`, now+1000, user, lease, now)
	if err != nil {
		tx.Rollback()
		return err
	}
	n, err = res.RowsAffected()
	if err != nil || n != 1 {
		tx.Rollback()
		if err != nil {
			return err
		}
		return ErrBusy
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	err = s.Auth.PushHistory(ctx, grant.Access, rating, time.Unix(at, 0), verified)
	if errors.Is(err, ErrIdentityNotFound) {
		return s.dropUnsupported(ctx, user, lease, key, at, "not_found")
	}
	if err != nil {
		return s.exportFailure(ctx, user, lease, err)
	}
	_, err = s.Tokens.DB.ExecContext(ctx, `DELETE FROM simkl_outbox WHERE user_id=? AND rating_key=? AND watched_at=? AND EXISTS(SELECT 1 FROM simkl_tokens WHERE user_id=? AND lease_id=? AND lease_until>?)`, user, key, at, user, lease, time.Now().UnixMilli())
	return err
}
func (s Service) dropUnsupported(ctx context.Context, user int64, lease string, key, at int64, warning string) error {
	tx, err := s.Tokens.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE simkl_tokens SET last_warning=? WHERE user_id=? AND lease_id=? AND lease_until>?`, warning, user, lease, time.Now().UnixMilli())
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
	if _, err = tx.ExecContext(ctx, `DELETE FROM simkl_outbox WHERE user_id=? AND rating_key=? AND watched_at=?`, user, key, at); err != nil {
		return err
	}
	return tx.Commit()
}
func (s Service) exportFailure(ctx context.Context, user int64, lease string, err error) error {
	now := time.Now()
	delay := time.Minute
	expire := false
	var api *APIError
	if errors.As(err, &api) && api.Code == "invalid_grant" {
		_, e := s.Tokens.DB.ExecContext(ctx, `UPDATE simkl_tokens SET last_warning='auth_revoked',backoff_until=? WHERE user_id=? AND lease_id=?`, now.Add(365*24*time.Hour).UnixMilli(), user, lease)
		if e != nil {
			return e
		}
		return err
	}
	if errors.As(err, &api) && api.Status == 429 {
		if d, ok := retryafter.Parse(api.RetryAfter, now, 0); ok && d >= time.Second {
			delay = d
		}
	}
	var h *HTTPError
	if errors.As(err, &h) {
		expire = h.Status == 401
		if h.Status == 429 {
			if d, ok := retryafter.Parse(h.RetryAfter, now, 0); ok && d >= time.Second {
				delay = d
			}
		}
	}
	query := `UPDATE simkl_tokens SET backoff_until=? WHERE user_id=? AND lease_id=?`
	if expire {
		query = `UPDATE simkl_tokens SET backoff_until=?,expires_at=0 WHERE user_id=? AND lease_id=?`
	}
	if _, e := s.Tokens.DB.ExecContext(ctx, query, now.Add(delay).UnixMilli(), user, lease); e != nil {
		return e
	}
	return err
}

// RunDelivery is a leader-only loop. State events are enqueued by every node,
// but a shared per-viewer lease fences concurrent manual sync and delivery.
func RunDelivery(ctx context.Context, service func() Service, enabled func() bool, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	runDelivery(ctx, service, enabled, log, ticker.C)
}

func runDelivery(ctx context.Context, service func() Service, enabled func() bool, log *slog.Logger, ticks <-chan time.Time) {
	var cursor int64
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
		}
		if !enabled() {
			continue
		}
		s := service()
		rows, err := s.Tokens.DB.QueryContext(ctx, `SELECT user_id FROM (SELECT DISTINCT o.user_id FROM simkl_outbox o JOIN simkl_tokens t ON t.user_id=o.user_id WHERE t.backoff_until<=?) queued ORDER BY CASE WHEN user_id > ? THEN 0 ELSE 1 END, user_id LIMIT 100`, time.Now().UnixMilli(), cursor)
		if err != nil {
			if ctx.Err() == nil {
				log.Warn("simkl: queue scan failed", "err", err)
			}
			continue
		}
		var users []int64
		for rows.Next() {
			var id int64
			if rows.Scan(&id) == nil {
				users = append(users, id)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			continue
		}
		// Advance the starting viewer, even if it is leased or otherwise busy.
		// Ordering wraps over the entire queue, not just the first page.
		if len(users) > 0 {
			cursor = users[0]
		}
		for _, id := range users {
			if ctx.Err() != nil {
				return
			}
			if err := s.FlushOne(ctx, id); err != nil && !errors.Is(err, ErrBusy) && ctx.Err() == nil {
				log.Warn("simkl: delivery failed", "viewer", id, "err", err)
			}
		}
	}
}
