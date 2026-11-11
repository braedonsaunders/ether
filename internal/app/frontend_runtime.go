package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/diagnostics"
	"github.com/braedonsaunders/ether/internal/egress"
	experiencepkg "github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/compactids"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/frontends/dlna"
	"github.com/braedonsaunders/ether/internal/frontends/m3u"
	"github.com/braedonsaunders/ether/internal/frontends/mediabrowser"
	"github.com/braedonsaunders/ether/internal/frontends/plex"
	"github.com/braedonsaunders/ether/internal/frontends/silo"
	"github.com/braedonsaunders/ether/internal/frontends/stremio"
	"github.com/braedonsaunders/ether/internal/frontends/webdav"
	"github.com/braedonsaunders/ether/internal/frontends/xtream"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/livetv"
	"github.com/braedonsaunders/ether/internal/plextv"
	"github.com/braedonsaunders/ether/internal/sessions"
	"github.com/braedonsaunders/ether/internal/trackprefs"
	"github.com/braedonsaunders/ether/internal/updater"
	"github.com/braedonsaunders/ether/internal/watchparty"
)

type plexDiscovery interface {
	Start(context.Context) error
	Stop()
}

// frontendRuntime keeps the owner listener independent from protocol mounts.
type frontendRuntime struct {
	app            *App
	registry       *frontend.Registry
	plex           plex.Frontend
	control        *frontendControl
	events         *sessions.EventBus
	broker         *sessions.Broker
	stateUnsub     func()
	artworkWait    func()
	ctx            context.Context
	cancel         context.CancelFunc
	mu             sync.Mutex
	lifecycleMu    sync.Mutex
	lan            lanCheck
	lanChecking    bool
	lanGeneration  uint64
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
	parties := watchparty.New(watchparty.Options{
		Allow: func(ctx context.Context, protocol frontend.Protocol, viewer *core.Viewer, item core.RatingKey) error {
			exp := experiences[protocol]
			if exp == nil || viewer == nil {
				return core.ErrNotFound
			}
			_, err := exp.Item(ctx, frontend.ItemRequest{Viewer: viewer, Key: item})
			return err
		},
		Streams: func(ctx context.Context, protocol frontend.Protocol, viewer *core.Viewer, device core.Device, item core.RatingKey) error {
			if r.broker == nil || viewer == nil || viewer.StreamLimit <= 0 {
				return nil
			}
			rows, err := sessions.BindSurface(r.broker, protocol).Sessions(ctx, frontend.SessionAccess{Viewer: viewer, Device: device, Protocol: protocol})
			if err != nil {
				return err
			}
			for _, row := range rows {
				if row.Item == item && row.ViewerID == viewer.ID {
					return nil
				}
			}
			if len(rows) >= viewer.StreamLimit {
				return core.ErrStreamLimit
			}
			return nil
		},
		ItemID: ids.RatingKeyGUID,
		JellyfinUser: func(id int64) string {
			value, err := ids.GUID(ids.User, id)
			if err != nil {
				return ""
			}
			return value
		},
	})
	repoFile := repoFileURL(sourceRef(updater.Version, updater.Commit, buildRevision()))
	metadataHTTP := egress.Client(egress.CapMetadata, 20*time.Second)
	spikeCfg := cfg.Spike
	watchRefresh := experiencepkg.BindWatchRefresh(a.state)
	var homeInvalidation frontend.HomeInvalidationSubscriber
	if a.db != nil {
		homeInvalidation = experiencepkg.BindHomeInvalidation(a.state)
	}
	r.registry = frontend.NewRegistry(func(id frontend.Protocol) frontend.Host {
		if id == "plex" {
			_ = a.ensurePlexAccountServices()
		}
		settings := func() frontend.Settings {
			value, err := r.control.Settings(r.ctx, id)
			if err != nil || value == nil {
				value = frontend.Settings{}
			}
			name := "Ether"
			if a.cfg != nil {
				if trimmed := strings.TrimSpace(a.cfg.Read().Server.FriendlyName); trimmed != "" {
					name = trimmed
				}
			}
			out := make(frontend.Settings, len(value)+1)
			for k, v := range value {
				if k != "serverName" {
					out[k] = v
				}
			}
			out["serverName"] = name
			return out
		}
		var hostConfig func() frontend.HostConfig
		if a.cfg != nil {
			// Domain construction fixes the spike library for this runtime;
			// live config reads reflect current identity and settings without
			// switching the underlying library mid-run.
			hostConfig = func() frontend.HostConfig {
				c := a.cfg.Read()
				c.Spike = spikeCfg
				return c
			}
		}
		currentCfg := cfg
		if hostConfig != nil {
			currentCfg = hostConfig()
		}
		serverIdentity := frontend.ServerIdentity{
			MachineID:    currentCfg.Server.MachineIdentifier,
			FriendlyName: currentCfg.Server.FriendlyName,
			Version:      plextv.DefaultServerVersion,
			Platform:     plexPlatform(),
		}
		ownerSub := func() (bool, []string) {
			if a.registrar == nil {
				return false, nil
			}
			st := a.registrar.Status()
			return st.PlexPass, st.OwnerFeatures
		}
		publicOrigin := func() string {
			if a.registrar == nil {
				return ""
			}
			return publicPlexOrigin(a.registrar.Status().Connections)
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
		versionHandles, handleErr := sessions.NewVersionHandles(a.db, r.broker, id, a.cfg.DerivedKey("ether-version-id-v1"))
		if handleErr != nil {
			a.log.Error("version handles unavailable", "surface", id, "err", handleErr)
		}
		var sourceOffers frontend.SourceOffers
		if id == "silo" {
			var err error
			sourceOffers, err = sessions.NewSourceOffers(a.db, r.broker, id)
			if err != nil {
				a.log.Error("source offers unavailable", "surface", id, "err", err)
			}
		}
		if lazyErr != nil {
			a.log.Error("lazy streams unavailable", "surface", id, "err", lazyErr)
		}
		var imageGrants frontend.ImageGrants
		var imageRevision func() uint64
		{
			var svc *identity.ImageGrantService
			var err error
			if artwork != nil {
				svc, err = identity.NewArtworkImageGrantService(a.db, id, grants, func() frontend.Experience { return experience }, artwork, 10*time.Minute, nil)
			} else {
				svc, err = identity.NewImageGrantService(a.db, id, grants, func() frontend.Experience { return experience }, a.images, 10*time.Minute, nil)
			}
			if err == nil && svc != nil {
				if a.cfg != nil {
					svc.SetImageKey(a.cfg.DerivedKey("ether-image-page-grant-v1"))
				}
				svc.SetImageGeneration(func() uint64 {
					var gen uint64
					if a.catalog != nil {
						gen = a.catalog.WriteGeneration()
					}
					if a.cfg != nil {
						gen ^= a.cfg.Generation()
					}
					if a.libraries != nil {
						gen ^= a.libraries.Generation()
					}
					if a.pluginsRT != nil && a.pluginsRT.pluginitems != nil {
						gen ^= a.pluginsRT.pluginitems.Generation()
					}
					return gen
				})
				imageGrants = svc
				imageRevision = svc.Generation
			}
		}
		var extraPlay frontend.ExtraStreams
		var cinema frontend.CinemaIntros
		if a.extras != nil {
			extraPlay = extraSurface{extras: a.extras}
			cinema = cinemaSurface{exp: experience, extras: a.extras}
		}
		var prerollIntros frontend.Prerolls
		if a.prerolls != nil {
			prerollIntros = prerollSurface{svc: a.prerolls, state: a.state}
		}
		var subtitleCatalog frontend.SubtitleCatalog
		if a.subtitles != nil {
			subtitleCatalog = subtitleSurface{app: a, exp: experience, surface: string(id)}
		}
		var kindStreams frontend.KindStreams
		if a.pluginsRT != nil && a.pluginsRT.kindResolver != nil {
			kindStreams = a.pluginsRT.kindResolver
		}
		return frontend.Host{Support: a.support, IDs: ids.Codecs{Allocator: compact}, DirectProxy: egress.ProxyFunc(egress.CapDirect), MetadataHTTP: metadataHTTP, Experience: experience, KindStreams: kindStreams, LastPlayedSource: surfaceLastPlayedSource{experience: experience, state: a.state}, VersionHandles: versionHandles, SourceOffers: sourceOffers, Editions: surfaceEditions{app: a}, VirtualPaths: surfaceVirtualPaths{experience: experience, index: &pathIndex{}, stamp: func(v *core.Viewer) string {
			if v == nil || a.catalog == nil || a.cfg == nil {
				return ""
			}
			var libGen, kindGen uint64
			if a.libraries != nil {
				libGen = a.libraries.Generation()
			}
			if a.pluginsRT != nil && a.pluginsRT.pluginitems != nil {
				kindGen = a.pluginsRT.pluginitems.Generation()
			}
			return fmt.Sprintf("%d|%d|%d|%d|%d|%s|%v", a.catalog.WriteGeneration(), a.cfg.Generation(), libGen, kindGen, v.ID, v.MaxContentRating, v.AllowedLibraries)
		}, enabled: func() bool { _, ok := r.registry.Instance("webdav"); return ok }, base: func(ctx context.Context, req *http.Request) (string, error) { return r.surfaceURL(ctx, "webdav", req) }}, ViewerCapabilities: a.viewerCapabilities(local, experience), ViewerPolicy: surfaceViewerPolicy{app: a}, ImageGrants: imageGrants, Artwork: artwork, ExtraStreams: extraPlay, Cinema: cinema, Prerolls: prerollIntros, Subtitles: subtitleCatalog, Preferences: identity.NewPreferences(a.db, id), TrackPreferences: surfaceTrackPreferences{app: a}, LiveTV: surfaceLiveTV{app: a, surface: string(id), channelArtwork: channelArtwork, artwork: artwork}, Sessions: scorecardSessions(a, sessions.BindSurface(r.broker, id)), LazyStreams: lazy, LANStreams: lanStreams, Capabilities: surfaceCapabilities{app: a, surface: id}, Identity: surfaceIdentity{settings: settings, surface: id, users: a.users, resolver: resolver, local: local, grants: grants, quick: quick, login: login, poll: poll, times: times}, Events: sessions.BindEvents(r.events, id), WatchRefresh: watchRefresh, HomeInvalidation: homeInvalidation, Tickets: tickets, WatchParty: parties, Images: frontendImages{source: a.images}, Log: a.log.With("frontend", id), ClientSeen: func(deviceID string) { a.frontendClients.observe(id, deviceID) }, RemoteClientSeen: func(address string) {
			if a.registrar != nil {
				a.registrar.ObserveRemoteClient(address)
			}
		}, ClientIP: a.frontendClientIP, ViewerPortalURL: r.viewerPortalURL, SecurePortalURL: r.viewerPortalSecureURL, SurfaceURL: func(ctx context.Context, req *http.Request) (string, error) {
			return scorecardSurfaceURL(ctx, r, id, req)
		}, PublicOrigin: publicOrigin, RepoFileURL: repoFile, TracePlayback: a.tracePlayback, ServerIdentity: serverIdentity, OwnerSubscription: ownerSub, LANViewer: lanViewer, PageFreshness: func() frontend.PageFreshness {
			f := a.browseFreshness()
			if imageRevision != nil {
				f.Images = imageRevision()
			}
			if grants != nil {
				f.Grants = grants.Generation()
			}
			return f
		}, Config: settings, HostConfig: hostConfig, Now: parityClock(), MetadataLanguage: func() string {
			if a.cfg == nil {
				return ""
			}
			return a.cfg.Get().Catalog.Language
		}, AudioOrder: trackprefs.AudioOrder, PickAudio: experiencepkg.PickAudio, PersonalHomes: a.personalHomeDisk(), CacheDir: a.frontendCacheDir(id), ClientsDir: a.frontendClientsDir()}
	})
	r.control = newFrontendControl(r.registry, a.cfg)
	r.control.listeners.log = a.log
	r.control.listeners.perf = a.perf
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
	r.plex = plex.NewFrontend()
	if err := r.registry.Register(r.plex); err != nil {
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
	if err := r.registry.Register(xtream.Frontend{}); err != nil {
		return nil, err
	}
	if err := r.registry.Register(m3u.Frontend{}); err != nil {
		return nil, err
	}
	// Plugin-provided protocol surfaces (spec §6.5): register one generic
	// adapter frontend per declaration and keep them in step with plugin
	// lifecycle. OnSync replays every installed plugin, so this also does
	// the initial registration.
	if _, err := a.buildAdapterController(r.registry); err != nil {
		return nil, err
	}
	if cfg.Frontends == nil || cfg.Frontends["plex"].Enabled {
		if err := r.control.listeners.start(ctx, "plex"); err != nil {
			return nil, err
		}
		r.publishLibrarySections()
	}
	if a.deployment != nil && a.deployment.playback != nil {
		// Resolve the current protocol mounts through the original public dispatcher.
		if receiver, ok := a.deployment.playback.(interface{ BindMediaBrowserDelivery(http.Handler) error }); ok {
			if err := receiver.BindMediaBrowserDelivery(r.plexHandler()); err != nil {
				return nil, err
			}
		}
		if receiver, ok := a.deployment.playback.(interface{ BindPlexDelivery(http.Handler) error }); ok {
			// Replay the entire public protocol handler, including current auth/grant
			// middleware and dynamic mount. Never bind a bare representation callback.
			if err := receiver.BindPlexDelivery(r.plexHandler()); err != nil {
				return nil, err
			}
		}
	}
	initialized = true
	return r, nil
}

func (a *App) frontendClientIP(req *http.Request) (netip.Addr, bool) {
	ip, _ := identity.ClientNet(req, a.trustedProxyPrefixes()...)
	return ip, ip.IsValid()
}

// trustedProxyPrefixes caches the parsed trusted-proxy list. Reading it
// used to clone the whole configuration, and a play does that several times.
func (a *App) trustedProxyPrefixes() []netip.Prefix {
	gen := a.cfg.Generation()
	a.proxyMu.Lock()
	defer a.proxyMu.Unlock()
	if a.proxyReady && a.proxyGen == gen {
		return a.proxyPrefixes
	}
	a.proxyPrefixes = identity.ProxyPrefixes(a.cfg.TrustedProxyCIDRs())
	a.proxyGen = gen
	a.proxyReady = true
	return a.proxyPrefixes
}
func (r *frontendRuntime) plexHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if handler := r.control.listeners.sharedHandler(req.URL.Path); handler != nil {
			handler.ServeHTTP(w, req)
			return
		}
		if siloProbeMiss(w, req) {
			return
		}
		inst, ok := r.registry.Instance("plex")
		if !ok {
			http.NotFound(w, req)
			return
		}
		r.control.listeners.observe("plex", inst.Handler()).ServeHTTP(w, req)
	})
}

