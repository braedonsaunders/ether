package plexwatch

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"strconv"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/state"
	"github.com/braedonsaunders/ether/internal/store"
)

const (
	sentPerUserCap     = 1000
	unmappedPerUserCap = 5000
)

func (s *Service) ensureFenceTx(ctx context.Context, tx *sql.Tx, userID int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO plexwatch_fence(user_id, generation, inflight) VALUES(?,1,0)
		ON CONFLICT(user_id) DO NOTHING`, userID)
	return err
}

// lockFence is the shared row lock for consent transitions and permit
// admission. On PostgreSQL it is FOR UPDATE, so a disable cannot commit
// between the admission read and the permit insert.
func (s *Service) lockFence(ctx context.Context, tx *sql.Tx, userID int64) (int64, error) {
	q := `SELECT generation FROM plexwatch_fence WHERE user_id=?`
	if store.DialectOf(s.db) == store.Postgres {
		q += ` FOR UPDATE`
	}
	var gen int64
	err := tx.QueryRowContext(ctx, q, userID).Scan(&gen)
	return gen, err
}

// withFence serializes an account transition on the fence row.
func (s *Service) withFence(ctx context.Context, userID int64, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.ensureFenceTx(ctx, tx, userID); err != nil {
		return err
	}
	if _, err := s.lockFence(ctx, tx, userID); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// sendPermit is one admitted send. OK is false when this attempt must not
// send. Terminal is set only when this attempt still owns the live row and
// the refusal is final (missing or stale consent, or sync left off). A
// database error is not terminal: the caller leaves the intent retryable.
// A permit that was inserted is already in flight. A later disable does not
// delete it, and the HTTP request cannot be recalled.
type sendPermit struct {
	ID         string
	Generation int64
	Token      string
	PlexID     int64
	OK         bool
	Terminal   string
}

// acquireSend admits one send under the fence lock. The permit is inserted
// only when the locked fence generation matches the intent's consent
// generation and this attempt still owns that journal row. Release deletes
// that permit id only.
func (s *Service) acquireSend(ctx context.Context, row state.SyncRow, attempt string, item postItem) (sendPermit, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return sendPermit{}, err
	}
	defer tx.Rollback()
	if s.admitFault != nil {
		if err := s.admitFault(); err != nil {
			return sendPermit{}, err
		}
	}
	fenceGen, err := s.lockFence(ctx, tx, row.UserID)
	fenceMissing := errors.Is(err, sql.ErrNoRows)
	if err != nil && !fenceMissing {
		return sendPermit{}, err
	}
	var sealed, eligibility string
	var enabled, revoked int
	var tokenGen int64
	var plexID int64
	err = tx.QueryRowContext(ctx, `SELECT plex_id, token, enabled, revoked, token_generation, eligibility FROM plexwatch_account WHERE user_id=?`, row.UserID).
		Scan(&plexID, &sealed, &enabled, &revoked, &tokenGen, &eligibility)
	accountMissing := errors.Is(err, sql.ErrNoRows)
	if err != nil && !accountMissing {
		return sendPermit{}, err
	}
	owned, consent, err := s.lockIntent(ctx, tx, row.Seq, attempt)
	if err != nil {
		return sendPermit{}, err
	}
	stale := fenceMissing || accountMissing || consent != fenceGen || consent != row.ConsentGen
	disabled := enabled == 0 || revoked != 0 || sealed == "" || tokenGen != fenceGen
	notEligible := eligibility != "allowed"
	if !owned || stale || disabled || notEligible {
		out := sendPermit{}
		if owned && stale {
			out.Terminal = "consent"
		} else if owned && disabled {
			out.Terminal = "disabled"
		} else if owned && notEligible {
			out.Terminal = "not_eligible"
		}
		return out, nil
	}

	// Conflicts belong to the remote account/GUID/aspect, not a worker lease
	// or consent generation. Also retain local identity to fence GUID remaps.
	// Unmapped legacy attempts conservatively fence their account/aspect.
	var conflicts int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM plexwatch_attempt
  WHERE receipt_at=0 AND aspect=? AND
   ((user_id=? AND (rating_key=? OR guid='')) OR (plex_id=? AND (guid=? OR guid='')))`,
		string(row.Aspect), row.UserID, int64(row.Key), plexID, item.GUID).Scan(&conflicts); err != nil {
		return sendPermit{}, err
	}
	if conflicts > 0 {
		return sendPermit{Terminal: "uncertain_send"}, nil
	}
	var neverSent int
	if err := tx.QueryRowContext(ctx, `SELECT never_sent FROM state_sync_journal WHERE seq=?`, row.Seq).Scan(&neverSent); err != nil {
		return sendPermit{}, err
	}
	if neverSent != 1 {
		// Old writers may append after upgrade. A dead queue row alone is not a
		// durable hold: compaction, consent changes and supersession can erase it.
		if _, err := tx.ExecContext(ctx, `INSERT INTO plexwatch_attempt(attempt_id,seq,user_id,rating_key,plex_id,generation,guid,aspect,occurred_at,payload,admitted_at,next_reconcile)
   VALUES(?,?,?,?,?,?,'',?,?,'',0,0) ON CONFLICT(attempt_id) DO NOTHING`, "legacy:"+strconv.FormatInt(row.Seq, 10), row.Seq, row.UserID, int64(row.Key), plexID, row.ConsentGen, string(row.Aspect), row.At.UnixNano()); err != nil {
			return sendPermit{}, err
		}
		if err := tx.Commit(); err != nil {
			return sendPermit{}, err
		}
		return sendPermit{Terminal: "uncertain_legacy"}, nil
	}
	plain, _, err := config.OpenWith(s.sealer, sealed, "plexwatch", "token", strconv.FormatInt(row.UserID, 10), "scrobbles.plex.tv")
	if err != nil {
		return sendPermit{}, err
	}
	payload, err := encodeState(plexID, item)
	if err != nil {
		return sendPermit{}, err
	}
	if s.afterFenceRead != nil {
		s.afterFenceRead()
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM plexwatch_permit WHERE user_id=? AND until_at<=?`, row.UserID, s.now().UnixNano()); err != nil {
		return sendPermit{}, err
	}
	permitID := newID()
	lease := s.now().Add(s.claimFor).UnixNano()
	if _, err := tx.ExecContext(ctx, `INSERT INTO plexwatch_permit(permit_id, user_id, generation, until_at) VALUES(?,?,?,?)`,
		permitID, row.UserID, fenceGen, lease); err != nil {
		return sendPermit{}, err
	}

	if _, err := tx.ExecContext(ctx, `INSERT INTO plexwatch_attempt(attempt_id,seq,user_id,rating_key,plex_id,generation,guid,aspect,occurred_at,payload,admitted_at,next_reconcile)
  VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, permitID, row.Seq, row.UserID, int64(row.Key), plexID, fenceGen, item.GUID, string(row.Aspect), row.At.UnixNano(), string(payload), s.now().UnixNano(), lease); err != nil {
		return sendPermit{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE state_sync_journal SET never_sent=0 WHERE seq=?`, row.Seq); err != nil {
		return sendPermit{}, err
	}
	commit := tx.Commit
	if s.admissionCommit != nil {
		commit = func() error { return s.admissionCommit(tx) }
	}
	if err := commit(); err != nil {
		return sendPermit{}, err
	}

	return sendPermit{ID: permitID, Generation: fenceGen, Token: plain, PlexID: plexID, OK: true}, nil
}

// lockIntent locks the journal row and reports whether attempt still owns it.
// consent is the generation stored on that row. A missing row is not owned.
func (s *Service) lockIntent(ctx context.Context, tx *sql.Tx, seq int64, attempt string) (owned bool, consent int64, err error) {
	q := `SELECT claimed_by, dead, superseded, delivered_at, consent_generation FROM state_sync_journal WHERE seq=?`
	if store.DialectOf(s.db) == store.Postgres {
		q += ` FOR UPDATE`
	}
	var claimed string
	var dead, superseded int
	var delivered int64
	err = tx.QueryRowContext(ctx, q, seq).Scan(&claimed, &dead, &superseded, &delivered, &consent)
	if errors.Is(err, sql.ErrNoRows) {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, err
	}
	owned = claimed == attempt && dead == 0 && superseded == 0 && delivered == 0
	return owned, consent, nil
}

func (s *Service) releaseSend(ctx context.Context, permitID string) {
	if permitID == "" {
		return
	}
	_, _ = s.db.ExecContext(ctx, `DELETE FROM plexwatch_permit WHERE permit_id=?`, permitID)
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// compactSent deletes older echoes for the same title and kind. The newest
// echo of every title stays, so compacting one title cannot make another
// title's own send look like a new remote mark.
func (s *Service) compactSent(ctx context.Context, _ int) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM plexwatch_sent
		WHERE sent_at < (
			SELECT MAX(newer.sent_at) FROM plexwatch_sent newer
			WHERE newer.user_id=plexwatch_sent.user_id AND newer.guid=plexwatch_sent.guid AND newer.kind=plexwatch_sent.kind
		)`)
	return err
}
