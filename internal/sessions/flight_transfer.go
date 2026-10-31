package sessions

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/handles/capability"
)

// brokerTransferNamespace is the current capability-header primitive for a
// playback flight. It replaces the parked sessions_lazy_leases and
// sessions_lazy_transfers tables: one retirement-owner claim protects the
// source capability, and a second claimed header is the target side.
const brokerTransferNamespace = "broker-transfer"

// errTransferCommitUnknown means Commit failed after the transaction may have
// become durable. Callers must not delete either side or stop the source.
var errTransferCommitUnknown = errors.New("sessions: transfer commit outcome unknown")

// beforeTransferCommit runs inside a transfer transaction immediately before
// Commit. Production leaves it nil.
var beforeTransferCommit func(*sql.Tx) error

type transferSides struct {
	Source capability.Header
	Target capability.Header
}

type capabilityHold struct {
	header  capability.Header
	db      *sql.DB
	refs    int
	done    bool
	failed  bool
	settled bool
}

// prepareFlightTransfer claims the existing source header and admits the target
// header in one transaction. A commit error keeps both sides protected.
func prepareFlightTransfer(ctx context.Context, db *sql.DB, source, target capability.Header, nowMillis int64) (transferSides, error) {
	sides := transferSides{Source: source, Target: target}
	if db == nil {
		return transferSides{}, nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return transferSides{}, err
	}
	defer tx.Rollback()
	claimed, err := capability.ClaimHeaderInTx(ctx, db, tx, source, capability.RetirementOwnerClaim, nowMillis)
	if err != nil {
		return transferSides{}, err
	}
	if !claimed {
		var n int
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM capability_claims WHERE namespace=? AND digest=? AND purpose=?`, source.Namespace, source.Digest, capability.RetirementOwnerClaim).Scan(&n); err != nil {
			return transferSides{}, err
		}
		if n != 1 {
			return transferSides{}, core.ErrNotAvailable
		}
	}
	if err = capability.AdmitHeadersInTx(ctx, db, tx, []capability.Header{target}, capability.Limits{PerScope: 256, PerShard: 4096}); err != nil {
		return transferSides{}, err
	}
	claimed, err = capability.ClaimHeaderInTx(ctx, db, tx, target, capability.RetirementOwnerClaim, nowMillis)
	if err != nil {
		return transferSides{}, err
	}
	if !claimed {
		return transferSides{}, core.ErrNotAvailable
	}
	if beforeTransferCommit != nil {
		if err = beforeTransferCommit(tx); err != nil {
			return transferSides{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return sides, fmt.Errorf("%w: %w", errTransferCommitUnknown, err)
	}
	return sides, nil
}

func dropTransferHeader(ctx context.Context, db *sql.DB, h capability.Header) error {
	if db == nil || h.Digest == "" {
		return nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = capability.LockHeaderScopeInTx(ctx, db, tx, h.HeaderScope); err != nil {
		return err
	}
	if err = capability.DropHeadersInTx(ctx, tx, h.HeaderScope, []string{h.Digest}); err != nil {
		return err
	}
	return tx.Commit()
}

func releaseRetirementClaim(ctx context.Context, db *sql.DB, h capability.Header) error {
	if db == nil || h.Digest == "" {
		return nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = capability.LockHeaderScopeInTx(ctx, db, tx, h.HeaderScope); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM capability_claims WHERE namespace=? AND digest=? AND purpose=?`, h.Namespace, h.Digest, capability.RetirementOwnerClaim); err != nil {
		return err
	}
	return tx.Commit()
}

func (b *Broker) admitSessionBarrier(ctx context.Context, viewer int64, protocol, device, playID, sessionID string) (*capability.Header, error) {
	if b == nil || b.deps.DB == nil || viewer <= 0 || protocol == "" || playID == "" || sessionID == "" {
		return nil, nil
	}
	if len(device) > 512 {
		return nil, core.ErrNotAvailable
	}
	now := b.deps.Now()
	h := capability.Header{HeaderScope: capability.HeaderScope{Namespace: brokerTransferNamespace, ViewerID: viewer, Surface: protocol, DeviceID: device}, Digest: lazyHash(playID + "\x00" + sessionID), ExpiresMillis: now.Add(2 * time.Hour).UnixMilli()}
	tx, err := b.deps.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = capability.AdmitHeadersInTx(ctx, b.deps.DB, tx, []capability.Header{h}, capability.Limits{PerScope: 256, PerShard: 4096}); err != nil {
		return nil, err
	}
	claimed, err := capability.ClaimHeaderInTx(ctx, b.deps.DB, tx, h, capability.RetirementOwnerClaim, now.UnixMilli())
	if err != nil {
		return nil, err
	}
	if !claimed {
		return nil, core.ErrNotAvailable
	}
	if beforeTransferCommit != nil {
		if err = beforeTransferCommit(tx); err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return &h, fmt.Errorf("%w: %w", errTransferCommitUnknown, err)
	}
	return &h, nil
}

