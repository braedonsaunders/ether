package clusterplayback

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/nodewire"
	"github.com/braedonsaunders/ether/internal/store"
)

// PreparationReceipt proves closure only of one source preparation operation.
// It is not placement, encoder, reopen or immutable-media equivalence evidence.
// Media fields contain actual captured facts when known; an error-owned lifetime
// can finish without ever returning media. Its exact opaque lifetime identity
// and immutable request hash remain bound to the terminal operation.
type PreparationReceipt struct {
	Version                    uint8
	Source, Controller         nodewire.Identity
	OperationHash, RequestHash [32]byte
	LifetimeID                 string
	Kind                       string // source-retired or no-source-barrier; never inferred from absence
	PlayID, Revision           string
	Size                       int64
}

func preparationHash(operation string) [32]byte {
	return sha256.Sum256(append([]byte("ether/preparation-operation/v1\x00"), operation...))
}
func preparationArgs(source, controller nodewire.Identity, operation [32]byte) []any {
	return []any{source.Cluster, source.NodeID, source.Boot, controller.NodeID, controller.Boot, hex.EncodeToString(operation[:])}
}
func validPreparationIdentity(id nodewire.Identity) bool {
	return id.Cluster != "" && len(id.Cluster) <= 128 && id.NodeID != "" && len(id.NodeID) <= 128 && id.Boot != "" && len(id.Boot) <= 128
}
func (receipt PreparationReceipt) encode() (string, error) {
	if receipt.Version != 1 || !validPreparationIdentity(receipt.Source) || !validPreparationIdentity(receipt.Controller) || receipt.Source.Cluster != receipt.Controller.Cluster || receipt.OperationHash == ([32]byte{}) || receipt.LifetimeID == "" || len(receipt.LifetimeID) > 128 || len(receipt.PlayID) > 256 || len(receipt.Revision) > 1024 || receipt.Size < 0 {
		return "", ErrDenied
	}
	if receipt.Kind != "source-retired" && receipt.Kind != "no-source-barrier" {
		return "", ErrDenied
	}
	if receipt.Kind == "no-source-barrier" && (receipt.PlayID != "" || receipt.Revision != "" || receipt.Size != 0) {
		return "", ErrDenied
	}
	if receipt.Kind == "source-retired" && receipt.RequestHash == ([32]byte{}) {
		return "", ErrDenied
	}
	raw, err := json.Marshal(receipt)
	if err != nil || len(raw) > 4096 {
		return "", ErrDenied
	}
	return string(raw), nil
}

