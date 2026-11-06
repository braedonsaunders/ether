package clusterplayback

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/nodewire"
	"github.com/braedonsaunders/ether/internal/placement"
	"github.com/braedonsaunders/ether/internal/remotetranscode"
	"net/http"
	"time"
)

type execution struct {
	initial                       *initialAdmission
	output                        *bufferedSession
	foreign                       bool
	done                          chan struct{}
	busy, reservePending, retired bool
	route                         Route
	request                       placement.Request
	adapter                       *remotetranscode.Adapter
	session                       *remotetranscode.Session
	retirement                    core.EncoderRetirement
	stopping                      bool
	reserved                      bool
}

// Supports is a side-effect-free admission gate. Unsupported requests must use
// the existing local backend before any distributed ownership is allocated.
func Supports(q core.HLSRequest) bool { return supportedRequest(q, remotetranscode.Supported()) }
func supportedRequest(q core.HLSRequest, caps remotetranscode.Capabilities) bool {
	if !remotetranscode.SupportsRequest(q, caps) || len(q.SessionID) == 0 || len(q.SessionID) > 128 {
		return false
	}
	for _, c := range q.SessionID {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

// StartDistributedHLS starts from the exact source already prepared by the
// authenticated broker. ErrUnsupported is returned exclusively before side effects.
func (r *Runtime) StartDistributedHLS(ctx context.Context, viewer int64, playID, revision string, q core.HLSRequest) (core.HLSSession, error) {
	r.mu.Lock()
	remote := r.remotePlayback
	r.mu.Unlock()
	if remote != nil {
		if prepared, ok := remote.prepared(playID); ok {
			if prepared.Revision != revision {
				return nil, core.ErrSourceChanged
			}
			return r.StartPreparedHLS(ctx, viewer, prepared, q)
		}
	}
	return r.StartPreparedHLS(ctx, viewer, PreparedSource{Owner: r.o.Self, PlayID: playID, Revision: revision}, q)
}

// StartPreparedHLS composes a concrete remote source-owner preparation with the
// same durable encoder placement. The broker's remote planning seam can use it.
func (r *Runtime) StartPreparedHLS(ctx context.Context, viewer int64, prepared PreparedSource, q core.HLSRequest) (core.HLSSession, error) {
	q = cloneAdmissionRequest(q)
	playID, revision := prepared.PlayID, prepared.Revision
	if r.o.Eligible(ctx, r.o.Self, config.RoleServer) != nil || r.o.Eligible(ctx, prepared.Owner, config.RoleSource) != nil {
		return nil, ErrDenied
	}
	if !r.Supports(q) {
		return nil, remotetranscode.ErrUnsupported
	}
	if q.RecoveryStart != nil && (q.RecoveryStart.Audio == nil || q.RecoveryStart.Audio.SourceRevision != revision) {
		// A transported expectation cannot substitute another immutable source.
		// Actual same-lease AU validation and held output proof remain mandatory.
		return nil, remotetranscode.ErrUnsupported
	}
	if r.o.Decide == nil || viewer <= 0 || revision == "" {
		return nil, ErrDenied
	}
	r.mu.Lock()
	if r.closed || (r.executions[q.SessionID] != nil && !r.executions[q.SessionID].retired) || (r.executions[q.SessionID] == nil && len(r.executions) >= r.o.HistoryCapacity) {
		r.mu.Unlock()
		return nil, ErrPending
	}
	x := &execution{busy: true, done: make(chan struct{})}
	r.executions[q.SessionID] = x
	r.mu.Unlock()
	running := false
	defer func() {
		r.mu.Lock()
		x.busy = false
		if !running && x.reserved {
			x.stopping = true
		}
		if !x.reserved && !x.reservePending && r.executions[q.SessionID] == x {
			x.retired = true
			if x.done != nil {
				close(x.done)
			}
			delete(r.executions, q.SessionID)
		}
		r.mu.Unlock()
	}()

	// The durable session index is checked even after coordinator reboot. An old
	// generation may not be replaced without exact retirement proof.
	old, e := r.o.Placement.Lookup(ctx, q.SessionID)
	if e == nil && !old.Retired {
		return nil, ErrPending
	}
	if e != nil && !errors.Is(e, sql.ErrNoRows) && !errors.Is(e, placement.ErrFenced) {
		return nil, e
	}
	expected := int64(0)
	if e == nil {
		expected = old.Generation
	}
	target, claims, capabilities, e := r.selectTarget(ctx, q)
	if e != nil {
		return nil, e
	}
	operation, e := randomToken()
	if e != nil {
		return nil, e
	}
	request := placement.Request{Session: q.SessionID, Operation: operation, ExpectedGeneration: expected, Controller: placement.Controller{ID: r.o.Self.NodeID, Boot: r.o.Self.Boot}, Target: target, Role: config.RoleTranscode, Claims: claims, Lease: 2 * time.Minute}
	r.mu.Lock()
	x.request = request
	x.route = Route{Fence: placement.Fence{Session: q.SessionID, Generation: expected + 1, Controller: request.Controller, Target: target}, Source: prepared.Owner, Viewer: viewer, PlayID: playID, Revision: revision}
	x.reservePending = true
	r.mu.Unlock()
	rec, e := r.o.Placement.Reserve(ctx, request)
	if e != nil {
		var unknown *placement.UnknownOutcome
		if !errors.As(e, &unknown) {
			r.mu.Lock()
			x.reservePending = false
			r.mu.Unlock()
		}
		return nil, e
	}
	rt := Route{Fence: rec.Fence, Source: prepared.Owner, Viewer: viewer, PlayID: playID, Revision: revision}
	r.mu.Lock()
	x.route = rt
	x.reserved = true
	x.reservePending = false
	r.mu.Unlock()
	if e = r.putRoute(ctx, rt); e != nil {
		return nil, e
	}
	bound, e := r.sourceCall(ctx, rt, sourceRequest{Op: "bind"})
	if e != nil {
		return nil, e
	}
	deadline := rec.Deadline.Add(-2 * r.o.ClockSkew).UTC()
	endpoint, tc, check, e := r.peer(ctx, nodewire.Identity{Cluster: r.o.Self.Cluster, NodeID: target.NodeID, Boot: target.Boot}, WorkerPath)
	if e != nil {
		return nil, e
	}
	adapter, e := remotetranscode.NewAdapter(remotetranscode.AdapterOptions{CommitStart: func(c context.Context, id remotetranscode.Identity, hash [32]byte) error {
		return r.persistAdmission(c, x, id, hash)
	}, Capabilities: capabilities, Endpoint: endpoint, Client: &http.Client{Transport: &http.Transport{TLSClientConfig: tc}, Timeout: 0}, RecheckServer: check, Assign: func(context.Context, core.HLSRequest) (remotetranscode.Assignment, error) {
		return remotetranscode.Assignment{Identity: remoteIdentity(rt), Grant: bound.Stable, Expires: deadline}, nil
	}, Decide: r.o.Decide, Hardware: r.o.Hardware, Capacity: 1})
	if e != nil {
		return nil, e
	}
	r.mu.Lock()
	x.adapter = adapter
	blocked := r.closed || x.stopping
	if !blocked {
		x.initial = newInitialAdmission(rt, q)
		// The transport receives a separate mutable copy; callbacks cannot alter
		// the retained original admission even on an uncertain start result.
		q = cloneAdmissionRequest(x.initial.request)
	}
	r.mu.Unlock()
	if blocked {
		return nil, ErrPending
	}
	startCtx, cancelStart := context.WithTimeout(ctx, 30*time.Second)
	defer cancelStart()
	hs, token, e := adapter.StartHLSOwned(startCtx, q)
	committed := r.retainStartCommitment(x, token)
	if e != nil || hs == nil || !committed {
		return nil, ErrPending
	}
	running = true
	r.mu.Lock()
	stopped := x.stopping || r.closed
	r.mu.Unlock()
	if stopped {
		r.Stop(q.SessionID)
		return nil, ErrPending
	}
	if output := r.newBufferedSession(rt, q, x.session); output != nil {
		r.mu.Lock()
		x.output = output
		r.mu.Unlock()
		return output, nil
	}
	return hs, nil
}
func (r *Runtime) selectTarget(ctx context.Context, q core.HLSRequest) (placement.Target, []placement.Claim, remotetranscode.Capabilities, error) {
	rows, e := r.o.DB.QueryContext(ctx, `SELECT node,boot,roles,scopes FROM placement_targets ORDER BY node LIMIT 4096`)
	if e != nil {
		return placement.Target{}, nil, remotetranscode.Capabilities{}, e
	}
	type candidate struct {
		target                placement.Target
		rolesJSON, scopesJSON string
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if e = rows.Scan(&c.target.NodeID, &c.target.Boot, &c.rolesJSON, &c.scopesJSON); e != nil {
			rows.Close()
			return placement.Target{}, nil, remotetranscode.Capabilities{}, e
		}
		candidates = append(candidates, c)
	}
	err := rows.Err()
	rows.Close()
	if err != nil {
		return placement.Target{}, nil, remotetranscode.Capabilities{}, err
	}
	failure := &SelectionError{}
	for _, candidate := range candidates {
		target, rolesJSON, scopesJSON := candidate.target, candidate.rolesJSON, candidate.scopesJSON
		var roles config.NodeRoles
		var scopes map[config.NodeRole][]string
		if json.Unmarshal([]byte(rolesJSON), &roles) != nil || !roles.Has(config.RoleTranscode) || json.Unmarshal([]byte(scopesJSON), &scopes) != nil {
			continue
		}
		names := scopes[config.RoleTranscode]
		if len(names) == 0 {
			continue
		}
		if e = r.o.Eligible(ctx, nodewire.Identity{Cluster: r.o.Self.Cluster, NodeID: target.NodeID, Boot: target.Boot}, config.RoleTranscode); e != nil {
			continue
		}
		failure.Eligible++
		claims := make([]placement.Claim, len(names))
		available := true
		for i, name := range names {
			var remaining int64
			err := r.o.DB.QueryRowContext(ctx, `SELECT p.capacity-COALESCE(SUM(c.units),0) FROM placement_pools p LEFT JOIN placement_claims c ON c.scope=p.scope WHERE p.scope=? AND p.role=? GROUP BY p.scope,p.capacity`, name, string(config.RoleTranscode)).Scan(&remaining)
			if err != nil {
				failure.Unavailable++
				available = false
				break
			}
			if remaining < 1 {
				failure.Exhausted++
				available = false
				break
			}
			claims[i] = placement.Claim{Scope: name, Units: 1}
		}
		// This read is advisory only; Reserve remains the atomic admission authority.
		if !available {
			continue
		}
		caps, e := r.workerCapabilities(ctx, nodewire.Identity{Cluster: r.o.Self.Cluster, NodeID: target.NodeID, Boot: target.Boot}, q)
		if e != nil {
			if errors.Is(e, remotetranscode.ErrUnsupported) {
				failure.Unsupported++
			} else {
				failure.Unavailable++
			}
			continue
		}
		if !supportedRequest(q, caps) {
			failure.Unsupported++
			continue
		}
		return target, claims, caps, nil
	}
	return placement.Target{}, nil, remotetranscode.Capabilities{}, failure
}

func (r *Runtime) Session(id string) (core.HLSSession, bool) {
	r.mu.Lock()
	x := r.executions[id]
	var a *remotetranscode.Adapter
	var output *bufferedSession
	active := x != nil && !x.stopping && !x.retired && !r.closed
	if x != nil {
		a = x.adapter
		output = x.output
	}
	r.mu.Unlock()
	if a == nil {
		return nil, false
	}
	// Published local bytes remain available after an encoder failure, while
	// exact current authority is rechecked at every cache request. This is not
	// evidence of worker retirement or permission to replace it.
	if output != nil {
		if !active {
			return nil, false
		}
		if output.published.Load() {
			return output, true
		}
	}
	session, ok := a.Session(id)
	if ok && output != nil {
		return output, true
	}
	return session, ok
}
func (r *Runtime) EncoderRetirement(id string) (core.EncoderRetirement, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	x := r.executions[id]
	if x == nil || x.retired {
		return nil, false
	}
	return executionRetirement{r: r, id: id, x: x}, true
}

type executionRetirement struct {
	r  *Runtime
	id string
	x  *execution
}

func (t executionRetirement) EncoderDone() <-chan struct{} {
	t.r.mu.Lock()
	defer t.r.mu.Unlock()
	return t.x.done
}
func (t executionRetirement) EncoderErr() error {
	t.r.mu.Lock()
	defer t.r.mu.Unlock()
	if t.x.retired {
		return nil
	}
	return core.ErrEncoderPending
}

func (r *Runtime) Stop(id string) {
	r.mu.Lock()
	x := r.executions[id]
	var s *remotetranscode.Session
	if x != nil {
		x.stopping = true
		s = x.session
	}
	r.mu.Unlock()
	if s != nil {
		_ = s.RetireEncoder()
	}
}

// Tick owns only bounded control work; Cleanup runs separately. A lost renewal
// response is reconciled through worker accepted history, never source replay.
func (r *Runtime) Tick(ctx context.Context) error {
	if !r.control.TryLock() {
		return ErrPending
	}
	defer r.control.Unlock()
	r.mu.Lock()
	list := make([]*execution, 0, len(r.executions))
	for _, x := range r.executions {
		copy := *x
		list = append(list, &copy)
	}
	r.mu.Unlock()
	result := r.tickReopenLives(ctx)
	if err := r.persistPreparationReceipts(ctx); err != nil {
		result = err
	}
	r.mu.Lock()
	remote := r.remotePlayback
	r.mu.Unlock()
	if remote != nil {
		if e := remote.tick(ctx); e != nil {
			result = e
		}
	}
	for _, x := range list {
		if e := ctx.Err(); e != nil {
			return e
		}
		if x.busy || x.retired {
			continue
		}
		if x.reservePending {
			rec, e := r.o.Placement.Reserve(ctx, x.request)
			if e != nil {
				var unknown *placement.UnknownOutcome
				if !errors.As(e, &unknown) {
					observed, lookupErr := r.o.Placement.Lookup(ctx, x.request.Session)
					// A business rejection plus a fresh ledger observation proves this
					// immutable operation never reserved. No execution RPC was sent.
					absent := errors.Is(lookupErr, placement.ErrFenced)
					other := lookupErr == nil && observed.Operation != x.request.Operation && observed.Generation <= x.request.ExpectedGeneration
					if absent || other {
						r.mu.Lock()
						current := r.executions[x.request.Session]
						if current != nil && current.request.Operation == x.request.Operation && !current.reserved {
							current.retired = true
							if current.done != nil {
								close(current.done)
							}
							delete(r.executions, x.request.Session)
						}
						r.mu.Unlock()
					}
				}
				result = e
				continue
			}
			x.route.Fence = rec.Fence
			x.reserved = true
			x.reservePending = false
			x.stopping = true
			r.mu.Lock()
			current := r.executions[x.route.Fence.Session]
			if current != nil && current.request.Operation == x.request.Operation {
				current.route = x.route
				current.reserved = true
				current.reservePending = false
				current.stopping = true
			}
			r.mu.Unlock()
		}
		if !x.reserved {
			continue
		}
		if e := r.putRoute(ctx, x.route); e != nil {
			result = e
			continue
		}
		if x.stopping {
			if e := r.retire(ctx, x); e != nil {
				result = e
			} else {
				r.mu.Lock()
				current := r.executions[x.route.Fence.Session]
				if current != nil && current.route == x.route {
					if !current.retired {
						current.retired = true
						if current.done != nil {
							close(current.done)
						}
					}
				}
				r.mu.Unlock()
				if x.adapter != nil {
					x.adapter.Close()
				}
			}
			continue
		}
		if x.session == nil {
			result = ErrPending
			continue
		}
		lease := x.session.Lease()
		if lease.Deadline.After(time.Now().Add(time.Minute)) {
			continue
		}
		deadline := time.Now().Add(2 * time.Minute).UTC().Truncate(time.Millisecond)
		if e := r.o.Placement.Renew(ctx, x.route.Fence, deadline.Add(2*r.o.ClockSkew)); e != nil {
			r.Stop(x.route.Fence.Session)
			result = e
			continue
		}
		n := remotetranscode.Renewal{Identity: remoteIdentity(x.route), Operation: lease.Version + 1, PreviousVersion: lease.Version, PreviousDeadline: lease.Deadline, Deadline: deadline}
		if _, e := x.session.Renew(ctx, n); e != nil {
			_ = x.session.Reconcile(ctx)
			accepted := x.session.Lease().Accepted
			if accepted == nil || !equalRenewal(*accepted, n) {
				r.Stop(x.route.Fence.Session)
				result = ErrPending
			}
		}
	}
	return result
}
func (r *Runtime) retire(ctx context.Context, x *execution) error {
	if retired, e := r.retainedReceipt(ctx, x.route); e != nil {
		return e
	} else if retired {
		return nil
	}
	if x.foreign {
		return r.retireForeign(ctx, x)
	}
	if x.session != nil {
		_ = x.session.RetireEncoder()
		_ = x.session.Reconcile(ctx)
		if e := x.session.EncoderErr(); e != nil {
			return e
		}
	} else if x.retirement != nil {
		return ErrPending
	}
	source, e := r.sourceCall(ctx, x.route, sourceRequest{Op: "retire"})
	if e != nil || !source.Retired {
		return ErrPending
	}
	return r.retireWithEvidence(ctx, x.route, "clusterplayback/exact-retirement")
}
func (r *Runtime) verifyRetired(ctx context.Context, f placement.Fence) error {
	r.mu.Lock()
	x := r.executions[f.Session]
	if x == nil || x.route.Fence != f || x.busy || !x.stopping {
		r.mu.Unlock()
		return ErrPending
	}
	s := x.session
	foreign := x.foreign
	rt := x.route
	r.mu.Unlock()
	if foreign {
		return r.verifyForeign(ctx, rt)
	}
	if s != nil && s.EncoderErr() != nil {
		return ErrPending
	}
	reply, e := r.sourceCall(ctx, rt, sourceRequest{Op: "status"})
	if e != nil || !reply.Retired {
		return ErrPending
	}
	return nil
}

func (r *Runtime) Supports(q core.HLSRequest) bool {
	return supportedRequest(q, r.o.RemoteCapabilities)
}
func (r *Runtime) Allocated(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.executions[id] != nil
}

func equalRenewal(a, b remotetranscode.Renewal) bool {
	return a.Identity == b.Identity && a.Operation == b.Operation && a.PreviousVersion == b.PreviousVersion && a.PreviousDeadline.Equal(b.PreviousDeadline) && a.Deadline.Equal(b.Deadline)
}
