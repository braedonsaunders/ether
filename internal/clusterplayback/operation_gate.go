package clusterplayback

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/braedonsaunders/ether/internal/nodewire"
	"github.com/braedonsaunders/ether/internal/placement"
	"github.com/braedonsaunders/ether/internal/remotetranscode"
	"github.com/braedonsaunders/ether/internal/store"
)

const maxArmBytes = 16 << 10
const maxGateBytes = maxAdmissionBytes + 2*maxArmBytes

func init() {
	store.Register("clusterplayback", 4, `
CREATE TABLE clusterplayback_operation_gates (
 cluster TEXT NOT NULL, session TEXT NOT NULL, generation BIGINT NOT NULL,
 gate TEXT NOT NULL, PRIMARY KEY(cluster,session,generation));
`)
}

// All types and methods here are storage-only historical bookkeeping. They do
// not authenticate a Worker handle or observer, issue a permit, authorize a seal,
// prove retirement, or implement an execution veto. No production caller uses
// the mutation methods. A separate reviewed integration must own those checks.
type operationKey struct {
	Cluster, Session string
	Generation       int64
}
type operationBinding struct {
	Version                    int
	Route                      Route
	Operation, AdmissionDigest [32]byte
	Identity                   remotetranscode.Identity
	StartHash                  [32]byte
}
type acceptedAttempt struct {
	Nonce     [32]byte
	Worker    nodewire.Identity
	Identity  remotetranscode.Identity
	StartHash [32]byte
}
type operationSealReason string

const (
	sealCancellation     operationSealReason = "cancellation"
	sealAuthorityLoss    operationSealReason = "authority-loss"
	sealPolicyRevocation operationSealReason = "policy-revocation"
)

type operationSeal struct {
	Actor  nodewire.Identity
	Reason operationSealReason
}

// Payload is an opaque bounded association, never a self-authenticating receipt.
// The eventual observer owns authentication and retained live kernel handles.
type armAssociation struct {
	ID            [32]byte
	BindingDigest [32]byte
	Payload       []byte
}
type operationGate struct {
	Binding  operationBinding
	Seal     *operationSeal
	Accepted *acceptedAttempt
	Arm      *armAssociation
}
type operationGateState uint8

const (
	gateUnknown           operationGateState = iota // cancelled or unreadable
	gateAbsentUnsupported                           // missing/legacy: unknown participation, never never-started
	gateUnclaimed
	gateClaimed
	gateSealedUnclaimed
	gateSealedClaimed
)

type operationHistory struct {
	State operationGateState
	Gate  operationGate
}

