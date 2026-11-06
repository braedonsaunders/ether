package app

import (
	"context"
	"errors"
	"time"

	"github.com/braedonsaunders/ether/internal/clusterplayback"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/nodewire"
	"github.com/braedonsaunders/ether/internal/placement"
	"github.com/braedonsaunders/ether/internal/playback"
	"github.com/braedonsaunders/ether/internal/remotetranscode"
)

func (a *App) buildClusterPlayback(ctx context.Context, _ config.Config) error {
	p := a.deployment
	if p == nil {
		return nil
	}
	verifier := clusterplayback.NewVerifier()
	ledger, err := placement.New(a.db, placement.Options{MaxSessions: p.settings.LedgerCapacity, MaxLease: 10 * time.Minute, LockTimeout: 3 * time.Second}, verifier)
	if err != nil {
		return err
	}
	if err := ledger.EnsureLedgerQuota(ctx); err != nil {
		return err
	}
	self := a.cluster.Self()
	o := clusterplayback.Options{DB: a.db, Guard: p.guard, Self: nodewire.Identity{Cluster: a.cfg.Get().Server.MachineIdentifier, NodeID: self.ID, Boot: self.Incarnation}, Roles: p.settings.Roles, Placement: ledger, Endpoint: a.privateEndpoint, MemberRole: a.privateMemberRole, Eligible: a.privateEligible, Capacity: p.settings.Capacity, ClockSkew: time.Duration(p.settings.ClockSkewMillis) * time.Millisecond, ClockBoundKnown: p.settings.ClockBoundKnown}
	// The reviewed complete source/capability/start/observation chain preserves
	// bounded indexes without truncation. The same dedicated manager below owns
	// both real worker preflight and execution; all other extensions stay closed.
	capabilities := remotetranscode.Supported()
	capabilities.Keyframes = true
	o.RemoteCapabilities, o.WorkerCapabilities = capabilities, capabilities
	if p.settings.Roles.FullServer() {
		o.Roles = config.NodeRoles{config.RoleServer, config.RoleSource, config.RoleDelivery, config.RoleBackground}
		if local := a.activeTranscoder(); local != nil {
			o.Decide = local.Decide
			o.Hardware = local.HWAccel()
		} else {
			o.Decide = directOnly{}.Decide
		}
	}
	if p.settings.Roles.Has(config.RoleSource) {
		if a.playback == nil || a.users == nil {
			return errors.New("source role requires playback and viewer authority")
		}
		o.SourcePolicy = a.privateSourcePolicy
		o.Playback = a.playback
		o.ActiveViewer = a.users.ActiveUser
		o.SourceAccess, err = playback.NewSourceAccess(a.playback, 4096, 4096)
		if err != nil {
			return err
		}
	}
	if p.settings.Roles.Has(config.RoleTranscode) && !p.settings.Roles.FullServer() {
		if err := a.buildPrivateEncoder(a.cfg.Get()); err != nil {
			return err
		}
		if tc := p.encoder; tc != nil {
			o.Encoder = tc
			o.Decide = tc.Decide
			o.Hardware = tc.HWAccel()
		}
		if o.Encoder == nil {
			return errors.New("transcode role requires owned encoder lifecycle")
		}
	}
	if o.Decide == nil {
		o.Decide = directOnly{}.Decide
	}
	runtime, err := clusterplayback.NewRuntime(o)
	if err != nil {
		return err
	}
	if err := verifier.Bind(runtime); err != nil {
		return err
	}
	p.playback = runtime
	p.placement = ledger
	a.closers = append(a.closers, func() { _ = runtime.Close() })
	return a.buildSourceAdmin()
}

