package sessions

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/store"
)

type receiptProgress interface {
	ProgressWithReceipt(context.Context, int64, core.RatingKey, time.Duration, time.Duration, string, func(*sql.Tx) error) error
}
type sampleReportKey struct{}
type sampleReport struct {
	sample  frontend.ProgressSample
	outcome frontend.ReportOutcome
}

var errSampleNotApplied = errors.New("sessions: sample not applied")

// ReportSample uses the same policy and ownership gate as ordinary reporting,
// but requires a synchronous receipt writer instead of the availability outbox.
func (b *Broker) ReportSample(ctx context.Context, a frontend.SessionAccess, id string, sample frontend.ProgressSample) (frontend.ReportOutcome, error) {
	if a.Protocol != "silo" || !canonicalUUID(sample.InstallationID) || sample.Sequence == 0 || math.IsNaN(sample.PositionSeconds) || math.IsInf(sample.PositionSeconds, 0) || sample.PositionSeconds < 0 || sample.PositionSeconds >= float64(math.MaxInt64)/float64(time.Second) {
		return "", core.ErrUnauthorized
	}
	if _, ok := b.deps.Progress.(receiptProgress); !ok || b.deps.DB == nil {
		return "", core.ErrNotAvailable
	}
	// Do not forward a lossy Timeline to another node: it has no fingerprint.
	s, _, err := b.access(ctx, a, id)
	if err != nil {
		return "", err
	}
	if s.public.ClientSessionID != sample.InstallationID {
		return "", core.ErrUnauthorized
	}
	if s.live || s.public.Item.Kind() == core.KindExtra {
		return "", core.ErrNotAvailable
	}
	if sample.PositionSeconds == 0 {
		sample.PositionSeconds = 0
	}
	report := &sampleReport{sample: sample, outcome: frontend.ReportStale}
	status := "playing"
	if sample.Paused {
		status = "paused"
	}
	if sample.Stop {
		status = "stopped"
	}
	err = b.Report(context.WithValue(ctx, sampleReportKey{}, report), a, id, frontend.Timeline{PlaybackID: id, ClientSessionID: sample.InstallationID, Position: time.Duration(sample.PositionSeconds * float64(time.Second)), State: status})
	if errors.Is(err, errSampleNotApplied) {
		return report.outcome, nil
	}
	if err != nil {
		return "", err
	}
	return report.outcome, nil
}

func (b *Broker) sampleReceipt(ctx context.Context, s *brokerSession, report *sampleReport) func(*sql.Tx) error {
	return func(tx *sql.Tx) error {
		if err := store.LockKey(ctx, store.DialectOf(b.deps.DB), tx, "sessions:sample-receipts"); err != nil {
			return err
		}
		if b.cluster != nil {
			result, err := tx.ExecContext(ctx, `UPDATE plex_sessions SET active=active WHERE id=? AND node_id=? AND active=1`, s.public.ID, b.cluster.store.node)
			if err != nil {
				return err
			}
			n, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if n != 1 {
				return errSampleNotApplied
			}
		}
		encoded := fmt.Sprintf("%020d", report.sample.Sequence)
		fingerprint := fmt.Sprintf("%016x/%t", math.Float64bits(report.sample.PositionSeconds), report.sample.Paused)
		var oldSeq, oldFingerprint, installation string
		err := tx.QueryRowContext(ctx, `SELECT sequence,fingerprint,installation_id FROM sessions_sample_receipts WHERE session_id=?`, s.public.ID).Scan(&oldSeq, &oldFingerprint, &installation)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			if installation != report.sample.InstallationID {
				return core.ErrUnauthorized
			}
			if oldSeq > encoded {
				report.outcome = frontend.ReportStale
				return errSampleNotApplied
			}
			if oldSeq == encoded {
				report.outcome = frontend.ReportConflict
				if oldFingerprint == fingerprint {
					report.outcome = frontend.ReportReplayed
				}
				return errSampleNotApplied
			}
		}
		if errors.Is(err, sql.ErrNoRows) {
			var count int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions_sample_receipts`).Scan(&count); err != nil {
				return err
			}
			if count >= b.sampleLimit {
				return core.ErrNotAvailable
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO sessions_sample_receipts(session_id,installation_id,sequence,fingerprint) VALUES(?,?,?,?) ON CONFLICT(session_id) DO UPDATE SET sequence=excluded.sequence,fingerprint=excluded.fingerprint`, s.public.ID, report.sample.InstallationID, encoded, fingerprint)
		return err
	}
}

// Once local access is irrevocably removed, a receipt can be retired. Cluster
// uncertainty retains it. Crash orphans stay charged against the hard cap;
// there is deliberately no age-based sweep that could erase active ordering.
func (b *Broker) cleanupSampleReceipt(ctx context.Context, s *brokerSession) {
	if s.public.Protocol != "silo" || b.deps.DB == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	tx, err := b.deps.DB.BeginTx(ctx, nil)
	if err == nil {
		defer tx.Rollback()
		if b.cluster != nil {
			_, err = tx.ExecContext(ctx, `UPDATE plex_sessions SET active=active WHERE id=?`, s.public.ID)
			if err == nil {
				var active int
				err = tx.QueryRowContext(ctx, `SELECT active FROM plex_sessions WHERE id=?`, s.public.ID).Scan(&active)
				if errors.Is(err, sql.ErrNoRows) {
					err = nil
				} else if err == nil && active != 0 {
					return
				}
			}
		}
		if err == nil {
			_, err = tx.ExecContext(ctx, `DELETE FROM sessions_sample_receipts WHERE session_id=?`, s.public.ID)
		}
		if err == nil {
			err = tx.Commit()
		}
	}
	if err != nil && b.deps.OnEventError != nil {
		b.deps.OnEventError(err)
	}
}