// LookupPreparationReceipt reads only exact immutable trusted publication. Old
// source membership is intentionally not required: the source boot may have
// safely rolled after publication. The requesting controller is always Self.
func (r *Runtime) LookupPreparationReceipt(ctx context.Context, owner nodewire.Identity, operation string) (PreparationReceipt, error) {
	if !validPreparationIdentity(owner) || owner.Cluster != r.o.Self.Cluster || operation == "" || len(operation) > 128 || r.o.MemberRole(ctx, r.o.Self, config.RoleServer) != nil {
		return PreparationReceipt{}, ErrDenied
	}
	return r.readPreparationReceipt(ctx, owner, r.o.Self, preparationHash(operation))
}
func (r *Runtime) readPreparationReceipt(ctx context.Context, owner, controller nodewire.Identity, operation [32]byte) (PreparationReceipt, error) {
	var raw string
	err := r.o.DB.QueryRowContext(ctx, `SELECT receipt FROM clusterplayback_preparation_receipts WHERE cluster=? AND source=? AND source_boot=? AND controller=? AND controller_boot=? AND operation_hash=?`, preparationArgs(owner, controller, operation)...).Scan(&raw)
	if err != nil {
		return PreparationReceipt{}, err
	}
	var receipt PreparationReceipt
	if len(raw) > 4096 || json.Unmarshal([]byte(raw), &receipt) != nil || receipt.Source != owner || receipt.Controller != controller || receipt.OperationHash != operation {
		return PreparationReceipt{}, ErrDenied
	}
	canonical, err := receipt.encode()
	if err != nil || canonical != raw || owner.Cluster != r.o.Self.Cluster {
		return PreparationReceipt{}, ErrDenied
	}
	return receipt, nil
}
func (r *Runtime) PreparedSourceRetired(ctx context.Context, owner nodewire.Identity, operation string) (bool, error) {
	_, err := r.LookupPreparationReceipt(ctx, owner, operation)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (r *Runtime) preparationRetiredResponse(ctx context.Context, entry *preparation) (sourceResponse, error) {
	if err := r.publishPreparationReceipt(ctx, entry); err != nil {
		return sourceResponse{}, err
	}
	return sourceResponse{Retired: true, Phase: "retired"}, nil
}

// No caller mutex is held across backend proof getters, revocation or SQL.
func (r *Runtime) publishPreparationReceipt(ctx context.Context, entry *preparation) error {
	r.mu.Lock()
	if entry.receipt {
		r.mu.Unlock()
		return nil
	}
	if entry.running || entry.operations != 0 {
		r.mu.Unlock()
		return ErrPending
	}
	life := entry.life
	negative := entry.noAcquisition
	r.mu.Unlock()
	if life == nil && !negative {
		return ErrPending
	}
	if life != nil {
		select {
		case <-life.Done():
			if life.Err() != nil {
				return ErrPending
			}
		default:
			return ErrPending
		}
	}
	r.mu.Lock()
	if entry.running || entry.operations != 0 || entry.life != life {
		r.mu.Unlock()
		return ErrPending
	}
	// Every future replay/use/bind checks this exact barrier before acquisition.
	entry.retiring = true
	var bindings []*ownedSource
	for _, source := range r.sources {
		if life != nil && (source.life == life || (entry.result.PlayID != "" && source.route.PlayID == entry.result.PlayID && source.route.Source == r.o.Self)) {
			source.retiring = true
			bindings = append(bindings, source)
		}
	}
	receipt := PreparationReceipt{Version: 1, Source: r.o.Self, Controller: entry.peer, OperationHash: preparationHash(entry.request.Operation), RequestHash: entry.hash, LifetimeID: entry.lifetimeID, Kind: "no-source-barrier"}
	if life != nil {
		receipt.Kind = "source-retired"
		receipt.PlayID = entry.result.PlayID
		receipt.Revision = entry.capturedRevision
		receipt.Size = entry.capturedSize
	}
	r.mu.Unlock()
	for _, source := range bindings {
		r.mu.Lock()
		busy := source.binding || source.issuing || source.operations != 0
		handle, playID, route := source.handle, source.playID, source.route
		r.mu.Unlock()
		if busy {
			return ErrPending
		}
		if handle.Value() != "" && r.authority.Revoke(handle) != nil {
			return ErrPending
		}
		if r.o.Playback.RetireSource(playID, life) != nil {
			return ErrPending
		}
		r.mu.Lock()
		source.sealed = true
		r.mu.Unlock()
		if !r.sourceJoined(source) || r.bytes.Pending(scopeForRoute(route)) != 0 {
			return ErrPending
		}
	}
	raw, err := receipt.encode()
	if err != nil {
		return err
	}
	// The operation barrier is permanent; a failed/unknown write retains the
	// exact payload and pending obligation until immutable read reconciliation.
	if old, err := r.readPreparationReceipt(ctx, receipt.Source, receipt.Controller, receipt.OperationHash); err == nil {
		encoded, _ := old.encode()
		if encoded != raw {
			return ErrDenied
		}
	} else {
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		tx, err := r.o.DB.BeginTx(writeCtx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err = store.LockKey(writeCtx, store.DialectOf(r.o.DB), tx, "clusterplayback:preparation-receipts"); err != nil {
			return err
		}
		// Bound retained history per source boot independently of concurrency.
		var count int
		if err = tx.QueryRowContext(writeCtx, `SELECT COUNT(*) FROM clusterplayback_preparation_receipts WHERE cluster=? AND source=? AND source_boot=?`, receipt.Source.Cluster, receipt.Source.NodeID, receipt.Source.Boot).Scan(&count); err != nil {
			return err
		}
		if count >= r.o.HistoryCapacity {
			return ErrPending
		}
		args := append(preparationArgs(receipt.Source, receipt.Controller, receipt.OperationHash), raw)
		if _, err = tx.ExecContext(writeCtx, `INSERT INTO clusterplayback_preparation_receipts(cluster,source,source_boot,controller,controller_boot,operation_hash,receipt) VALUES(?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`, args...); err != nil {
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		confirmed, err := r.readPreparationReceipt(ctx, receipt.Source, receipt.Controller, receipt.OperationHash)
		if err != nil {
			return err
		}
		encoded, _ := confirmed.encode()
		if encoded != raw {
			return ErrDenied
		}
	}
	r.mu.Lock()
	entry.receipt = true
	r.mu.Unlock()
	return nil
}

func (r *Runtime) persistPreparationReceipts(ctx context.Context) error {
	r.mu.Lock()
	var entries []*preparation
	for _, entry := range r.preparations {
		if !entry.receipt {
			entries = append(entries, entry)
		}
	}
	r.mu.Unlock()
	for _, entry := range entries {
		if err := r.publishPreparationReceipt(ctx, entry); err != nil && !errors.Is(err, ErrPending) {
			return err
		}
	}
	return nil
}
