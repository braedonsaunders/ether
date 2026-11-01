package plexwatch

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/state"
)

// completeSend records acceptance, import echo and acknowledgement as one
// transaction. Until commit is known to succeed the immutable admission keeps
// conflicting writes fenced, even if the original journal row was superseded,
// reclaimed, cancelled or compacted while the request was in flight.
func (s *Service) completeSend(ctx context.Context, row state.SyncRow, id, guid, kind string, at int64, rating int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO plexwatch_sent(user_id,guid,kind,at_ms,rating,sent_at) VALUES(?,?,?,?,?,?) ON CONFLICT DO NOTHING`, row.UserID, guid, kind, at, rating, s.now().UnixNano()); err != nil {
		return err
	}
	if s.receiptFault != nil {
		if err := s.receiptFault("echo"); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE state_sync_journal SET delivered_at=?,claimed_by='',claimed_at=0,dead=0,last_error='' WHERE seq=?`, s.now().UnixNano(), row.Seq); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE plexwatch_attempt SET receipt_at=? WHERE attempt_id=?`, s.now().UnixNano(), id); err != nil {
		return err
	}
	if s.receiptFault != nil {
		if err := s.receiptFault("commit"); err != nil {
			return err
		}
	}
	if s.receiptCommit != nil {
		return s.receiptCommit(tx)
	}
	return tx.Commit()
}

// clearAttempt removes an admission row when Plex definitively refused the
// write (HTTP 400, 401, 403, 422), so a rejected write does not fence later
// changes for that title. When restoreNeverSent is true, the journal row stays
// eligible for a fresh admission once the refusal condition clears.
func (s *Service) clearAttempt(ctx context.Context, attemptID string, seq int64, restoreNeverSent bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if attemptID != "" {
		if _, err := tx.ExecContext(ctx, `DELETE FROM plexwatch_attempt WHERE attempt_id=? AND receipt_at=0`, attemptID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM plexwatch_permit WHERE permit_id=?`, attemptID); err != nil {
			return err
		}
	}
	if restoreNeverSent && seq > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE state_sync_journal SET never_sent=1 WHERE seq=? AND delivered_at=0`, seq); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type heldAttempt struct {
	id                string
	seq               int64
	userID            int64
	ratingKey         int64
	plexID            int64
	generation        int64
	guid              string
	aspect            string
	occurredAt        int64
	payload           string
	admittedAt        int64
	reconcileAttempts int
	nextReconcile     int64
	livePermit        bool
}

// ReconcileOnce runs one pass of scheduled hold reconciliation across viewers
// whose unreceipted attempts are due and have no live in-flight permit.
func (s *Service) ReconcileOnce(ctx context.Context) (bool, error) {
	now := s.now().UnixNano()
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT a.user_id FROM plexwatch_attempt a
		WHERE a.receipt_at=0 AND a.next_reconcile<=?
		  AND NOT EXISTS (SELECT 1 FROM plexwatch_permit p WHERE p.permit_id=a.attempt_id AND p.until_at>?)
		ORDER BY a.user_id`, now, now)
	if err != nil {
		return false, err
	}
	var users []int64
	for rows.Next() {
		var u int64
		if err := rows.Scan(&u); err != nil {
			rows.Close()
			return false, err
		}
		users = append(users, u)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, err
	}
	rows.Close()
	if len(users) == 0 {
		return false, nil
	}
	for _, u := range users {
		if err := s.reconcileUser(ctx, u, false); err != nil && ctx.Err() != nil {
			return true, err
		}
	}
	return true, nil
}