func (r *frontendRuntime) sharedStatus(id frontend.Protocol, status *frontend.Status) {
	instance, ok := r.registry.Instance(id)
	if !ok {
		return
	}
	if id == "plex" {
		mounted := status.Mounted
		errText := status.Error
		*status = r.plexStatus(context.Background())
		status.Mounted = mounted
		if errText != "" {
			status.Error = errText
		}
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
		// Only the first shared prefix is the address owners type into an
		// app; extra shared prefixes are routing aliases (Silo's root
		// /api/v2 and /api/v1 beside its canonical /silo).
		break
	}
}
func (r *frontendRuntime) plexStatus(ctx context.Context) frontend.Status {
	r.mu.Lock()
	addr := r.mainAddr
	discoveryError := r.discoveryError
	lan := r.lan
	r.mu.Unlock()
	cfg := r.app.cfg.Get()
	st := frontend.Status{TargetVersion: plextv.PMSVersion(cfg), LAN: "unknown", WAN: "unknown", Conformance: "unknown", LiveStreamsKnown: true}
	targets, machine := r.lanInputs()
	currentLAN := !lan.checked.IsZero() && lan.fingerprint == lanFingerprint(targets, machine)
	if !currentLAN || time.Since(lan.checked) >= 5*time.Minute {
		r.refreshLAN()
	}
	if currentLAN {
		st.LAN = lan.state
		st.LANReason = lan.reason
		st.LANCheckedAt = lan.checked
	}
	if discoveryError != "" {
		st.Warnings = append(st.Warnings, discoveryError)
	}
	if addr != nil {
		st.Addresses = []string{addr.String()}
		addresses, _ := net.InterfaceAddrs()
		st.ConnectURLs = connectionURLs(addr.String(), "", addresses)
	}
	if r.app.registrar != nil {
		remote := r.app.registrar.Status()
		// ConnectURLs are the at-home addresses. The public host, custom
		// outside routes and the WAN address belong to Away from home.
		for _, connection := range remote.Connections {
			if connection.Local {
				st.ConnectURLs = append(st.ConnectURLs, connection.URI)
			}
		}
		wan := remote.WANCheck()
		st.WAN, st.WANReason, st.WANAddress, st.WANCheckedAt = wan.State, wan.Reason, wan.Address, wan.CheckedAt
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
	r.publishLibrarySections()
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
	r.lan = lanCheck{}
	r.lanGeneration++
	r.mu.Unlock()
	r.control.Reconcile(r.ctx, r.app.cfg.Get())
	r.publishLibrarySections()
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
	r.lan = lanCheck{}
	r.lanGeneration++
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
	if r.registry != nil {
		_ = r.registry.Shutdown(context.Background())
	}
	if r.app != nil {
		r.app.closePlexAccountServices()
	}
}

func (r *frontendRuntime) publishLibrarySections() {
	if r == nil || r.app == nil || r.app.registrar == nil || r.registry == nil {
		return
	}
	inst, ok := r.registry.Instance("plex")
	if !ok {
		return
	}
	provider, ok := inst.(frontend.PublishedLibraryProvider)
	if !ok {
		return
	}
	secs := provider.PublishedLibraries()
	libraries := make([]plextv.LibrarySection, 0, len(secs))
	for _, sec := range secs {
		libraries = append(libraries, plextv.LibrarySection{
			Key: sec.ID, Type: string(sec.Type), Title: sec.Title,
			UUID: sec.UUID, Language: sec.Language,
		})
	}
	r.app.registrar.SetLibrarySections(libraries)
}

func (r *frontendRuntime) noteOwnerStop(sessionID, reason string) {
	if r == nil || r.registry == nil {
		return
	}
	for _, d := range r.registry.Descriptors() {
		if inst, ok := r.registry.Instance(d.ID); ok {
			if stopper, ok := inst.(frontend.OwnerSessionStopper); ok {
				stopper.NoteOwnerStop(sessionID, reason)
			}
		}
	}
}

func (r *frontendRuntime) notifyLiveLineup(viewerID int64) {
	if r == nil || r.registry == nil {
		return
	}
	for _, d := range r.registry.Descriptors() {
		if inst, ok := r.registry.Instance(d.ID); ok {
			if obs, ok := inst.(frontend.LiveLineupObserver); ok {
				obs.NotifyLiveLineupChanged(viewerID)
			}
		}
	}
}

func (r *frontendRuntime) supportViewer(ctx context.Context, viewer *core.User) (diagnostics.ViewerFacts, error) {
	if r != nil && r.registry != nil {
		if inst, ok := r.registry.Instance("plex"); ok {
			if diag, ok := inst.(frontend.ViewerDiagnostics); ok {
				return diag.SupportViewer(ctx, viewer)
			}
		}
		for _, d := range r.registry.Descriptors() {
			if inst, ok := r.registry.Instance(d.ID); ok {
				if diag, ok := inst.(frontend.ViewerDiagnostics); ok {
					return diag.SupportViewer(ctx, viewer)
				}
			}
		}
	}
	return diagnostics.ViewerFacts{}, errors.New("Plex unavailable")
}

func (r *frontendRuntime) prune(ctx context.Context, id frontend.Protocol) (int64, error) {
	if r == nil || r.registry == nil {
		return 0, nil
	}
	return r.registry.Prune(ctx, id)
}

// frontendCacheDir is the node-local directory a frontend may fill with
// files it regenerates (Xtream's whole-library exports), under the data
// directory's cache, which Kubernetes keeps on the writable data volume.
func (a *App) frontendCacheDir(id frontend.Protocol) string {
	if a == nil || a.opts.DataDir == "" {
		return ""
	}
	return filepath.Join(a.opts.DataDir, "cache", "frontends", string(id))
}

// frontendClientsDir is the persistent directory for third-party client
// builds downloaded at runtime (the official Jellyfin Web build the native
// apps load from /web). It must survive restarts, unlike the node-local
// frontend cache, so a completed download is reused forever.
func (a *App) frontendClientsDir() string {
	if a == nil || a.opts.DataDir == "" {
		return ""
	}
	return filepath.Join(a.opts.DataDir, "clients")
}

// backgroundBusy counts work mounted frontends run behind their
// requests (Plex home/enrichment and Xtream export builds).
func (r *frontendRuntime) backgroundBusy() int {
	if r == nil || r.registry == nil {
		return 0
	}
	busy := 0
	for _, d := range r.registry.Descriptors() {
		if inst, ok := r.registry.Instance(d.ID); ok {
			if b, ok := inst.(frontend.BackgroundWorker); ok {
				busy += b.BackgroundBusy()
			}
		}
	}
	return busy
}

func (a *App) browseFreshness() frontend.PageFreshness {
	f := frontend.PageFreshness{ArtBucket: time.Now().Unix() / 120}
	if a == nil {
		return f
	}
	if a.catalog != nil {
		f.Catalog = a.catalog.WriteGeneration()
	}
	if a.cfg != nil {
		f.Config = a.cfg.Generation()
	}
	if a.state != nil {
		f.Watch = a.state.Generation()
	}
	if a.favorites != nil {
		f.Favorite = a.favorites.Generation()
	}
	if a.notInterested != nil {
		f.Hidden = a.notInterested.Generation()
	}
	if a.watchlist != nil {
		f.Listed = a.watchlist.Generation()
	}
	if a.users != nil {
		f.Accounts = a.users.Generation()
	}
	if a.safety != nil {
		f.Safety = a.safety.Generation()
	}
	if a.personalize != nil {
		f.Layout = a.personalize.LayoutVersion()
	}
	if a.libraries != nil {
		f.Libraries = a.libraries.Generation()
	}
	if a.pluginsRT != nil && a.pluginsRT.pluginitems != nil {
		f.KindSync = a.pluginsRT.pluginitems.Generation()
	}
	return f
}
