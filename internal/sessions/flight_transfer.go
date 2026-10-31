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

// transferViewerCap is abuse protection for one viewer. It counts distinct
// live streams. Repeated plays of the same title and device reuse one ticket,
// and an idle ticket is evicted when a new stream needs the slot.
const transferViewerCap = 256

func (b *Broker) admitSessionBarrier(ctx context.Context, viewer int64, protocol, device string, item core.RatingKey, playID, sessionID string) (*capability.Header, error) {
	if b == nil || b.deps.DB == nil || viewer <= 0 || protocol == "" || playID == "" || sessionID == "" {
		return nil, nil
	}
	if len(device) > 512 {
		return nil, core.ErrNotAvailable
	}
	if reused := b.liveTransferTicket(viewer, protocol, device, item); reused != nil {
		held, err := b.renewTransferTicket(ctx, *reused)
		if err != nil || held {
			return reused, err
		}
	}
	now := b.deps.Now()
	h := capability.Header{HeaderScope: capability.HeaderScope{Namespace: brokerTransferNamespace, ViewerID: viewer, Surface: protocol, DeviceID: device}, Digest: lazyHash(playID + "\x00" + sessionID), ExpiresMillis: now.Add(2 * time.Hour).UnixMilli()}
	tx, err := b.deps.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = b.evictIdleTransferTickets(ctx, tx, viewer, protocol); err != nil {
		return nil, err
	}
	if err = capability.AdmitHeadersInTx(ctx, b.deps.DB, tx, []capability.Header{h}, capability.Limits{PerScope: transferViewerCap, PerShard: 4096}); err != nil {
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

// liveTransferTicket is the admission ticket a live session already holds for
// this viewer, surface, device and title. Part reads and transcode starts of
// that stream reuse it instead of minting another.
func (b *Broker) liveTransferTicket(viewer int64, protocol, device string, item core.RatingKey) *capability.Header {
	if b == nil || item == 0 {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	var found *capability.Header
	var started time.Time
	for _, s := range b.sessions {
		if s == nil || s.transferBarrier == nil || s.transferBarrier.Digest == "" {
			continue
		}
		if s.public.ViewerID != viewer || string(s.public.Protocol) != protocol || s.public.Device.ID != device || s.public.Item != item {
			continue
		}
		if found == nil || s.public.StartedAt.After(started) {
			cp := *s.transferBarrier
			found = &cp
			started = s.public.StartedAt
		}
	}
	return found
}

func (b *Broker) transferHeld(digest string) bool {
	if digest == "" {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, s := range b.sessions {
		if s != nil && s.transferBarrier != nil && s.transferBarrier.Digest == digest {
			return true
		}
	}
	return false
}

func (b *Broker) liveTransferDigests() map[string]bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	live := make(map[string]bool)
	for _, s := range b.sessions {
		if s != nil && s.transferBarrier != nil && s.transferBarrier.Digest != "" {
			live[s.transferBarrier.Digest] = true
		}
	}
	return live
}

// renewTransferTicket extends the live ticket. held is false when the row is
// already gone, so the caller mints a replacement.
func (b *Broker) renewTransferTicket(ctx context.Context, h capability.Header) (bool, error) {
	now := b.deps.Now()
	h.ExpiresMillis = now.Add(2 * time.Hour).UnixMilli()
	tx, err := b.deps.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	ok, err := capability.RenewHeaderInTx(ctx, b.deps.DB, tx, h, now.UnixMilli())
	if err != nil || !ok {
		return false, err
	}
	return true, tx.Commit()
}

// evictIdleTransferTickets frees the oldest tickets that no live session holds
// until this viewer has room for one more stream. A full set of live streams
// is still refused.
func (b *Broker) evictIdleTransferTickets(ctx context.Context, tx *sql.Tx, viewer int64, surface string) error {
	scope := capability.HeaderScope{Namespace: brokerTransferNamespace, ViewerID: viewer, Surface: surface}
	if err := capability.LockHeaderScopeInTx(ctx, b.deps.DB, tx, scope); err != nil {
		return err
	}
	live := b.liveTransferDigests()
	rows, err := tx.QueryContext(ctx, `SELECT digest, device_id FROM capability_headers WHERE namespace=? AND shard=? AND viewer_id=? AND surface=? ORDER BY expires ASC, digest ASC`, brokerTransferNamespace, viewer%capability.HeaderShardCount, viewer, surface)
	if err != nil {
		return err
	}
	defer rows.Close()
	type ticket struct{ digest, device string }
	var all, idle []ticket
	for rows.Next() {
		var row ticket
		if err = rows.Scan(&row.digest, &row.device); err != nil {
			return err
		}
		all = append(all, row)
		if !live[row.digest] {
			idle = append(idle, row)
		}
	}
	if err = rows.Err(); err != nil {
		return err
	}
	excess := len(all) + 1 - transferViewerCap
	if excess <= 0 {
		return nil
	}
	if len(idle) < excess {
		return &capability.CapacityError{Scope: "viewer"}
	}
	byDevice := make(map[string][]string)
	for _, row := range idle[:excess] {
		byDevice[row.device] = append(byDevice[row.device], row.digest)
	}
	for device, digests := range byDevice {
		dropScope := scope
		dropScope.DeviceID = device
		for start := 0; start < len(digests); start += capability.HeaderBatchMax {
			end := min(start+capability.HeaderBatchMax, len(digests))
			if err = capability.DropHeadersInTx(ctx, tx, dropScope, digests[start:end]); err != nil {
				return err
			}
		}
	}
	return nil
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
		// A replacement session for this title already holds the ticket.
		if b.transferHeld(header.Digest) {
			return
		}
		if err := dropTransferHeader(context.WithoutCancel(ctx), db, header); err != nil && report != nil {
			report(err)
		}
	}()
}
