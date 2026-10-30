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
	"github.com/braedonsaunders/ether/internal/frontends/dlna"
	"github.com/braedonsaunders/ether/internal/frontends/m3u"
	"github.com/braedonsaunders/ether/internal/frontends/mediabrowser"
	"github.com/braedonsaunders/ether/internal/frontends/silo"
	"github.com/braedonsaunders/ether/internal/frontends/stremio"
	"github.com/braedonsaunders/ether/internal/frontends/webdav"
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
	artworkWait    func()
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
	times := grants.ViewerTimes()
	resolver, err := identity.NewResolver(a.users, grants)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &frontendRuntime{app: a, ctx: ctx, cancel: cancel, events: a.sessionEvents, discovery: a.gdm, newDiscovery: func() plexDiscovery { return a.newGDMResponder() }}
	initialized := false
	defer func() {
		if !initialized {
			r.close()
		}
	}()

	r.broker, err = a.newSessionBroker(r.events)
	if err != nil {
		return nil, err
	}
	r.stateUnsub = subscribeSessionState(a.state, r.events, func(err error) { a.log.Warn("user data event publication failed", "err", err) })
	compact, err := compactids.New(a.db)
	if err != nil {
		return nil, err
	}
	var artwork frontend.Artwork
	var channelArtwork func(*core.Viewer, livetv.Channel) frontend.Image
	if a.tmdb != nil {
		source, wait := a.newSharedArtwork(ctx)
		a.artwork = source
		r.artworkWait = wait
		artwork = source
		channelArtwork = source.ChannelImage
	}

	experiences := make(map[frontend.Protocol]frontend.Experience)
	for _, protocol := range []frontend.Protocol{"plex", "jellyfin", "emby", "silo"} {
		experience, err := a.newSurfaceExperience(protocol, artwork)
		if err != nil {
			return nil, err
		}
		if scoped, ok := experience.(*surfaceExperience); ok {
			scoped.playability = func(ctx context.Context, req frontend.ItemRequest) *bool {
				return detailCanPlay(ctx, req, protocol, surfaceCapabilities{app: a, surface: protocol}, r.broker)
			}
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
			if scoped, ok := experience.(*surfaceExperience); ok {
				scoped.playability = func(ctx context.Context, req frontend.ItemRequest) *bool {
					return detailCanPlay(ctx, req, id, surfaceCapabilities{app: a, surface: id}, r.broker)
				}
			}
		}
		tickets, _ := identity.NewTicketService(a.db, id, grants, nil)
		var lanViewer func(context.Context, *http.Request) (*core.Viewer, error)
		if id == "dlna" {
			lanViewer = func(ctx context.Context, req *http.Request) (*core.Viewer, error) {
				return a.dlnaViewer(ctx, req, settings())
			}
		}
		var lanStreams frontend.LANLazyStreams
		if lanViewer != nil {
			var err error
			lanStreams, err = sessions.NewLANLazyStreams(a.db, r.broker, a.frontendClientIP, lanViewer, time.Hour)
			if err != nil {
				a.log.Error("LAN lazy streams unavailable", "err", err)
			}
		}
		lazy, lazyErr := sessions.NewLazyStreams(a.db, r.broker, id, grants, time.Hour, func(req *http.Request) bool { ip, ok := a.frontendClientIP(req); return !ok || !identity.LAN(ip) })
		versionHandles, handleErr := sessions.NewVersionHandles(a.db, r.broker, id)
		if handleErr != nil {
			a.log.Error("version handles unavailable", "surface", id, "err", handleErr)
		}
		if lazyErr != nil {
			a.log.Error("lazy streams unavailable", "surface", id, "err", lazyErr)
		}
		var imageGrants frontend.ImageGrants
		if id != "plex" {
			if artwork != nil {
				imageGrants, _ = identity.NewArtworkImageGrantService(a.db, id, grants, func() frontend.Experience { return experience }, artwork, 10*time.Minute, nil)
			} else {
				imageGrants, _ = identity.NewImageGrantService(a.db, id, grants, func() frontend.Experience { return experience }, a.images, 10*time.Minute, nil)
			}
		}
		return frontend.Host{IDs: ids.Codecs{Allocator: compact}, Experience: experience, LastPlayedSource: surfaceLastPlayedSource{experience: experience, state: a.state}, VersionHandles: versionHandles, VirtualPaths: surfaceVirtualPaths{experience: experience, enabled: func() bool { _, ok := r.registry.Instance("webdav"); return ok }, base: func(ctx context.Context, req *http.Request) (string, error) { return r.surfaceURL(ctx, "webdav", req) }}, ViewerCapabilities: a.viewerCapabilities(local, experience), ViewerPolicy: surfaceViewerPolicy{app: a}, ImageGrants: imageGrants, Preferences: identity.NewPreferences(a.db, id), TrackPreferences: surfaceTrackPreferences{app: a}, LiveTV: surfaceLiveTV{app: a, channelArtwork: channelArtwork, artwork: artwork}, Sessions: scorecardSessions(a, sessions.BindSurface(r.broker, id)), LazyStreams: lazy, LANStreams: lanStreams, Capabilities: surfaceCapabilities{app: a, surface: id}, Identity: surfaceIdentity{settings: settings, surface: id, resolver: resolver, local: local, grants: grants, quick: quick, login: login, poll: poll, times: times}, Events: sessions.BindEvents(r.events, id), Tickets: tickets, Images: frontendImages{source: a.images}, Log: a.log.With("frontend", id), ClientSeen: func(deviceID string) { a.frontendClients.observe(id, deviceID) }, ClientIP: a.frontendClientIP, ViewerPortalURL: r.viewerPortalURL, SecurePortalURL: r.viewerPortalSecureURL, SurfaceURL: func(ctx context.Context, req *http.Request) (string, error) {
			return scorecardSurfaceURL(ctx, r, id, req)
		}, LANViewer: lanViewer, Config: settings}
	})
	r.control = newFrontendControl(r.registry, a.cfg)
	r.control.listeners.discovery = newSharedDiscovery(r.ctx)
	r.control.listeners.discoveryEndpoint = r.discoveryEndpoint
	r.control.clientCount = a.frontendClients.count
	r.control.streamCounts = func(context.Context) (map[frontend.Protocol]int, bool) {
		if a.playback == nil {
			return nil, false
		}
		return countFrontendStreams((ownerPlayback{Playback: a.playback, app: a}).Sessions()), true
	}
	r.control.sharedStatus = r.sharedStatus
	if err := r.registry.Register(plex.Frontend{NewServer: func() (*plex.Server, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		current := a.cfg.Get()
		// Domain construction fixes the spike library for this runtime. First mount
		// uses current identity/settings without changing the underlying library.
		current.Spike = cfg.Spike
		handler, err := a.buildPlex(ctx, current)
		if err != nil {
			return nil, err
		}
		return handler.(*plex.Server), nil
	}, WatchState: a.state, ExplicitWatchOnly: cfg.Spike.Enabled, StatusFunc: r.plexStatus, ListenersFunc: r.plexListeners}); err != nil {
		return nil, err
	}
	if err := r.registry.Register(mediabrowser.Jellyfin{MachineID: cfg.Server.MachineIdentifier}); err != nil {
		return nil, err
	}
	if err := r.registry.Register(mediabrowser.Emby{MachineID: cfg.Server.MachineIdentifier}); err != nil {
		return nil, err
	}
	if err := r.registry.Register(silo.Frontend{MachineID: cfg.Server.MachineIdentifier}); err != nil {
		return nil, err
	}
	if err := r.registry.Register(stremio.Frontend{}); err != nil {
		return nil, err
	}
	if err := r.registry.Register(webdav.Frontend{}); err != nil {
		return nil, err
	}
	if err := r.registry.Register(dlna.Frontend{MachineID: cfg.Server.MachineIdentifier}); err != nil {
		return nil, err
	}
	if err := r.registry.Register(m3u.Frontend{}); err != nil {
		return nil, err
	}
	if cfg.Frontends == nil || cfg.Frontends["plex"].Enabled {
		if err := r.control.listeners.start(ctx, "plex"); err != nil {
			return nil, err
		}
	}
	initialized = true
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
func (r *frontendRuntime) plexStatus(ctx context.Context) frontend.Status {
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
	if snapshot, ok := ctx.Value(frontendStreamCountsKey{}).(frontendStreamCounts); ok {
		st.LiveStreams, st.LiveStreamsKnown = snapshot.counts["plex"], snapshot.known
	} else if r.app.playback != nil {
		st.LiveStreams = countFrontendStreams((ownerPlayback{Playback: r.app.playback, app: r.app}).Sessions())["plex"]
	}
	return st
}

func countFrontendStreams(sessions []*core.PlaySession) map[frontend.Protocol]int {
	counts := make(map[frontend.Protocol]int)
	for _, session := range sessions {
		if session == nil {
			continue
		}
		protocol := session.Client.Protocol
		if protocol == "" {
			protocol = "plex"
		}
		counts[frontend.Protocol(protocol)]++
	}
	return counts
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
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	r.mu.Unlock()
	r.stop()
	if r.cancel != nil {
		r.cancel()
	}
	if r.control != nil && r.control.listeners != nil {
		_ = r.control.listeners.close(context.Background())
	}
	if r.stateUnsub != nil {
		r.stateUnsub()
	}
	if r.broker != nil {
		r.broker.Close(context.Background())
	}
	if r.artworkWait != nil {
		r.artworkWait()
	}
	if r.app != nil {
		r.app.plexBuildMu.Lock()
		closePlex := r.app.plexClose
		r.app.plexClose = nil
		r.app.plexBuildMu.Unlock()
		if closePlex != nil {
			closePlex()
		}
	}
}