func (a *App) privateMemberRole(ctx context.Context, id nodewire.Identity, role config.NodeRole) error {
	n, err := a.privateMember(ctx, id)
	if err != nil {
		return err
	}
	if !n.Caps.Roles.Has(role) || role == config.RoleTranscode && (!n.Caps.Transcode || n.Caps.Roles.FullServer()) || !n.RemoteCompatible(1) {
		return nodewire.ErrDenied
	}
	return nil
}
func (a *App) privateEligible(ctx context.Context, id nodewire.Identity, role config.NodeRole) error {
	n, err := a.privateMember(ctx, id)
	if err != nil {
		return err
	}
	if n.Draining || id.NodeID == a.cluster.Self().ID && a.nodeDraining() || !n.Caps.Roles.Has(role) || role == config.RoleTranscode && (!n.Caps.Transcode || n.Caps.Roles.FullServer()) || !n.RemoteCompatible(1) {
		return nodewire.ErrDenied
	}
	return nil
}

// reconcilePrivatePlacement changes only this process's authenticated target.
// Scope survives boots, so unresolved old encoders continue consuming capacity.
func (a *App) reconcilePrivatePlacement(ctx context.Context, observation *privateReadinessSnapshot) error {
	p := a.deployment
	self := a.cluster.Self()
	id := nodewire.Identity{Cluster: a.cfg.Get().Server.MachineIdentifier, NodeID: self.ID, Boot: self.Incarnation}
	if _, err := a.privateMemberObserved(ctx, id, observation); err != nil {
		return err
	}
	var roles config.NodeRoles
	scopes := map[config.NodeRole][]string{}
	if p.settings.Roles.Has(config.RoleTranscode) && !p.settings.Roles.FullServer() && !a.nodeDraining() && p.playback.Ready() && p.quotaOpen.Load() {
		scope := p.settings.ResourceScope
		if scope == "" {
			scope = "node:" + self.ID
		}
		if err := p.placement.SetPool(ctx, scope, config.RoleTranscode, int64(p.settings.Capacity)); err != nil {
			observation.reason, observation.at = readinessPool, time.Now()
			observation.errorClass = privateReadinessError(err)
			observation.deadline, _ = ctx.Deadline()
			return err
		}
		roles = config.NodeRoles{config.RoleTranscode}
		scopes[config.RoleTranscode] = []string{scope}
	}
	err := p.placement.SetTarget(ctx, placement.Target{NodeID: self.ID, Boot: self.Incarnation}, roles, scopes)
	observation.reason, observation.at = readinessTarget, time.Now()
	observation.errorClass = privateReadinessError(err)
	observation.deadline, _ = ctx.Deadline()
	return err
}

// privateSourcePolicy uses current durable viewer policy and cached item/parent
// facts. It never resolves providers or refreshes metadata on a byte request.
func (a *App) privateSourcePolicy(ctx context.Context, v *core.Viewer, s *core.PlaySession) error {
	if v == nil || s == nil || v.ID != s.UserID || a.safety == nil || a.catalog == nil {
		return core.ErrUnauthorized
	}
	fresh, err := a.safety.ConfirmPlayback(ctx, v, s.Client.Remote, time.Now())
	if err != nil || fresh == nil || fresh.ID != s.UserID {
		return core.ErrUnauthorized
	}
	visibility := experienceVisibility(a.safety, a.catalog.CachedGet, nil, nil, serverLibraries(a.cfg.Read()))
	visibility.SectionFor = brokerSectionFor(visibility.SectionFor)
	title, err := cachedSessionTitle(ctx, fresh, s.Key, a.catalog.CachedGet, visibility)
	if err != nil || title == nil || (!title.Released.IsZero() && title.Released.After(time.Now())) {
		return core.ErrUnauthorized
	}
	return nil
}

func (a *App) refreshPrivateQuota(ctx context.Context) error {
	p := a.deployment
	status, err := p.placement.LedgerStatus(ctx)
	open := err == nil && status.Matches && !status.Exhausted
	previous := p.quotaOpen.Swap(open)
	if err == nil {
		p.quota.Store(&runtimeQuotaSnapshot{SampledAt: time.Now().UTC(), Status: status})
	}
	if previous && !open {
		a.log.Warn("private placement ledger quota blocks new admissions; retained ownership unchanged", "capacity", status.Capacity, "used", status.Used, "retired", status.Retired)
	}
	return err
}
