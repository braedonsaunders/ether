package app

import (
	"context"
	"fmt"
	"net/http"
	"runtime"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/egress"
	"github.com/braedonsaunders/ether/internal/plex"
	"github.com/braedonsaunders/ether/internal/plexguid"
	"github.com/braedonsaunders/ether/internal/plextv"
	"github.com/braedonsaunders/ether/internal/resolver"
)

// buildPlex assembles the PMS-compatible front end. In spike mode the
// library is the configured local files instead of the TMDB catalogue, but
// auth, plex.tv registration and the transcoder are the real ones, so the
// spike exercises the same path official apps will use.
func (a *App) buildPlex(ctx context.Context, cfg config.Config) (http.Handler, error) {
	a.plexBuildMu.Lock()
	defer a.plexBuildMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if a.plexServer != nil && !a.plexServer.Closed() {
		return a.plexServer, nil
	}
	tc := a.activeTranscoder()
	identity := plex.Identity{
		MachineIdentifier: cfg.Server.MachineIdentifier,
		FriendlyName:      cfg.Server.FriendlyName,
		Version:           plextv.DefaultServerVersion,
		Platform:          plexPlatform(),
	}
	hooks := plex.Hooks{
		OnClientSeen: func(id string) { a.frontendClients.observe("plex", id) },
	}

	var deps plex.Deps
	if cfg.Spike.Enabled {
		deps = a.spikeLibrary.Deps()
		deps.Auth = a.users
		if tc != nil {
			deps.Transcoder = tc
		}
	} else {
		deps = plex.Deps{
			Catalog:     a.catalog,
			Collections: a.discoveryPersonal.ReviewCollections(a.collections),
			VibeSearch:  a.collections,
			Recommender: a.personalize,
			State:       a.state,
			Playlists:   a.playlists,
			Auth:        a.users,
			Playback:    a.playback,
			Transcoder:  tc,
			ImageURL:    a.tmdb.ImageURLFunc(),
			DB:          a.db, // plex_ultrablur
		}
	}
	if !cfg.Spike.Enabled && a.availability != nil {
		deps.Catalog = &availabilityCatalog{Catalog: a.catalog, availability: a.availability}
		deps.FilterAvailable = a.availability.FilterDiscovery
	}
	deps.PrioritizeAvailable = func(ctx context.Context, items []core.Title) []core.Title {
		if a.availability == nil {
			return items
		}
		return a.availability.Prioritize(ctx, items)
	}
	deps.Variants = a.resolver       // "Play Version" release picker (cache-only)
	deps.DecisionTraces = a.resolver // latest resolution, for the support capture
	deps.PlexGUID = plexguid.New(a.db, plexguid.Options{
		Token:             func() string { return a.cfg.Read().Plex.Token },
		MachineIdentifier: cfg.Server.MachineIdentifier,
		Background:        true,
	})
	a.plexGUID = deps.PlexGUID
	if err := a.buildPlexWatch(); err != nil {
		return nil, fmt.Errorf("plex watch state: %w", err)
	}
	if !cfg.Spike.Enabled {
		// EpisodeOrder is TMDB-backed (build_integrations.go): metadataTitle
		// prefers it over Catalog, and the real catalog does not know the
		// spike catalog's KindLocal keys, so wiring it in spike mode makes
		// every /library/metadata/<KindLocal> 404 while hubs and play
		// queues (which read Catalog directly) keep working.
		deps.EpisodeOrder = a.episodeorder
	}
	deps.Discovery = a.discoveryFeatures
	deps.LiveTV = a.livetv
	deps.Subtitles = plexSubtitles{a}
	deps.TrackPreferences = a.trackprefs
	deps.SessionCoordination = a.sessionCoordination
	deps.Markers = a.markers
	// The remuxer is built once in buildIntegrations and shared with every
	// front end. Support is the device-scoped "Record a problem" recorder.
	deps.Downloads = a.downloadRemuxer
	deps.Support = a.support
	deps.Extras = a.extras
	deps.Trickplay = a.trickplay.Borrow()
	a.wirePlexPolicy(&deps)
	deps.FavoriteShows = a.favoriteShows
	deps.Now = parityClock()
	deps.Config = a.cfg.Read
	deps.ApplyClientRules = resolver.ApplyClientRules
	deps.HTTPClient = egress.Client(egress.CapMetadata, 20*time.Second)
	deps.DirectHTTP = &http.Client{Transport: egress.Transport(egress.CapDirect)}
	deps.Logger = a.log.With("component", "plex")
	deps.TracePlayback = a.tracePlayback
	deps.Identity = identity
	deps.OwnerSubscription = func() (bool, []string) {
		st := a.registrar.Status()
		return st.PlexPass, st.OwnerFeatures
	}
	deps.Hooks = hooks
	if a.stats != nil {
		deps.Dashboard = a.stats.Dashboard()
	}
	if a.hostResources != nil {
		deps.Resources = a.hostResources
	}
	if node := a.clusterNode(); node != nil && deps.DB != nil {
		deps.Cluster = node
	}
	if a.artwork != nil {
		deps.ArtworkImages = a.artwork.Images
		deps.ArtworkPalettes = a.artwork.Palettes
	}
	srv := plex.NewServer(deps)
	a.plexServer = srv
	if a.livetv != nil {
		a.livetv.SetOnChange(srv.NotifyLiveLineup)
	}
	a.publishLibrarySections()
	// Frontend unmount closes the server and the GUID worker. Server.Close is
	// idempotent, so the app closer may run as well. Watch refresh stays on
	// the Plex mount (ExplicitWatchOnly in spike), not a second subscription.
	a.plexClose = func() { srv.Close(); deps.PlexGUID.Close() }
	a.closers = append(a.closers, func() { srv.Close() })
	return srv, nil
}

// publishLibrarySections pushes the live Movies / TV Shows identities
// (including configured titles) to plex.tv account registration.
func (a *App) publishLibrarySections() {
	if a.plexServer == nil || a.registrar == nil {
		return
	}
	libraries := make([]plextv.LibrarySection, 0)
	for _, sec := range a.plexServer.LibrarySections() {
		libraries = append(libraries, plextv.LibrarySection{
			Key: sec.ID, Type: string(sec.Type), Title: sec.Title,
			UUID: sec.UUID, Language: sec.Language,
		})
	}
	a.registrar.SetLibrarySections(libraries)
}

func plexPlatform() string {
	switch runtime.GOOS {
	case "darwin":
		return "MacOSX"
	case "windows":
		return "Windows"
	default:
		return "Linux"
	}
}

// wirePlexPolicy supplies the same safety service to every Plex deployment.
func (a *App) wirePlexPolicy(deps *plex.Deps) {
	deps.AccessPolicy = a.safety
}

// currentPlexServer publishes lazy construction to playback callbacks. The
// mutex is released before callers invoke the adapter or shared playback.
func (a *App) currentPlexServer() *plex.Server {
	a.plexBuildMu.Lock()
	defer a.plexBuildMu.Unlock()
	return a.plexServer
}
