package clusterplayback

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/nodewire"
	"github.com/braedonsaunders/ether/internal/placement"
	"github.com/braedonsaunders/ether/internal/remotetranscode"
	"github.com/braedonsaunders/ether/internal/store"
)

const maxAdmissionBytes = 256 << 10

// admissionRecord is immutable historical state. It contains neither transport
// credentials nor a permission to execute under a historical controller boot.
// Explicit fields preserve facts intentionally omitted by core's ordinary JSON.
type admissionRecord struct {
	Version   int
	Route     Route
	Operation [32]byte
	Request   core.HLSRequest
	Keyframes []core.KeyframePoint
	SlateText []string
	Identity  remotetranscode.Identity
	StartHash [32]byte
}

func init() {
	store.Register("clusterplayback", 3, `
CREATE TABLE clusterplayback_admissions (
 cluster TEXT NOT NULL, session TEXT NOT NULL, generation BIGINT NOT NULL,
 controller TEXT NOT NULL, controller_boot TEXT NOT NULL, admission TEXT NOT NULL,
 PRIMARY KEY(cluster,session,generation));
CREATE INDEX clusterplayback_admission_quota ON clusterplayback_admissions(cluster,controller,controller_boot);
`)
}

func (a admissionRecord) encode() (string, error) {
	if a.Version != 1 || a.Route.Fence.Generation <= 0 || a.Route.Viewer <= 0 || a.Route.PlayID == "" || a.Route.Revision == "" || a.Route.Source.Cluster == "" || a.Operation == ([32]byte{}) || a.StartHash == ([32]byte{}) || a.Identity != remoteIdentity(a.Route) || a.Request.SessionID != a.Route.Fence.Session || a.Request.InputURL != "" || a.Request.InputHeaders != nil || a.Request.InputLease != nil {
		return "", ErrDenied
	}
	b, err := json.Marshal(a)
	if err != nil || len(b) > maxAdmissionBytes {
		return "", ErrDenied
	}
	return string(b), nil
}

func (r *Runtime) readAdmission(ctx context.Context, route Route) (admissionRecord, error) {
	var raw string
	var a admissionRecord
	err := r.o.DB.QueryRowContext(ctx, `SELECT admission FROM clusterplayback_admissions WHERE cluster=? AND session=? AND generation=?`, route.Source.Cluster, route.Fence.Session, route.Fence.Generation).Scan(&raw)
	if err != nil {
		return a, err
	}
	if len(raw) > maxAdmissionBytes || json.Unmarshal([]byte(raw), &a) != nil || a.Route != route {
		return admissionRecord{}, ErrDenied
	}
	if _, err = a.encode(); err != nil {
		return admissionRecord{}, err
	}
	a.Request.Info.Keyframes = append([]core.KeyframePoint(nil), a.Keyframes...)
	a.Request.SlateText = cloneAdmissionStrings(a.SlateText)
	return a, nil
}

