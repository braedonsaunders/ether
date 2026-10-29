package app

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/compactids"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/frontends/mediabrowser"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/livetv"
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
	broker         *sessions.Broker
	stateUnsub     func()
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
	local, err := identity.NewLocal(a.db)
	if err != nil {
		return nil, err
	}
	login, err := identity.NewLoginLimiter(a.db, 15*time.Minute, 30, 8, nil)
	if err != nil {
		return nil, err
	}
	poll, err := identity.NewPollLimiter(a.db, time.Minute, 120, 60, nil)
	if err != nil {
		return nil, err
	}
	quick, err := identity.NewQuickConnect(a.db, 10*time.Minute, 30*24*time.Hour, nil)
	if err != nil {
		return nil, err
	}
	resolver, err := identity.NewResolver(a.users, grants)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &frontendRuntime{app: a, ctx: ctx, cancel: cancel, events: a.sessionEvents, discovery: a.gdm, newDiscovery: func() plexDiscovery { return a.newGDMResponder() }}
	r.broker, err = a.newSessionBroker(r.events)
	if err != nil {
		cancel()
		return nil, err
	}
	r.stateUnsub = subscribeSessionState(a.state, r.events, a.plexServer.WatchStateChanged, func(err error) { a.log.Warn("user data event publication failed", "err", err) })
	compact, err := compactids.New(a.db)
	if err != nil {
		r.stateUnsub()
		r.broker.Close(context.Background())
		cancel()
		return nil, err
	}
	var artwork frontend.Artwork
	var channelArtwork func(*core.Viewer, livetv.Channel) frontend.Image
	if a.tmdb != nil {
		variants := newArtworkVariantsQueue(ctx, a.tmdb.CachedArtworkVariants, a.tmdb.ArtworkVariants)
		source := plex.FrontendArtwork{Server: a.plexServer, Variants: variants.get}
		if a.images != nil {
			source.URLImages = a.images
		}
		artwork = source
		channelArtwork = source.ChannelImage
	}
	experiences := make(map[frontend.Protocol]frontend.Experience)
	for _, protocol := range []frontend.Protocol{"plex", "jellyfin", "emby"} {
		experience, err := a.newSurfaceExperience(protocol, artwork)
		if err != nil {
			r.stateUnsub()
			r.broker.Close(context.Background())
			cancel()
			return nil, err
		}
		experiences[protocol] = experience
	}
	r.registry = frontend.NewRegistry(func(id frontend.Protocol) frontend.Host {
		settings := func() frontend.Settings {
			value, err := r.control.Settings(r.ctx, id)
			if err != nil {
				return frontend.Settings{}
			}
			return value
		}
		experience := experiences[id]
		if experience == nil {
			var err error
			experience, err = a.newSurfaceExperience(id, artwork)
			if err != nil {
				a.log.Error("frontend experience unavailable", "surface", id, "err", err)
			}
		}
		var imageGrants frontend.ImageGrants
		if id != "plex" {
			if artwork != nil {
				imageGrants, _ = identity.NewArtworkImageGrantService(a.db, id, grants, func() frontend.Experience { return experience }, artwork, 10*time.Minute, nil)
			} else {
				imageGrants, _ = identity.NewImageGrantService(a.db, id, grants, func() frontend.Experience { return experience }, a.images, 10*time.Minute, nil)
			}
		}
		return frontend.Host{IDs: ids.Codecs{Allocator: compact}, Experience: experience, ViewerCapabilities: a.viewerCapabilities(local, experience), ImageGrants: imageGrants, Preferences: identity.NewPreferences(a.db, id), LiveTV: surfaceLiveTV{app: a, channelArtwork: channelArtwork, artwork: artwork}, Sessions: sessions.BindSurface(r.broker, id), Capabilities: surfaceCapabilities{app: a, surface: id}, Identity: surfaceIdentity{settings: settings, surface: id, resolver: resolver, local: local, grants: grants, quick: quick, login: login, poll: poll}, Events: sessions.BindEvents(r.events, id), Images: frontendImages{source: a.images}, Log: a.log.With("frontend", id), ClientSeen: func(deviceID string) { a.frontendClients.observe(id, deviceID) }, ClientIP: a.frontendClientIP, ViewerPortalURL: r.viewerPortalURL, Config: settings}
	})
	r.control = newFrontendControl(r.registry, a.cfg)
	r.control.clientCount = a.frontendClients.count
	r.control.sharedStatus = r.sharedStatus
	if err := r.registry.Register(plex.Frontend{Server: a.plexServer, StatusFunc: r.plexStatus, ListenersFunc: r.plexListeners}); err != nil {
		r.stateUnsub()
		r.broker.Close(context.Background())
		cancel()
		return nil, err
	}
	if err := r.registry.Register(mediabrowser.Jellyfin{MachineID: cfg.Server.MachineIdentifier}); err != nil {
		r.stateUnsub()
		r.broker.Close(context.Background())
		cancel()
		return nil, err
	}
	if err := r.registry.Register(mediabrowser.Emby{MachineID: cfg.Server.MachineIdentifier}); err != nil {
		r.events.Close()
		cancel()
		return nil, err
	}
	if cfg.Frontends == nil || cfg.Frontends["plex"].Enabled {
		if err := r.control.listeners.start(ctx, "plex"); err != nil {
			r.stateUnsub()
			r.broker.Close(context.Background())
			cancel()
			return nil, err
		}
	}
	return r, nil
}

func (a *App) frontendClientIP(req *http.Request) (netip.Addr, bool) {
	ip, _ := identity.ClientNet(req, identity.ProxyPrefixes(a.cfg.Get().Server.TrustedProxyCIDRs)...)
	return ip, ip.IsValid()
}
func (r *frontendRuntime) plexHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if handler := r.control.listeners.sharedHandler(req.URL.Path); handler != nil {
			handler.ServeHTTP(w, req)
			return
		}
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

func (r *frontendRuntime) sharedStatus(id frontend.Protocol, status *frontend.Status) {
	if id == "plex" {
		return
	}
	instance, ok := r.registry.Instance(id)
	if !ok {
		return
	}
	r.mu.Lock()
	addr := r.mainAddr
	r.mu.Unlock()
	if addr == nil {
		return
	}
	for _, spec := range instance.Listeners() {
		if !spec.Shared {
			continue
		}
		status.Addresses = append(status.Addresses, addr.String()+spec.PathPrefix)
		addresses, _ := net.InterfaceAddrs()
		status.ConnectURLs = append(status.ConnectURLs, connectionURLs(addr.String(), spec.PathPrefix, addresses)...)
	}
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
	if r.stateUnsub != nil {
		r.stateUnsub()
	}
	if r.broker != nil {
		r.broker.Close(context.Background())
	}
}