func (s *Service) loadHeldAttempts(ctx context.Context, userID int64) ([]heldAttempt, error) {
	now := s.now().UnixNano()
	rows, err := s.db.QueryContext(ctx, `SELECT a.attempt_id, a.seq, a.user_id, a.rating_key, a.plex_id, a.generation,
		a.guid, a.aspect, a.occurred_at, a.payload, a.admitted_at, a.reconcile_attempts, a.next_reconcile,
		CASE WHEN EXISTS (SELECT 1 FROM plexwatch_permit p WHERE p.permit_id=a.attempt_id AND p.until_at>?) THEN 1 ELSE 0 END
		FROM plexwatch_attempt a
		WHERE a.user_id=? AND a.receipt_at=0
		ORDER BY a.seq`, now, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []heldAttempt
	for rows.Next() {
		var a heldAttempt
		var live int
		if err := rows.Scan(&a.id, &a.seq, &a.userID, &a.ratingKey, &a.plexID, &a.generation,
			&a.guid, &a.aspect, &a.occurredAt, &a.payload, &a.admittedAt, &a.reconcileAttempts, &a.nextReconcile, &live); err != nil {
			return nil, err
		}
		a.livePermit = live != 0
		list = append(list, a)
	}
	return list, rows.Err()
}

// reconcileUser reads Plex's current watch state for userID and compares it
// against any unreceipted plexwatch_attempt rows. If Plex already has the
// admitted state, the attempt is receipted without re-POSTing. If Plex clearly
// does not have it and no permit for that attempt is still in flight, the hold
// is released and the current intent is allowed one fresh admission.
func (s *Service) reconcileUser(ctx context.Context, userID int64, manual bool) error {
	held, err := s.loadHeldAttempts(ctx, userID)
	if err != nil || len(held) == 0 {
		return err
	}
	acct, ok, err := s.loadAccount(ctx, userID)
	if err != nil {
		return err
	}
	if !ok || acct.revoked || acct.token == "" || acct.plexID <= 0 {
		if manual {
			return errors.New("plexwatch: viewer has not connected a Plex account")
		}
		return nil
	}
	if acct.eligibility != "allowed" {
		if manual {
			return ErrNotEligible
		}
		return nil
	}

	targets := make(map[string]bool)
	resolvedGUIDs := make(map[string]string, len(held))
	for _, a := range held {
		g := a.guid
		if g == "" {
			if rg, gerr := s.lookupGUID(ctx, core.RatingKey(a.ratingKey)); gerr == nil && rg != "" {
				g = rg
			}
		}
		if g != "" {
			resolvedGUIDs[a.id] = g
			targets[g] = true
		}
	}

	found, err := s.fetchRemoteItems(ctx, acct.token, acct.plexID, targets)
	if err != nil {
		if errors.Is(err, ErrRevoked) {
			_ = s.revoke(ctx, userID, acct.tokenGeneration)
		} else if errors.Is(err, ErrNotEligible) {
			_, _ = s.db.ExecContext(ctx, `UPDATE plexwatch_account SET eligibility='refused', enabled=0, checked_at=? WHERE user_id=? AND token_generation=?`,
				s.now().UnixNano(), userID, acct.tokenGeneration)
		}
		s.deferReconcileAttempts(ctx, held)
		return err
	}

	now := s.now().UnixNano()
	for _, a := range held {
		targetGUID := resolvedGUIDs[a.id]
		wantItem, legacyNoJournal := s.expectedAttemptItem(ctx, a, targetGUID)
		if targetGUID != "" {
			if remote, ok := found[targetGUID]; ok && remoteMatchesAttempt(a.aspect, wantItem, remote, legacyNoJournal) {
				if err := s.receiptReconciledAttempt(ctx, a, targetGUID, wantItem, remote); err != nil {
					return err
				}
				continue
			}
		}
		// Absence during a live permit (or before the initial settle window on
		// automatic checks) is not proof that an in-flight request will not land.
		if a.livePermit || (!manual && now < a.nextReconcile) {
			continue
		}
		if a.guid == "" && !manual {
			s.deferReconcileAttempt(ctx, a)
			continue
		}
		if err := s.releaseReconciledAttempt(ctx, a); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) fetchRemoteItems(ctx context.Context, token string, plexID int64, targets map[string]bool) (map[string]remoteItem, error) {
	found := make(map[string]remoteItem)
	cursor := ""
	for page := 0; page < 20; page++ {
		remote, err := s.client.fetchState(ctx, token, plexID, cursor)
		if err != nil {
			return nil, err
		}
		for _, item := range remote.Items {
			if item.GUID == "" {
				continue
			}
			if _, exists := found[item.GUID]; !exists {
				found[item.GUID] = item
			}
		}
		allFound := len(targets) > 0
		for g := range targets {
			if _, ok := found[g]; !ok {
				allFound = false
				break
			}
		}
		if allFound || !remote.HasNextPage || remote.EndCursor == "" {
			break
		}
		cursor = remote.EndCursor
	}
	return found, nil
}

func (s *Service) expectedAttemptItem(ctx context.Context, a heldAttempt, targetGUID string) (postItem, bool) {
	if a.payload != "" {
		var wire struct {
			Items []postItem `json:"items"`
			User  int64      `json:"user"`
		}
		if err := json.Unmarshal([]byte(a.payload), &wire); err == nil && len(wire.Items) == 1 {
			return wire.Items[0], false
		}
	}
	sentAt := time.Unix(0, a.occurredAt).UnixMilli()
	var kind string
	var rating float64
	var occurredAt int64
	err := s.db.QueryRowContext(ctx, `SELECT kind, rating, occurred_at FROM state_sync_journal WHERE seq=?`, a.seq).
		Scan(&kind, &rating, &occurredAt)
	if err != nil {
		return postItem{GUID: targetGUID, ViewedAt: sentAt, RatedAt: sentAt}, true
	}
	if occurredAt > 0 {
		sentAt = time.Unix(0, occurredAt).UnixMilli()
	}
	item := postItem{GUID: targetGUID}
	switch a.aspect {
	case string(state.SyncWatch):
		item.ViewState = kind
		item.ViewedAt = sentAt
	case string(state.SyncRating):
		if n, ok := starRating(rating); ok {
			item.Rating = &n
			item.RatedAt = sentAt
		}
	}
	return item, false
}

func remoteMatchesAttempt(aspect string, want postItem, remote remoteItem, legacyNoJournal bool) bool {
	switch aspect {
	case string(state.SyncWatch):
		if remote.ViewState != state.SyncComplete && remote.ViewState != state.SyncUnplayed {
			return false
		}
		if legacyNoJournal {
			return remote.ViewedAt >= want.ViewedAt
		}
		if want.ViewState != "" && remote.ViewState == want.ViewState && remote.ViewedAt >= want.ViewedAt {
			return true
		}
		return want.ViewedAt > 0 && remote.ViewedAt > want.ViewedAt
	case string(state.SyncRating):
		if remote.Rating == nil {
			return false
		}
		if legacyNoJournal {
			return remote.RatedAt >= want.RatedAt
		}
		if want.Rating != nil && *remote.Rating == *want.Rating && remote.RatedAt >= want.RatedAt {
			return true
		}
		return want.RatedAt > 0 && remote.RatedAt > want.RatedAt
	default:
		return false
	}
}

func (s *Service) receiptReconciledAttempt(ctx context.Context, a heldAttempt, targetGUID string, want postItem, remote remoteItem) error {
	sentKind := want.ViewState
	sentAt := want.ViewedAt
	sentRating := 0
	if a.aspect == string(state.SyncRating) {
		sentKind = "rating"
		sentAt = want.RatedAt
		if sentAt == 0 {
			sentAt = remote.RatedAt
		}
		if want.Rating != nil {
			sentRating = *want.Rating
		} else if remote.Rating != nil {
			sentRating = *remote.Rating
		}
	} else {
		if sentKind == "" {
			sentKind = remote.ViewState
		}
		if sentAt == 0 {
			sentAt = remote.ViewedAt
		}
	}

	remapped := false
	if currentGUID, err := s.lookupGUID(ctx, core.RatingKey(a.ratingKey)); err == nil && currentGUID != "" && currentGUID != targetGUID {
		remapped = true
	}

	now := s.now().UnixNano()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO plexwatch_sent(user_id,guid,kind,at_ms,rating,sent_at) VALUES(?,?,?,?,?,?) ON CONFLICT DO NOTHING`,
		a.userID, targetGUID, sentKind, sentAt, sentRating, now); err != nil {
		return err
	}
	if !remapped {
		if _, err := tx.ExecContext(ctx, `UPDATE state_sync_journal SET delivered_at=?,claimed_by='',claimed_at=0,dead=0,last_error='' WHERE seq=?`, now, a.seq); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE plexwatch_attempt SET receipt_at=? WHERE attempt_id=?`, now, a.id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM plexwatch_permit WHERE permit_id=?`, a.id); err != nil {
		return err
	}
	if err := s.reviveUnblockedTx(ctx, tx, a, remapped); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) releaseReconciledAttempt(ctx context.Context, a heldAttempt) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM plexwatch_attempt WHERE attempt_id=? AND receipt_at=0`, a.id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM plexwatch_permit WHERE permit_id=?`, a.id); err != nil {
		return err
	}
	if err := s.reviveUnblockedTx(ctx, tx, a, true); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) reviveUnblockedTx(ctx context.Context, tx *sql.Tx, a heldAttempt, reviveAttemptRow bool) error {
	if reviveAttemptRow {
		if _, err := tx.ExecContext(ctx, `UPDATE state_sync_journal
			SET never_sent=1, dead=0, attempts=0, next_attempt=0, last_error='', claimed_by='', claimed_at=0
			WHERE seq=? AND delivered_at=0 AND superseded=0
			  AND consent_generation=COALESCE((SELECT generation FROM plexwatch_fence WHERE user_id=state_sync_journal.user_id),0)`,
			a.seq); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE state_sync_journal
		SET dead=0, attempts=0, next_attempt=0, last_error='', claimed_by='', claimed_at=0
		WHERE user_id=? AND aspect=? AND delivered_at=0 AND superseded=0 AND never_sent=1
		  AND dead=1 AND last_error IN ('uncertain_send', 'uncertain_legacy')
		  AND consent_generation=COALESCE((SELECT generation FROM plexwatch_fence WHERE user_id=state_sync_journal.user_id),0)`,
		a.userID, a.aspect); err != nil {
		return err
	}
	return nil
}

func (s *Service) deferReconcileAttempts(ctx context.Context, held []heldAttempt) {
	for _, a := range held {
		s.deferReconcileAttempt(ctx, a)
	}
}

func (s *Service) deferReconcileAttempt(ctx context.Context, a heldAttempt) {
	next := s.now().Add(backoff(a.reconcileAttempts)).UnixNano()
	_, _ = s.db.ExecContext(ctx, `UPDATE plexwatch_attempt SET reconcile_attempts=reconcile_attempts+1, next_reconcile=? WHERE attempt_id=? AND receipt_at=0`,
		next, a.id)
}
