package remotetranscode

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"strconv"
	"time"
)

// AcceptedStartHandle is a Worker-owned local capability for one retained actual
// acceptance. Only Worker.AcceptedStart can return a usable handle. Zero values,
// caller allocations, copied objects and deserialized values are invalid. It is
// not a wire DTO, execution permit, authenticated host binding or retirement proof.
// Retain the returned pointer; neither a snapshot nor a nonce can recreate it.
type AcceptedStartHandle struct {
	worker *Worker
	record *execution
	id     Identity
	hash   [32]byte
	nonce  string
}

// AcceptedStartSnapshot is detached observation data, never a usable handle.
// Identity is the complete transport identity, not the full durable cluster fence.
// AttemptNonce is unique within the required globally unique Worker boot; it is
// an identifier, not a secret or an authenticity proof. Lease is accepted history,
// not proof of current placement/source authority. Starting includes an owned
// attempt whose source/backend callback or Start response has not completed.
type AcceptedStartSnapshot struct {
	Identity        Identity
	StartHash       [32]byte
	AttemptNonce    string
	InitialDeadline time.Time
	Lease           LeaseState
	Starting        bool
}

// Called under mu, before installation or any source/backend work. All records
// (including tombstones) are retained for this unique boot, so the next ordinal
// cannot repeat. len is bounded by Capacity (an int), hence +1 cannot overflow
// uint64. There is no random/counter failure path that could lose owned work.
func (w *Worker) newAcceptedStart(e *execution) *AcceptedStartHandle {
	return &AcceptedStartHandle{worker: w, record: e, id: e.id, hash: e.hash,
		nonce: w.opts.Boot + "/" + strconv.FormatUint(uint64(len(w.records))+1, 10)}
}

// AcceptedStart observes only an actual retained acceptance, including starting
// attempts and uncertain/blocked Start responses. Absence and negative tombstones
// are unknown. No authorization callback, preflight, expiry, Resolve, backend,
// renewal, stop, claim, arm or retirement operation is invoked here.
func (w *Worker) AcceptedStart(ctx context.Context, id Identity) (*AcceptedStartHandle, error) {
	if w == nil || ctx == nil || !validIdentity(id) || id.Worker != w.opts.Boot {
		return nil, ErrUnsupported
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	e := w.records[id]
	if e == nil || e.id != id {
		return nil, ErrUncertain
	}
	if _, err := w.inspectAcceptedStart(ctx, e.acceptedStart); err != nil {
		return nil, err
	}
	return e.acceptedStart, nil
}

// InspectAcceptedStart revalidates the exact Worker, handle pointer, live retained
// record, boot, immutable accepted hash and lease history on every call. A prior
// successful snapshot grants no future validity. It performs no state transition
// and copies all mutable data under mu without calling backend getters.
func (w *Worker) InspectAcceptedStart(ctx context.Context, h *AcceptedStartHandle) (AcceptedStartSnapshot, error) {
	if w == nil || ctx == nil {
		return AcceptedStartSnapshot{}, ErrUnsupported
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.inspectAcceptedStart(ctx, h)
}

// mu held throughout validation and copying. Expired observations fail closed
// without running expire or stopping any retained ownership.
func (w *Worker) inspectAcceptedStart(ctx context.Context, h *AcceptedStartHandle) (AcceptedStartSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return AcceptedStartSnapshot{}, err
	}
	if h == nil || h.worker != w || h.record == nil || !validIdentity(h.id) || h.id.Worker != w.opts.Boot {
		return AcceptedStartSnapshot{}, ErrUnsupported
	}
	e := h.record
	if w.closed || w.records[h.id] != e || e.acceptedStart != h || e.id != h.id || e.start.Identity != h.id || e.origin != EvidenceAcceptedStart || e.negativeBarrierGeneration != 0 || e.stopped || e.failed || w.expired(e, time.Now()) || e.monotonicDeadline.IsZero() || h.nonce == "" || e.hash == ([32]byte{}) || e.hash != h.hash {
		return AcceptedStartSnapshot{}, ErrUncertain
	}
	encoded, err := json.Marshal(e.start)
	if err != nil || sha256.Sum256(encoded) != h.hash || e.start.Expires.IsZero() || e.lease.Deadline.Before(e.start.Expires) || e.lease.Renewable != (w.opts.ValidateRenewal != nil) {
		return AcceptedStartSnapshot{}, ErrUncertain
	}
	lease := e.lease
	if lease.Version == 0 {
		if lease.Accepted != nil || !lease.Deadline.Equal(e.start.Expires) {
			return AcceptedStartSnapshot{}, ErrUncertain
		}
	} else {
		n := lease.Accepted
		if !lease.Renewable || n == nil || !validRenewal(*n) || n.Identity != h.id || n.Operation != lease.Version || !n.Deadline.Equal(lease.Deadline) || n.PreviousDeadline.Before(e.start.Expires) || (n.PreviousVersion == 0 && !n.PreviousDeadline.Equal(e.start.Expires)) {
			return AcceptedStartSnapshot{}, ErrUncertain
		}
		copy := *n
		lease.Accepted = &copy
	}
	if err := ctx.Err(); err != nil {
		return AcceptedStartSnapshot{}, err
	}
	return AcceptedStartSnapshot{Identity: h.id, StartHash: h.hash, AttemptNonce: h.nonce,
		InitialDeadline: e.start.Expires.UTC().Round(0), Lease: lease, Starting: e.starting}, nil
}
