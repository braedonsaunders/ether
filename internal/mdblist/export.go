package mdblist

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
// caller runs it only on the cluster leader. MDBList's watched write is
// idempotent for already-watched items, including retries after an uncertain
// response, so a re-sent completion cannot create a duplicate play.
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
	var key, at, next int64
	err = s.Tokens.DB.QueryRowContext(ctx, `SELECT o.rating_key,o.watched_at,a.next_post FROM mdblist_outbox o JOIN mdblist_accounts a ON a.user_id=o.user_id WHERE o.user_id=? AND a.lease_id=? ORDER BY o.watched_at LIMIT 1`, user, lease).Scan(&key, &at, &next)
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
	// Persist pacing before the POST so a crash cannot cause another node to
	// immediately repeat it and trigger MDBList's write throttling.
	tx, err := s.Tokens.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	res, err := tx.ExecContext(ctx, `UPDATE mdblist_post_pacing SET next_post=? WHERE id=1 AND next_post<=?`, now+200, now)
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
	res, err = tx.ExecContext(ctx, `UPDATE mdblist_accounts SET next_post=? WHERE user_id=? AND lease_id=? AND lease_until>?`, now+200, user, lease, now)
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
	err = s.Client.PushHistory(ctx, grant.APIKey, rating, time.Unix(at, 0))
	if errors.Is(err, ErrUnsupported) || permanentClientError(err) {
		return s.dropUnsupported(ctx, user, lease, key, at, "not_found")
	}
	if err != nil {
		return s.exportFailure(ctx, user, lease, err)
	}
	_, err = s.Tokens.DB.ExecContext(ctx, `DELETE FROM mdblist_outbox WHERE user_id=? AND rating_key=? AND watched_at=? AND EXISTS(SELECT 1 FROM mdblist_accounts WHERE user_id=? AND lease_id=? AND lease_until>?)`, user, key, at, user, lease, time.Now().UnixMilli())
	return err
}

func permanentClientError(err error) bool {
	var h *HTTPError
	if errors.As(err, &h) {
		return h.Status >= 400 && h.Status < 500 && h.Status != 401 && h.Status != 403 && h.Status != 429
	}
	return false
}

func (s Service) dropUnsupported(ctx context.Context, user int64, lease string, key, at int64, warning string) error {
	tx, err := s.Tokens.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE mdblist_accounts SET last_warning=? WHERE user_id=? AND lease_id=? AND lease_until>?`, warning, user, lease, time.Now().UnixMilli())
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
	if _, err = tx.ExecContext(ctx, `DELETE FROM mdblist_outbox WHERE user_id=? AND rating_key=? AND watched_at=?`, user, key, at); err != nil {
		return err
	}
	return tx.Commit()
}

func (s Service) exportFailure(ctx context.Context, user int64, lease string, err error) error {
	now := time.Now()
	delay := time.Minute
	expire := false
	var h *HTTPError
	if errors.As(err, &h) {
		expire = h.Status == 401 || h.Status == 403
		if h.Status == 429 {
			if d, ok := retryafter.Parse(h.RetryAfter, now, 0); ok && d >= time.Second {
				delay = d
			}
		}
	}
	query := `UPDATE mdblist_accounts SET backoff_until=? WHERE user_id=? AND lease_id=?`
	if expire {
		// A rejected key needs the owner to paste a new one; park delivery
		// far out so the loop does not hammer MDBList, and flag it.
		delay = 365 * 24 * time.Hour
		query = `UPDATE mdblist_accounts SET backoff_until=?,last_warning='auth_rejected' WHERE user_id=? AND lease_id=?`
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
		rows, err := s.Tokens.DB.QueryContext(ctx, `SELECT user_id FROM (SELECT DISTINCT o.user_id FROM mdblist_outbox o JOIN mdblist_accounts a ON a.user_id=o.user_id WHERE a.backoff_until<=?) queued ORDER BY CASE WHEN user_id > ? THEN 0 ELSE 1 END, user_id LIMIT 100`, time.Now().UnixMilli(), cursor)
		if err != nil {
			if ctx.Err() == nil {
				log.Warn("mdblist: queue scan failed", "err", err)
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
				log.Warn("mdblist: delivery failed", "viewer", id, "err", err)
			}
		}
	}
}
