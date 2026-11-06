package app

import (
	"context"
	"net/http"

	"github.com/braedonsaunders/ether/internal/clusterplayback"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/episodeorder"
	"github.com/braedonsaunders/ether/internal/nodewire"
	"github.com/braedonsaunders/ether/internal/playback"
	"github.com/braedonsaunders/ether/internal/resolver"
	"github.com/braedonsaunders/ether/internal/safety"
)

// buildServerRole constructs the real browse/control graph and remote playback
// broker. Provider transports, caches and local encoders are absent. This graph
// remains guarded until protocol and deployment acceptance proves its routes.
func (a *App) buildServerRole(ctx context.Context, cfg config.Config) error {
	var err error
	a.safety, err = safety.New(ctx, a.db)
	if err != nil {
		return err
	}
	if err = a.buildCatalogue(ctx, cfg); err != nil {
		return err
	}
	a.safety.SetCatalogPolicy(a.catalog.VisibleForViewer, a.catalog.DiscoveryVisibleForViewer)
	if err = a.buildIdentity(ctx, cfg); err != nil {
		return err
	}
	if err = a.buildSessionEvents(ctx, cfg); err != nil {
		return err
	}
	if err = a.buildPersonalization(ctx, cfg); err != nil {
		return err
	}
	a.buildPlaybackPresentation()
	if err = a.buildPrivateCredentials(); err != nil {
		return err
	}
	if err = a.buildClusterPlayback(ctx, cfg); err != nil {
		return err
	}
	runtime := a.deployment.playback.(*clusterplayback.Runtime)
	a.brokerPlayback, err = clusterplayback.NewRemotePlayback(runtime, clusterplayback.RemotePlaybackOptions{IdleTimeout: specialistPlaybackIdleTimeout, Capacity: 4096, SelectSource: a.selectPrivateSource, Estimate: func(_ context.Context, t *core.Title, caps core.ClientCaps) core.MediaInfo {
		return playback.EstimateInfo(t, caps, resolver.SelectProfile(a.cfg.Read().Quality, caps, nil))
	}})
	if err != nil {
		return err
	}
	a.brokerTranscoder, err = runtime.DecisionTranscoder()
	if err != nil {
		return err
	}
	a.buildDiscovery()
	a.episodeorder = episodeorder.New(a.db, a.tmdb, a.catalog)
	a.buildExtras()
	a.frontendClients = newFrontendClients(a.db, a.log.With("component", "frontend-clients"), nil)
	a.closers = append(a.closers, a.frontendClients.Close)
	a.frontends, err = newFrontendRuntime(a, cfg)
	if err != nil {
		return err
	}
	a.closers = append(a.closers, a.frontends.close)
	mux := http.NewServeMux()
	mux.Handle("/ether/", a.perf.wrap("console", lanOnly(a.buildAdmin(), a.cfg.TrustedProxyCIDRs, a.cfg.AllowRemoteConsole)))
	mux.Handle("/", a.frontends.plexHandler())
	a.handler = mux
	// This is a genuine protocol handler; role-specific public media mounting and
	// complete supervisor lifecycle are separate acceptance steps before enablement.
	return nil
}

func (a *App) selectPrivateSource(ctx context.Context) (nodewire.Identity, error) {
	nodes, err := a.cluster.Nodes(ctx)
	if err != nil {
		return nodewire.Identity{}, err
	}
	// Prefer the explicitly isolated source pool. Selection happens before a
	// preparation operation exists; uncertain preparation never retries elsewhere.
	for _, dedicated := range []bool{true, false} {
		for _, n := range nodes {
			isDedicated := len(n.Caps.Roles) == 1 && n.Caps.Roles[0] == config.RoleSource
			if isDedicated != dedicated {
				continue
			}
			id := nodewire.Identity{Cluster: a.cfg.Get().Server.MachineIdentifier, NodeID: n.ID, Boot: n.Incarnation}
			if a.privateEligible(ctx, id, config.RoleSource) == nil {
				return id, nil
			}
		}
	}
	return nodewire.Identity{}, core.ErrNotAvailable
}
