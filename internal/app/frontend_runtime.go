package app

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/plex"
	"github.com/braedonsaunders/ether/internal/plextv"
	"github.com/braedonsaunders/ether/internal/sessions"
)

type plexDiscovery interface {
	Start(context.Context) error
	Stop()
}

// frontendRuntime keeps the owner listener independent from protocol mounts.
type frontendRuntime struct {
	app            *App
	registry       *frontend.Registry
	control        *frontendControl
	events         *sessions.EventBus
	ctx            context.Context
	cancel         context.CancelFunc
	mu             sync.Mutex
	lifecycleMu    sync.Mutex
	discoveryError string
	runCtx         context.Context
	mainAddr       net.Addr
	gdmRunning     bool
	discovery      plexDiscovery
	newDiscovery   func() plexDiscovery
	closed         bool
}

func newFrontendRuntime(a *App, cfg config.Config) (*frontendRuntime, error) {
	grants, err := identity.NewGrants(a.db, a.users, 30*24*time.Hour, nil)
	if err != nil {
		return nil, err
	}
	resolver, err := identity.NewResolver(a.users, grants)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &frontendRuntime{app: a, ctx: ctx, cancel: cancel, events: sessions.NewEventBus(a.clusterNode(), a.users.ActiveUser), discovery: a.gdm, newDiscovery: func() plexDiscovery { return a.newGDMResponder() }}
	r.registry = frontend.NewRegistry(func(id frontend.Protocol) frontend.Host {
		return frontend.Host{Identity: frontend.BindIdentity(resolver, id), Events: r.events, Log: a.log.With("frontend", id), ClientSeen: func(deviceID string) { a.frontendClients.observe(id, deviceID) }, Config: func() frontend.Settings {
			settings, err := r.control.Settings(r.ctx, id)
			if err != nil {
				return frontend.Settings{}
			}
			return settings
		}}
	})
	r.control = newFrontendControl(r.registry, a.cfg)
	r.control.clientCount = a.frontendClients.count
	if err := r.registry.Register(plex.Frontend{Server: a.plexServer, StatusFunc: r.plexStatus, ListenersFunc: r.plexListeners}); err != nil {
		r.events.Close()
		cancel()
		return nil, err
	}
	if cfg.Frontends == nil || cfg.Frontends["plex"].Enabled {
		if err := r.control.listeners.start(ctx, "plex"); err != nil {
			r.events.Close()
			cancel()
			return nil, err
		}
	}
	return r, nil
}
func (r *frontendRuntime) plexHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		inst, ok := r.registry.Instance("plex")
		if !ok {
			http.NotFound(w, req)
			return
		}
		inst.Handler().ServeHTTP(w, req)
	})
}
func (r *frontendRuntime) plexListeners() []frontend.ListenerSpec {
	r.mu.Lock()
	addr := r.mainAddr
	r.mu.Unlock()
	address := ""
	if addr != nil {
		address = addr.String()
	}
	return []frontend.ListenerSpec{{Address: address, PathPrefix: "/", Shared: true}}
}
func (r *frontendRuntime) plexStatus(context.Context) frontend.Status {
	r.mu.Lock()
	addr := r.mainAddr
	discoveryError := r.discoveryError
	r.mu.Unlock()
	cfg := r.app.cfg.Get()
	st := frontend.Status{TargetVersion: plextv.PMSVersion(cfg), LAN: "unknown", WAN: "unknown", Conformance: "unknown", LiveStreamsKnown: true}
	if discoveryError != "" {
		st.Warnings = append(st.Warnings, discoveryError)
	}
	if addr != nil {
		st.Addresses = []string{addr.String()}
		addresses, _ := net.InterfaceAddrs()
		st.ConnectURLs = connectionURLs(addr.String(), "", addresses)
	}
	if r.app.registrar != nil {
		if reachable := r.app.registrar.Status().PlexReachable; reachable != nil {
			if *reachable {
				st.WAN = "reachable"
			} else {
				st.WAN = "unreachable"
			}
		}
	}
	if r.app.playback != nil {
		for _, session := range (ownerPlayback{Playback: r.app.playback, app: r.app}).Sessions() {
			if session.Client.Protocol == "" || session.Client.Protocol == "plex" {
				st.LiveStreams++
			}
		}
	}
	return st
}
func (r *frontendRuntime) reconcile(cfg config.Config) {
	r.lifecycleMu.Lock()
	defer r.lifecycleMu.Unlock()
	r.mu.Lock()
	closed, running := r.closed, r.runCtx != nil
	r.mu.Unlock()
	if closed {
		return
	}
	if running {
		r.control.Reconcile(r.ctx, cfg)
	} else {
		desired := cfg.Frontends == nil || cfg.Frontends["plex"].Enabled
		_, mounted := r.registry.Instance("plex")
		if desired && !mounted {
			_ = r.control.listeners.start(r.ctx, "plex")
		}
		if !desired && mounted {
			_ = r.control.listeners.stop(r.ctx, "plex")
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.syncDiscoveryLocked()
}
func (r *frontendRuntime) start(ctx context.Context, addr net.Addr) {
	r.lifecycleMu.Lock()
	defer r.lifecycleMu.Unlock()
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.runCtx = ctx
	r.mainAddr = addr
	r.mu.Unlock()
	r.control.Reconcile(r.ctx, r.app.cfg.Get())
	r.mu.Lock()
	defer r.mu.Unlock()
	r.syncDiscoveryLocked()
}
func (r *frontendRuntime) syncDiscoveryLocked() {
	if r.closed || r.runCtx == nil {
		return
	}
	_, enabled := r.registry.Instance("plex")
	if enabled && !r.gdmRunning {
		if r.discovery == nil {
			r.discovery = r.newDiscovery()
		}
		if err := r.discovery.Start(r.runCtx); err != nil {
			r.app.log.Warn("GDM discovery responder", slog.Any("err", err))
			r.discoveryError = err.Error()
			return
		}
		r.gdmRunning = true
		r.discoveryError = ""
	} else if !enabled && r.gdmRunning {
		r.discovery.Stop()
		r.discovery = nil
		r.gdmRunning = false
	}
}
func (r *frontendRuntime) stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runCtx = nil
	r.mainAddr = nil
	if r.gdmRunning {
		r.discovery.Stop()
		r.discovery = nil
		r.gdmRunning = false
	}
}
func (r *frontendRuntime) close() {
	r.lifecycleMu.Lock()
	defer r.lifecycleMu.Unlock()
	r.stop()
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	r.cancel()
	_ = r.control.listeners.close(context.Background())
	r.events.Close()
}
