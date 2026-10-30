package app

import (
	"context"
	"net/http"
	"runtime"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plex"
	"github.com/braedonsaunders/ether/internal/plexguid"
	"github.com/braedonsaunders/ether/internal/plextv"
	"github.com/braedonsaunders/ether/internal/transcode"
)

// buildPlex assembles the PMS-compatible front end. In spike mode the
// library is the configured local files instead of the TMDB catalogue, but
// auth, plex.tv registration and the transcoder are the real ones, so the
// spike exercises the same path official apps will use.
func (a *App) buildPlex(cfg config.Config) (http.Handler, error) {
	a.plexBuildMu.Lock()
	defer a.plexBuildMu.Unlock()
	if a.plexServer != nil {
		return a.plexServer, nil
	}
	var tc core.Transcoder
	if a.transcoder != nil {
		tc = a.transcoder
	}
	identity := plex.Identity{
		MachineIdentifier: cfg.Server.MachineIdentifier,
		FriendlyName:      cfg.Server.FriendlyName,
		Version:           plextv.DefaultServerVersion,
		Platform:          plexPlatform(),
	}
	hooks := plex.Hooks{
		OnClientSeen: func(id string) { a.frontendClients.observe("plex", id) },
		OnTimeline: func(u *core.User, key core.RatingKey, offset, duration time.Duration, state string) {
			if u != nil {
				// Plex renews/stops the requesting client's exact session.
				// A title-wide renewal here would preserve abandoned streams
				// on another device watching the same title.
				// Session intent (#32): a detail open that just turned
				// into this playback was a conversion, not browsing.
				if state == "playing" {
					a.personalize.NotePlayback(u, key)
				}
			}
			a.prefetch.OnTimeline(u, key, offset, duration, state)
		},
	}

	var deps plex.Deps
	if cfg.Spike.Enabled {
		deps = a.spikeLibrary.Deps()
		deps.Auth = a.users
		if tc != nil {
			deps.Transcoder = tc
		}
		hooks.OnTimeline = nil
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
	deps.Variants = a.resolver // "Play Version" release picker (cache-only)
	deps.PlexGUID = plexguid.New(a.db, plexguid.Options{
		Token:      func() string { return a.cfg.Get().Plex.Token },
		Background: true,
	})

	deps.EpisodeOrder = a.episodeorder
	deps.Discovery = a.discoveryFeatures
	deps.LiveTV = a.livetv
	deps.Subtitles = plexSubtitles{a}
	deps.TrackPreferences = a.trackprefs
	deps.SessionCoordination = a.sessionCoordination
	deps.Markers = a.markers
	deps.Downloads = a.downloadRemuxer

	deps.Extras = a.extras
	if a.transcoder != nil {
		deps.Trickplay = transcode.NewThumbnails(transcode.ThumbOptions{
			FFmpeg: a.transcoder.FFmpegPath(), Logger: a.log,
		})
	}
	a.wirePlexPolicy(&deps)
	deps.FavoriteShows = a.favoriteShows
	deps.Now = parityClock()
	deps.Config = a.cfg.Get
	deps.Logger = a.log.With("component", "plex")
	deps.Identity = identity
	deps.OwnerSubscription = func() (bool, []string) {
		st := a.registrar.Status()
		return st.PlexPass, st.OwnerFeatures
	}
	deps.Hooks = hooks
	if node := a.clusterNode(); node != nil && deps.DB != nil {
		deps.Cluster = node
	}
	if a.artwork != nil {
		deps.ArtworkImages = a.artwork.Images
		deps.ArtworkPalettes = a.artwork.Palettes
	}
	srv := plex.NewServer(deps)
	a.plexServer = srv
	libraries := make([]plextv.LibrarySection, 0)
	for _, sec := range srv.LibrarySections() {
		libraries = append(libraries, plextv.LibrarySection{
			Key: sec.ID, Type: string(sec.Type), Title: sec.Title,
			UUID: sec.UUID, Language: sec.Language,
		})
	}
	a.registrar.SetLibrarySections(libraries)
	a.plexClose = func() { srv.Close(); deps.PlexGUID.Close() }
	return srv, nil
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