func (b operationBinding) key() operationKey {
	return operationKey{b.Route.Source.Cluster, b.Route.Fence.Session, b.Route.Fence.Generation}
}
func admissionBinding(a admissionRecord, raw string) operationBinding {
	// Domain/version plus the exact bounded admission includes explicit keyframes
	// and slate, all original route identities, and reservation operation hash.
	return operationBinding{1, a.Route, a.Operation, sha256.Sum256(append([]byte("clusterplayback:admission:v1\x00"), []byte(raw)...)), a.Identity, a.StartHash}
}
func boundedGateActor(a nodewire.Identity, cluster string) bool {
	return a.Cluster == cluster && len(a.Cluster) > 0 && len(a.Cluster) <= 256 && len(a.NodeID) > 0 && len(a.NodeID) <= 256 && len(a.Boot) > 0 && len(a.Boot) <= 256
}
func (g operationGate) encode() (string, error) {
	b := g.Binding
	if b.Version != 1 || b.key().Cluster == "" || b.key().Session == "" || b.key().Generation <= 0 || b.Identity != remoteIdentity(b.Route) || b.Operation == ([32]byte{}) || b.AdmissionDigest == ([32]byte{}) || b.StartHash == ([32]byte{}) {
		return "", ErrDenied
	}
	if s := g.Seal; s != nil {
		if !boundedGateActor(s.Actor, b.key().Cluster) {
			return "", ErrDenied
		}
		switch s.Reason {
		case sealCancellation, sealAuthorityLoss, sealPolicyRevocation:
		default:
			return "", ErrDenied
		}
	}
	if a := g.Accepted; a != nil {
		if a.Nonce == ([32]byte{}) || a.Identity != b.Identity || a.StartHash != b.StartHash || a.Worker != (nodewire.Identity{Cluster: b.key().Cluster, NodeID: b.Route.Fence.Target.NodeID, Boot: b.Route.Fence.Target.Boot}) {
			return "", ErrDenied
		}
	}
	if a := g.Arm; a != nil {
		if g.Accepted == nil || a.ID == ([32]byte{}) || a.BindingDigest == ([32]byte{}) || len(a.Payload) == 0 || len(a.Payload) > maxArmBytes {
			return "", ErrDenied
		}
	}
	raw, err := json.Marshal(g)
	if err != nil || len(raw) > maxGateBytes {
		return "", ErrDenied
	}
	return string(raw), nil
}
func gateHistory(g operationGate) operationHistory {
	state := gateUnclaimed
	if g.Accepted != nil {
		state = gateClaimed
	}
	if g.Seal != nil {
		state = gateSealedUnclaimed
		if g.Accepted != nil {
			state = gateSealedClaimed
		}
	}
	return operationHistory{state, g}
}

// lockAdmissionHistory orders admission reservation, claim, arm and seal across
// Runtime boots and SQL connections. SQLite takes its write lock BEFORE reads.
func (r *Runtime) lockAdmissionHistory(ctx context.Context, tx *sql.Tx) error {
	if err := store.LockKey(ctx, store.DialectOf(r.o.DB), tx, "clusterplayback:admissions"); err != nil {
		return err
	}
	if store.DialectOf(r.o.DB) == store.SQLite {
		_, err := tx.ExecContext(ctx, `UPDATE clusterplayback_admissions SET generation=generation WHERE 1=0`)
		return err
	}
	return nil
}

type gateReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readGate(ctx context.Context, q gateReader, key operationKey) (operationGate, error) {
	var raw, admission string
	var g operationGate
	// One SQL snapshot joins only this component's own immutable history. A gate
	// absent for a legacy admission stays unknown; it is never backfilled.
	err := q.QueryRowContext(ctx, `SELECT g.gate,a.admission FROM clusterplayback_operation_gates g JOIN clusterplayback_admissions a ON a.cluster=g.cluster AND a.session=g.session AND a.generation=g.generation WHERE g.cluster=? AND g.session=? AND g.generation=?`, key.Cluster, key.Session, key.Generation).Scan(&raw, &admission)
	if err != nil {
		return g, err
	}
	var a admissionRecord
	if len(raw) > maxGateBytes || len(admission) > maxAdmissionBytes || json.Unmarshal([]byte(raw), &g) != nil || json.Unmarshal([]byte(admission), &a) != nil {
		return operationGate{}, ErrDenied
	}
	canonical, err := a.encode()
	if err != nil || canonical != admission || g.Binding != admissionBinding(a, admission) || g.Binding.key() != key {
		return operationGate{}, ErrDenied
	}
	if _, err = g.encode(); err != nil {
		return operationGate{}, err
	}
	return g, nil
}
func (r *Runtime) readOperationGate(ctx context.Context, key operationKey) (operationHistory, error) {
	g, err := readGate(ctx, r.o.DB, key)
	if err != nil {
		state := gateUnknown
		if errors.Is(err, sql.ErrNoRows) {
			state = gateAbsentUnsupported
		}
		return operationHistory{State: state}, &placement.UnknownOutcome{Err: err}
	}
	return gateHistory(g), nil
}

type gateMutation struct {
	Accepted *acceptedAttempt
	Seal     *operationSeal
	Arm      *armAssociation
}