// persistAdmission is called only by the retained adapter's pre-send hook, after
// the actual source binding succeeded. A public request cannot supply its fence,
// operation, authenticated source binding or retained Start hash.
func (r *Runtime) persistAdmission(ctx context.Context, x *execution, id remotetranscode.Identity, hash [32]byte) error {
	r.mu.Lock()
	if r.closed || x.stopping || r.executions[x.route.Fence.Session] != x || x.initial == nil || x.initial.route != x.route || id != remoteIdentity(x.route) {
		r.mu.Unlock()
		return ErrPending
	}
	a := admissionRecord{Version: 1, Route: x.initial.route, Operation: sha256.Sum256([]byte(x.request.Operation)), Request: cloneAdmissionRequest(x.initial.request), Identity: id, StartHash: hash}
	r.mu.Unlock()
	a.Keyframes = append([]core.KeyframePoint(nil), a.Request.Info.Keyframes...)
	a.SlateText = cloneAdmissionStrings(a.Request.SlateText)
	raw, err := a.encode()
	if err != nil {
		return err
	}
	rec, err := r.o.Placement.Lookup(ctx, a.Route.Fence.Session)
	if err != nil {
		return err
	}
	if rec.Fence != a.Route.Fence || rec.Retired || !rec.Authority || sha256.Sum256([]byte(rec.Operation)) != a.Operation {
		return ErrDenied
	}
	writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err = r.insertAdmission(writeCtx, a, raw)
	// Even an ambiguous commit is reconciled only by an independent exact read.
	// No read success means no send, regardless of the SQL error's appearance.
	got, readErr := r.readAdmission(ctx, a.Route)
	if readErr != nil {
		if err != nil {
			return err
		}
		return &placement.UnknownOutcome{Err: readErr}
	}
	confirmed, encodeErr := got.encode()
	if encodeErr != nil || confirmed != raw {
		return ErrDenied
	}
	if err != nil && !isAdmissionUnknown(err) {
		return err
	}
	current, lookupErr := r.o.Placement.Lookup(ctx, a.Route.Fence.Session)
	route, routeErr := r.route(ctx, a.Route.Fence.Session)
	if lookupErr != nil || routeErr != nil || route != a.Route || current.Fence != a.Route.Fence || !current.Authority || current.Retired || sha256.Sum256([]byte(current.Operation)) != a.Operation {
		return ErrPending
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || x.stopping || r.executions[a.Route.Fence.Session] != x || x.initial == nil || x.initial.route != a.Route {
		return ErrPending
	}
	x.initial.commitmentIdentity, x.initial.commitment, x.initial.hasCommitment = id, hash, true
	return nil
}

func isAdmissionUnknown(err error) bool {
	var unknown *placement.UnknownOutcome
	return errors.As(err, &unknown)
}

func (r *Runtime) insertAdmission(ctx context.Context, a admissionRecord, raw string) (result error) {
	defer func() {
		if result != nil && result != ErrDenied && result != ErrPending {
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
	// This is a historical route observation, not a PostgreSQL row lock shared
	// with putRoute. Replacement can race this insert, leaving an inert old row.
	// Immutable generation keys prohibit overwrite; persistAdmission rechecks
	// placement and route after readback before permitting any Start bytes.
	var routeRaw string
	if err = tx.QueryRowContext(ctx, `SELECT route FROM clusterplayback_routes WHERE session=? AND generation=?`, a.Route.Fence.Session, a.Route.Fence.Generation).Scan(&routeRaw); err != nil {
		return err
	}
	var current Route
	if json.Unmarshal([]byte(routeRaw), &current) != nil || current != a.Route {
		return ErrDenied
	}
	var old string
	err = tx.QueryRowContext(ctx, `SELECT admission FROM clusterplayback_admissions WHERE cluster=? AND session=? AND generation=?`, a.Route.Source.Cluster, a.Route.Fence.Session, a.Route.Fence.Generation).Scan(&old)
	if err == nil {
		if old != raw {
			return ErrDenied
		}
		return tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	// The index's cluster prefix bounds retained history across all controller
	// boots, including retired generations. Restart cannot replenish capacity.
	// Exact retries above remain readable even at exhaustion. No eviction exists.
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM clusterplayback_admissions WHERE cluster=?`, a.Route.Source.Cluster).Scan(&count); err != nil {
		return err
	}
	if count >= r.o.HistoryCapacity {
		return ErrPending
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO clusterplayback_admissions(cluster,session,generation,controller,controller_boot,admission) VALUES(?,?,?,?,?,?)`, a.Route.Source.Cluster, a.Route.Fence.Session, a.Route.Fence.Generation, a.Route.Fence.Controller.ID, a.Route.Fence.Controller.Boot, raw); err != nil {
		return err
	}
	gateRaw, err := (operationGate{Binding: admissionBinding(a, raw)}).encode()
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO clusterplayback_operation_gates(cluster,session,generation,gate) VALUES(?,?,?,?)`, a.Route.Source.Cluster, a.Route.Fence.Session, a.Route.Fence.Generation, gateRaw); err != nil {
		return err
	}
	return tx.Commit()
}

// loadRecoveryAdmission is a narrow package-private historical consumer. The
// caller supplies the viewer authenticated by the current broker. It installs
// no execution and never starts, reassigns or renews an old controller boot.
func (r *Runtime) loadRecoveryAdmission(ctx context.Context, route Route, viewer int64) (*initialAdmission, error) {
	if viewer <= 0 || viewer != route.Viewer || route.Source.Cluster != r.o.Self.Cluster || r.o.MemberRole(ctx, r.o.Self, config.RoleServer) != nil {
		return nil, ErrDenied
	}
	a, err := r.readAdmission(ctx, route)
	if err != nil {
		return nil, err
	}
	rec, err := r.o.Placement.Lookup(ctx, route.Fence.Session)
	current, routeErr := r.route(ctx, route.Fence.Session)
	if err != nil || routeErr != nil || current != route || rec.Fence != route.Fence || sha256.Sum256([]byte(rec.Operation)) != a.Operation {
		return nil, ErrDenied
	}
	if _, err = r.sourceCall(ctx, route, sourceRequest{Op: "recovery-admission-policy"}); err != nil {
		return nil, err
	}
	// The remote policy callback can block or race replacement. Historical reads
	// fail closed if the indexed route/operation changed while it ran.
	rec, err = r.o.Placement.Lookup(ctx, route.Fence.Session)
	current, routeErr = r.route(ctx, route.Fence.Session)
	if err != nil || routeErr != nil || current != route || rec.Fence != route.Fence || sha256.Sum256([]byte(rec.Operation)) != a.Operation {
		return nil, ErrDenied
	}
	return &initialAdmission{route: route, request: cloneAdmissionRequest(a.Request), commitmentIdentity: a.Identity, commitment: a.StartHash, hasCommitment: true}, nil
}

// recoveryAdmissionPolicy authorizes only reading historical admission. It does
// not call sourcePolicy, which intentionally requires the original controller's
// current membership for execution. The new authenticated server is checked here.
func (r *Runtime) recoveryAdmissionPolicy(ctx context.Context, peer nodewire.Identity, route Route) (sourceResponse, error) {
	if peer.Cluster != r.o.Self.Cluster || route.Source != r.o.Self || r.o.MemberRole(ctx, peer, config.RoleServer) != nil || r.o.MemberRole(ctx, r.o.Self, config.RoleSource) != nil || r.o.ActiveViewer == nil || r.o.SourcePolicy == nil || r.o.Playback == nil {
		return sourceResponse{}, ErrDenied
	}
	r.mu.Lock()
	source := r.sources[sourceKey(route.Fence)]
	valid := !r.closed && source != nil && source.route == route && !source.binding && source.handle.Value() != ""
	r.mu.Unlock()
	if !valid {
		return sourceResponse{}, ErrDenied
	}
	viewer, err := r.o.ActiveViewer(ctx, route.Viewer)
	if err != nil || viewer == nil || viewer.ID != route.Viewer {
		return sourceResponse{}, ErrDenied
	}
	ps, ok := r.o.Playback.Session(route.PlayID)
	if !ok || ps == nil || ps.UserID != route.Viewer || core.SourceRevision(ps) != route.Revision || r.o.SourcePolicy(ctx, viewer, ps) != nil {
		return sourceResponse{}, ErrDenied
	}
	// Policy is external code: re-fetch durable identity and the retained binding
	// afterwards, and recheck new peer membership. No callback runs under mu.
	rec, err := r.o.Placement.Lookup(ctx, route.Fence.Session)
	current, routeErr := r.route(ctx, route.Fence.Session)
	if err != nil || routeErr != nil || rec.Fence != route.Fence || current != route || r.o.MemberRole(ctx, peer, config.RoleServer) != nil {
		return sourceResponse{}, ErrDenied
	}
	ps, ok = r.o.Playback.Session(route.PlayID)
	if !ok || ps == nil || ps.UserID != route.Viewer || core.SourceRevision(ps) != route.Revision {
		return sourceResponse{}, ErrDenied
	}
	r.mu.Lock()
	valid = !r.closed && r.sources[sourceKey(route.Fence)] == source && source.route == route && !source.binding && source.handle.Value() != ""
	r.mu.Unlock()
	if !valid {
		return sourceResponse{}, ErrDenied
	}
	return sourceResponse{}, nil
}