// beginLazyFlight protects the source capability for the duration of this
// response and until backend retirement succeeds. The returned function drops
// only this call's target header.
func (b *Broker) beginLazyFlight(ctx context.Context, id string, db *sql.DB, source, target capability.Header) (func(), error) {
	if b == nil || db == nil {
		return func() {}, nil
	}
	b.mu.Lock()
	if b.sessions[id] == nil {
		b.mu.Unlock()
		return nil, core.ErrNotAvailable
	}
	b.mu.Unlock()
	b.holdMu.Lock()
	if b.holds == nil {
		b.holds = make(map[string]*capabilityHold)
	}
	if cur := b.holds[source.Digest]; cur != nil {
		cur.refs++
		b.holdMu.Unlock()
		if _, err := prepareFlightTransfer(ctx, db, source, target, b.deps.Now().UnixMilli()); err != nil {
			b.endLazyHold(source.Digest, false)
			return nil, err
		}
		return b.trackFlight(ctx, id, db, source, target)
	}
	b.holds[source.Digest] = &capabilityHold{header: source, db: db, refs: 1}
	b.holdMu.Unlock()
	if _, err := prepareFlightTransfer(ctx, db, source, target, b.deps.Now().UnixMilli()); err != nil {
		retain := errors.Is(err, errTransferCommitUnknown)
		b.endLazyHold(source.Digest, retain)
		return nil, err
	}
	return b.trackFlight(ctx, id, db, source, target)
}

func (b *Broker) trackFlight(ctx context.Context, id string, db *sql.DB, source, target capability.Header) (func(), error) {
	b.mu.Lock()
	s := b.sessions[id]
	if s == nil {
		b.mu.Unlock()
		_ = dropTransferHeader(context.WithoutCancel(ctx), db, target)
		b.finishHold(source, false)
		return nil, core.ErrNotAvailable
	}
	seen := false
	for _, h := range s.lazyHolds {
		if h.Digest == source.Digest && h.Namespace == source.Namespace {
			seen = true
			break
		}
	}
	if !seen {
		s.lazyHolds = append(s.lazyHolds, source)
	}
	b.mu.Unlock()
	return func() {
		_ = dropTransferHeader(context.WithoutCancel(ctx), db, target)
		b.endLazyHold(source.Digest, false)
	}, nil
}

func (b *Broker) endLazyHold(digest string, retain bool) {
	b.holdMu.Lock()
	cur := b.holds[digest]
	if cur == nil {
		b.holdMu.Unlock()
		return
	}
	if cur.refs > 0 {
		cur.refs--
	}
	if retain {
		cur.failed = true
		cur.done = true
	}
	// The last in-flight response releases the lazy claim so expiry can
	// delete the capability. A failed backend retirement keeps it.
	drop := cur.refs == 0 && !cur.failed && !cur.settled
	if cur.refs == 0 {
		cur.settled = true
		delete(b.holds, digest)
	}
	header, db := cur.header, cur.db
	b.holdMu.Unlock()
	if drop {
		_ = releaseRetirementClaim(context.Background(), db, header)
	}
}

func (b *Broker) finishHold(header capability.Header, ok bool) {
	b.holdMu.Lock()
	if b.holds == nil {
		b.holds = make(map[string]*capabilityHold)
	}
	cur := b.holds[header.Digest]
	if cur == nil {
		cur = &capabilityHold{header: header, db: b.deps.DB}
		b.holds[header.Digest] = cur
	}
	cur.done = true
	if !ok {
		cur.failed = true
	}
	drop := cur.refs == 0 && cur.done && !cur.failed && !cur.settled
	if cur.refs == 0 && cur.done {
		cur.settled = true
		delete(b.holds, header.Digest)
	}
	db := cur.db
	b.holdMu.Unlock()
	if drop && db != nil {
		_ = releaseRetirementClaim(context.Background(), db, header)
	}
}

func (b *Broker) settleRetirement(ctx context.Context, s *brokerSession, ok bool) {
	if b == nil || s == nil {
		return
	}
	for _, h := range s.lazyHolds {
		b.finishHold(h, ok)
	}
	if !ok || s.transferBarrier == nil || b.deps.DB == nil {
		return
	}
	header := *s.transferBarrier
	db := b.deps.DB
	report := b.deps.OnEventError
	// Stop must return while a stalled database still holds its only
	// connection. Loss of the drop leaves the claim in place.
	go func() {
		if err := dropTransferHeader(context.WithoutCancel(ctx), db, header); err != nil && report != nil {
			report(err)
		}
	}()
}
