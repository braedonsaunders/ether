package playback

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

type resumeExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}
type nzbLease struct {
	hash  string
	owner payloadOwner
}

var errResumeCommitUnknown = errors.New("playback: resume commit outcome unknown")

func (s *resumeStore) writeBundle(ctx context.Context, rec resumeRecord, lease *nzbLease, points []core.KeyframePoint, probe core.MediaInfo, nzb []byte) error {
	if lease == nil || lease.owner.state == nil {
		return core.ErrNotAvailable
	}
	lease.owner.state.mu.Lock()
	defer lease.owner.state.mu.Unlock()
	if lease.owner.state.released {
		return core.ErrNotAvailable
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := payloadLock(ctx, s.db, tx, s.payload.domain, lease.hash); err != nil {
		return err
	}
	if err := store.LockKey(ctx, store.DialectOf(s.db), tx, fmt.Sprintf("playback:resume:%d:%d", rec.userID, rec.key)); err != nil {
		return err
	}
	// A replay after verified incarnation recovery cannot recreate a reference
	// to a file whose writer responsibility has already been retired.
	var owner int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM playback_payload_owners WHERE namespace=? AND digest=? AND domain=? AND incarnation=? AND payload_hash=?`, payloadNamespace, lease.owner.header.Digest, lease.owner.domain, lease.owner.incarnation, lease.hash).Scan(&owner); err != nil {
		return err
	}
	if owner != 1 {
		return core.ErrNotAvailable
	}
	oldHash, err := saveResumeRowTx(ctx, tx, rec, lease.hash)
	if err != nil {
		return err
	}
	if err := saveIndexWith(ctx, tx, rec.userID, rec.key, rec.release.ID, points); err != nil {
		return err
	}
	if err := saveProbeWith(ctx, tx, rec.userID, rec.key, rec.release.ID, nzb, probe, rec.updatedAt); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%w: %w", errResumeCommitUnknown, err)
	}
	if oldHash != "" && oldHash != lease.hash {
		s.gcNZB(ctx, oldHash)
	}
	return nil
}
