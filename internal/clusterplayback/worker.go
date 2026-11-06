package clusterplayback

import (
	"context"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/nodeinput"
	"github.com/braedonsaunders/ether/internal/nodesource"
	"github.com/braedonsaunders/ether/internal/remotetranscode"
	"net/url"
	"time"
)

type workerInput struct {
	route   Route
	input   *nodeinput.Input
	gateway *nodeinput.Gateway
	client  *nodesource.Client
	cancel  context.CancelFunc
	done    chan struct{}
}

func remoteIdentity(rt Route) remotetranscode.Identity {
	return remotetranscode.Identity{Controller: rt.Fence.Controller.Boot, Worker: rt.Fence.Target.Boot, Session: rt.Fence.Session, Generation: uint64(rt.Fence.Generation)}
}
func (r *Runtime) authorizeWorker(ctx context.Context, peer, op string, id remotetranscode.Identity, start *remotetranscode.Start) error {
	rt, e := r.route(ctx, id.Session)
	if e != nil || remoteIdentity(rt) != id || rt.Fence.Target.NodeID != r.o.Self.NodeID || rt.Fence.Target.Boot != r.o.Self.Boot || peer != rt.Fence.Controller.Boot {
		return ErrDenied
	}
	rec, e := r.o.Placement.Lookup(ctx, id.Session)
	if e != nil || rec.Fence != rt.Fence {
		return ErrDenied
	}
	// Exact stop/status remains available after expiry and role removal.
	if op == "stop" || op == "status" {
		return nil
	}
	if op == "start" && start != nil {
		info := core.CloneMediaInfo(start.Info)
		info.Keyframes = append([]core.KeyframePoint(nil), start.Keyframes...)
		// ManifestSessionID is set only by the primitive after request admission;
		// the external SupportsRequest gate intentionally rejects caller markers.
		request := core.HLSRequest{SessionID: id.Session, ReleaseID: start.ReleaseID, Info: info, Decision: start.Decision, Protocol: start.Protocol, StartOffset: start.Offset, SegmentDuration: start.SegmentDuration, SoftSubtitleStream: start.SoftSubtitleStream, RequireIndependentProof: start.IndependentProof, RecoveryStart: core.CloneRecoveryStart(start.RecoveryStart)}
		if !r.supportsBackend(request) {
			return ErrDenied
		}
	}
	if op == "start" && r.o.Eligible(ctx, r.o.Self, config.RoleTranscode) != nil {
		return ErrDenied
	}
	if r.o.MemberRole(ctx, r.o.Self, config.RoleTranscode) != nil {
		return ErrDenied
	}
	if !rec.Authority || rec.Retired || !rec.Deadline.Add(-r.o.ClockSkew).After(time.Now()) {
		return ErrDenied
	}
	return nil
}
func (r *Runtime) resolveInput(ctx context.Context, peer string, s remotetranscode.Start) (remotetranscode.Source, error) {
	if r.authorizeWorker(ctx, peer, "start", s.Identity, &s) != nil {
		return remotetranscode.Source{}, ErrDenied
	}
	rt, e := r.route(ctx, s.Identity.Session)
	if e != nil {
		return remotetranscode.Source{}, e
	}
	// Raw source expiry minus skew must cover the worker deadline.
	response, e := r.sourceCall(ctx, rt, sourceRequest{Op: "resolve", Stable: s.Grant, Expires: s.Expires.Add(r.o.ClockSkew)})
	if e != nil {
		return remotetranscode.Source{}, e
	}
	if response.Stable != s.Grant || response.Grant.Scope.Fence != rt.Fence || response.Grant.Scope.Revision != rt.Revision || response.Grant.Scope.Viewer != viewerString(rt.Viewer) || response.Grant.Expires.Add(-r.o.ClockSkew).Before(s.Expires) {
		return remotetranscode.Source{}, ErrDenied
	}
	ep, tc, check, e := r.peer(ctx, rt.Source, nodesource.Path)
	if e != nil {
		return remotetranscode.Source{}, e
	}
	c, e := nodesource.NewClient(nodesource.ClientOptions{Endpoint: ep, TLS: tc, BindServer: r.o.Guard.BindServer(rt.Source, mustHostname(ep)), RecheckServer: check, Peer: nodesource.Peer{NodeID: r.o.Self.NodeID, Boot: r.o.Self.Boot}, MaxLifetime: 10 * time.Minute, Timeout: 10 * time.Second, Now: time.Now})
	if e != nil {
		return remotetranscode.Source{}, e
	}
	gateway, e := nodeinput.New(nodeinput.Options{Client: c, Capacity: 1, MaxRequests: 8, RequestTimeout: 30 * time.Second})
	if e != nil {
		c.Close()
		return remotetranscode.Source{}, e
	}
	life, cancel := context.WithCancel(context.Background())
	v := &workerInput{route: rt, gateway: gateway, client: c, cancel: cancel, done: make(chan struct{})}
	r.mu.Lock()
	if r.closed || r.inputs[s.Identity] != nil || len(r.inputs) >= r.o.HistoryCapacity {
		r.mu.Unlock()
		cancel()
		_ = gateway.Close()
		c.Close()
		return remotetranscode.Source{}, ErrDenied
	}
	r.inputs[s.Identity] = v
	r.mu.Unlock()
	go func() { defer close(v.done); _ = gateway.Serve(life) }()
	input, e := gateway.Admit(ctx, s, response.Grant, response.Size)
	r.mu.Lock()
	v.input = input
	r.mu.Unlock()
	return remotetranscode.Source{Gateway: input}, e
}
func (r *Runtime) renewInput(ctx context.Context, peer string, s remotetranscode.Start, n remotetranscode.Renewal) (remotetranscode.RenewalReceipt, error) {
	if r.authorizeWorker(ctx, peer, "renew", s.Identity, nil) != nil {
		return remotetranscode.RenewalReceipt{}, ErrDenied
	}
	r.mu.Lock()
	v := r.inputs[s.Identity]
	var input *nodeinput.Input
	if v != nil {
		input = v.input
	}
	r.mu.Unlock()
	if v == nil || input == nil {
		return remotetranscode.RenewalReceipt{}, ErrDenied
	}
	rec, e := r.o.Placement.Lookup(ctx, s.Identity.Session)
	if e != nil || rec.Fence != v.route.Fence || !rec.Authority || rec.Deadline.Add(-r.o.ClockSkew).Before(n.Deadline) {
		return remotetranscode.RenewalReceipt{}, ErrDenied
	}
	evidence, e := input.Renew(ctx, n, func(c context.Context, old nodesource.Grant) (nodesource.Grant, error) {
		reply, e := r.sourceCall(c, v.route, sourceRequest{Op: "renew", Stable: s.Grant, Old: old, Expires: n.Deadline.Add(r.o.ClockSkew)})
		if e != nil {
			return nodesource.Grant{}, e
		}
		if reply.Grant.Expires.Add(-r.o.ClockSkew).Before(n.Deadline) {
			return nodesource.Grant{}, ErrDenied
		}
		return reply.Grant, nil
	})
	if e != nil {
		return remotetranscode.RenewalReceipt{}, e
	}
	return remotetranscode.RenewalReceipt{Renewal: n, PlacementUntil: rec.Deadline.Add(-r.o.ClockSkew), SourceUntil: evidence.SourceUntil.Add(-r.o.ClockSkew)}, nil
}

// URL is created by peer after trusted endpoint validation.
func mustHostname(ep string) string { u, _ := url.Parse(ep); return u.Hostname() }