// These private methods return only historical state, including on exact retries
// after seal. Success MUST NOT be interpreted as a new permission to execute.
func (r *Runtime) recordAcceptedAttempt(ctx context.Context, b operationBinding, a acceptedAttempt) (operationHistory, error) {
	return r.mutateOperationGate(ctx, b, gateMutation{Accepted: &a})
}
func (r *Runtime) recordOperationSeal(ctx context.Context, b operationBinding, s operationSeal) (operationHistory, error) {
	return r.mutateOperationGate(ctx, b, gateMutation{Seal: &s})
}
func (r *Runtime) recordArmAssociation(ctx context.Context, b operationBinding, a armAssociation) (operationHistory, error) {
	return r.mutateOperationGate(ctx, b, gateMutation{Arm: &a})
}
func sameArm(a, b *armAssociation) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.ID == b.ID && a.BindingDigest == b.BindingDigest && string(a.Payload) == string(b.Payload)
}
func matchesMutation(g operationGate, m gateMutation) bool {
	return (m.Accepted == nil || g.Accepted != nil && *g.Accepted == *m.Accepted) && (m.Seal == nil || g.Seal != nil && *g.Seal == *m.Seal) && (m.Arm == nil || sameArm(g.Arm, m.Arm))
}
func (r *Runtime) mutateOperationGate(ctx context.Context, b operationBinding, m gateMutation) (operationHistory, error) {
	err := r.writeOperationGate(ctx, b, m)
	return r.reconcileOperationGate(ctx, b, m, err)
}

func (r *Runtime) reconcileOperationGate(ctx context.Context, b operationBinding, m gateMutation, err error) (operationHistory, error) {
	// Independent read after transaction exit, including ambiguous Commit/Exec.
	// Do not detach cancellation or retry a write to manufacture certainty.
	h, readErr := r.readOperationGate(ctx, b.key())
	if readErr != nil {
		return h, readErr
	}
	if h.Gate.Binding != b {
		return operationHistory{State: gateUnknown}, ErrDenied
	}
	if err != nil && !isAdmissionUnknown(err) {
		return h, err
	}
	if !matchesMutation(h.Gate, m) {
		if err != nil {
			return operationHistory{State: gateUnknown}, err
		}
		return operationHistory{State: gateUnknown}, &placement.UnknownOutcome{Err: ErrPending}
	}
	return h, nil
}
func (r *Runtime) writeOperationGate(ctx context.Context, b operationBinding, m gateMutation) (result error) {
	defer func() {
		if result != nil && result != ErrDenied {
			result = &placement.UnknownOutcome{Err: result}
		}
	}()
	tx, err := r.o.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = r.lockAdmissionHistory(ctx, tx); err != nil {
		return err
	}
	g, err := readGate(ctx, tx, b.key())
	if err != nil {
		return err
	}
	if g.Binding != b {
		return ErrDenied
	}
	// Exactly one immutable slot per category, and a single irreversible seal.
	// Existing matching claim/arm is history even after seal; no new slot then.
	if m.Accepted != nil {
		if g.Accepted != nil {
			if *g.Accepted != *m.Accepted {
				return ErrDenied
			}
		} else {
			if g.Seal != nil {
				return ErrDenied
			}
			g.Accepted = m.Accepted
		}
	}
	if m.Arm != nil {
		if g.Arm != nil {
			if !sameArm(g.Arm, m.Arm) {
				return ErrDenied
			}
		} else {
			if g.Seal != nil {
				return ErrDenied
			}
			g.Arm = m.Arm
		}
	}
	if m.Seal != nil {
		if g.Seal != nil && *g.Seal != *m.Seal {
			return ErrDenied
		}
		g.Seal = m.Seal
	}
	raw, err := g.encode()
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE clusterplayback_operation_gates SET gate=? WHERE cluster=? AND session=? AND generation=?`, raw, b.key().Cluster, b.key().Session, b.key().Generation)
	if err != nil {
		return err
	}
	return tx.Commit()
}
