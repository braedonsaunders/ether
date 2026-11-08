package plexwatch

import (
	"context"
	"errors"
	"math"
	"net/http"
	"time"
)

const (
	// importRequests caps the Plex reads in one import pass for one viewer.
	importRequests = 40
	// catchUp is the delay before the next pass while a sweep is unfinished.
	catchUp = time.Minute
)

// RunImports is started under cluster leadership. Each pass is bounded and the
// persisted lease also serializes manual imports and overlapping leaders.
func (s *Service) RunImports(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		s.importDue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-s.closed:
			return
		case <-ticker.C:
		}
	}
}

// importDue runs the scheduled passes that are due: an import for viewers
// who read from Plex, and for viewers who only send while their account's
// sync setting is off, a check of that setting so their held changes flow
// as soon as it is turned on.
func (s *Service) importDue(ctx context.Context) {
	now := s.now().UnixNano()
	rows, err := s.db.QueryContext(ctx, `SELECT user_id,plex_id FROM plexwatch_account
		WHERE ((direction & 2)<>0 OR ((direction & 1)<>0 AND consent=0)) AND revoked=0 AND token<>'' AND next_import<=? AND next_allowed<=? AND import_lease<=?
		ORDER BY next_import,user_id LIMIT 8`, now, now, now)
	if err != nil {
		return
	}
	type viewer struct{ user, plex int64 }
	var viewers []viewer
	for rows.Next() {
		var v viewer
		if rows.Scan(&v.user, &v.plex) == nil {
			viewers = append(viewers, v)
		}
	}
	rows.Close()
	for _, v := range viewers {
		if ctx.Err() != nil {
			return
		}
		acct, err := s.requireMatch(ctx, v.user, v.plex)
		if err != nil {
			continue
		}
		_, _ = s.schedulePass(ctx, acct)
	}
}

// ImportCurrent runs one bounded import pass now. Scheduled and manual
// passes share the cursor, lease and status.
func (s *Service) ImportCurrent(ctx context.Context, userID, plexID int64) (Status, error) {
	acct, err := s.requireMatch(ctx, userID, plexID)
	if err != nil {
		return Status{}, err
	}
	if acct.direction&2 == 0 {
		return Status{}, ErrReceivingOff
	}
	return s.schedulePass(ctx, acct)
}

// schedulePass is one pass for one viewer under the persisted lease. It
// reads the account's sync setting first; the import runs only when the
// setting is on and the viewer reads from Plex.
func (s *Service) schedulePass(ctx context.Context, acct accountRow) (Status, error) {
	userID, plexID := acct.userID, acct.plexID
	now := s.now()
	lease := now.Add(15 * time.Minute).UnixNano()
	res, err := s.db.ExecContext(ctx, `UPDATE plexwatch_account SET import_lease=? WHERE user_id=? AND plex_id=? AND token_generation=? AND import_lease<=? AND next_allowed<=?`,
		lease, userID, plexID, acct.tokenGeneration, now.UnixNano(), now.UnixNano())
	if err != nil {
		return Status{}, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return Status{}, err
	} else if n == 0 {
		return s.Status(ctx, userID, plexID)
	}
	importCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	finished, imported := true, false
	on, err := s.readConsent(importCtx, acct)
	if err == nil && on && acct.direction&2 != 0 {
		finished, err = s.importPass(importCtx, acct)
		imported = err == nil
	}
	if errors.Is(err, ErrRevoked) {
		if rerr := s.revoke(importCtx, userID, acct.tokenGeneration); rerr != nil && !errors.Is(rerr, ErrRevoked) {
			err = rerr
		}
	}
	if importCtx.Err() != nil { // release the lease even after leadership loss
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		_, _ = s.db.ExecContext(cleanup, `UPDATE plexwatch_account SET import_lease=0 WHERE user_id=? AND import_lease=?`, userID, lease)
		return Status{}, importCtx.Err()
	}
	now = s.now()
	var interval, failures int
	if e := s.db.QueryRowContext(ctx, `SELECT import_interval,import_failures FROM plexwatch_account WHERE user_id=? AND import_lease=?`, userID, lease).Scan(&interval, &failures); e != nil {
		return Status{}, e
	}
	problem := ""
	nextAllowed := int64(0)
	last := int64(0)
	delay := time.Duration(interval) * time.Minute
	if !finished {
		delay = catchUp
	}
	if err != nil {
		failures++
		problem = "network"
		var st *StatusError
		errors.As(err, &st)
		switch {
		case errors.Is(err, ErrRevoked):
			problem = "token"
		case st != nil && st.Status == http.StatusTooManyRequests:
			problem = "rate_limited"
		}
		delay = time.Minute * time.Duration(1<<min(failures-1, 8))
		if st != nil && st.RetryAfter > delay {
			delay = st.RetryAfter
		}
		maxDelay := max(time.Unix(0, math.MaxInt64).Sub(now)-time.Minute, 0)
		delay = min(delay, maxDelay)
		nextAllowed = now.Add(delay).UnixNano()
		s.log.Debug("plex watch import paused", "user", userID, "problem", problem)
	} else {
		failures = 0
		if imported {
			last = now.Unix()
		}
	}
	// Stable per-viewer jitter spreads work without changing the interval.
	jitter := time.Duration(userID%61) * time.Second
	next := now.Add(delay + jitter).UnixNano()
	if _, e := s.db.ExecContext(ctx, `UPDATE plexwatch_account SET import_lease=0, import_problem=?, import_failures=?, next_allowed=CASE WHEN ?>next_allowed THEN ? ELSE next_allowed END,
		next_import=?, last_import=CASE WHEN ?>0 THEN ? ELSE last_import END WHERE user_id=? AND import_lease=?`,
		problem, failures, nextAllowed, nextAllowed, next, last, last, userID, lease); e != nil {
		return Status{}, e
	}
	st, statusErr := s.Status(ctx, userID, plexID)
	if statusErr != nil {
		return Status{}, statusErr
	}
	return st, err
}
