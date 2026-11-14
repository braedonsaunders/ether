package app

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/discovery"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/langfeat"
	"github.com/braedonsaunders/ether/internal/overlays"
	"github.com/braedonsaunders/ether/internal/personalize"
	"github.com/braedonsaunders/ether/internal/plexguid"
	"github.com/braedonsaunders/ether/internal/ratings"
	"github.com/braedonsaunders/ether/internal/watchlist"
	"net/http"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

func (a *App) buildDiscovery() {
	a.ratings = ratings.New(a.db, ratings.Options{Sealer: a.cfg, Certification: a.catalog.ApplyProviderCertification, Unrated: a.catalog.UnratedTitles})
	var sourceResolver core.Resolver
	var sourceEngine core.Engine
	var sourceCache core.CacheProvenanceRecorder
	if a.resolver != nil {
		sourceResolver = a.resolver
	}
	if a.media != nil {
		sourceEngine = a.media
	}
	if a.cache != nil {
		sourceCache = a.cache
	}
	a.watchlist = watchlist.New(a.db, watchlist.Options{
		Logger:     a.log.With("component", "watchlist"),
		Sealer:     a.cfg,
		Catalog:    a.catalog,
		Resolver:   sourceResolver,
		Engine:     sourceEngine,
		Cache:      sourceCache,
		Trakt:      a.trakt,
		Users:      a.users.Users,
		Allowed:    a.safety.Allowed,
		Notifier:   a.notifications,
		Tokens:     a.discoveryToken,
		OwnerToken: func(context.Context) (string, error) { return a.cfg.Read().Plex.Token, nil },
		PlexLookup: func(ctx context.Context, guid string) (core.RatingKey, error) {
			if a.plexGUID == nil {
				return 0, core.ErrNotFound
			}
			return a.plexGUID.Lookup(ctx, guid)
		},
		ClientIdentifier: plexguid.ClientIdentifier(a.cfg.Get().Plex.ClientIdentifier, a.cfg.Get().Server.MachineIdentifier),
		PrewarmBytes:     a.cfg.Get().Cache.PrefetchBytes,
		ResumeSchedule:   a.clustered(), // a new leader continues the schedule
		OnAvailable: func(ctx context.Context, userID int64, key core.RatingKey) {
			if a.sessionEvents == nil {
				return
			}
			if err := a.sessionEvents.Publish(ctx, frontend.Event{Type: "LibraryChanged", ViewerID: userID, Item: key}); err != nil && a.log != nil {
				a.log.Warn("watchlist available event", "err", err)
			}
		},
		OnFriendsRSSChanged: func(ctx context.Context) {
			if a.collections != nil {
				if err := a.collections.SyncFriendsWatchlist(ctx); err != nil && a.log != nil {
					a.log.Warn("watchlist friends collection sync failed", "err", err)
				}
			}
			if a.personalize != nil {
				a.personalize.InvalidateOwnerRows()
			}
		},
	})
	if a.collections != nil {
		ensureCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := a.collections.EnsureFriendsWatchlist(ensureCtx); err != nil && a.log != nil {
			a.log.Warn("watchlist friends collection startup check failed", "err", err)
		}
		cancel()
	}
	a.discoveryPersonal = personalize.NewDiscovery(a.db, a.catalog, a.state, a.safety.Allowed)
	a.discoveryPersonal.NotInterested = a.notInterestedSignals
	a.overlays = overlays.New(a.db, overlays.Options{CacheDir: filepath.Join(a.opts.DataDir, "cache", "overlays"), Images: a.images, Allowed: a.safety.Allowed, Ratings: a.ratings, FactsFor: a.overlayFacts, Variants: a.artworkVariants, Taste: func(ctx context.Context, u *core.User) []string {
		p, err := a.personalize.Taste(ctx, u)
		if err != nil {
			return nil
		}
		var tags []string
		for _, g := range p.Genres {
			if g.Weight > 0 {
				tags = append(tags, strings.ToLower(g.Name))
			}
		}
		for _, person := range p.People {
			if person.Weight > 0 {
				tags = append(tags, strings.ToLower(person.Name))
			}
		}
		for _, kw := range p.Keywords {
			if kw.Weight > 0 {
				tags = append(tags, strings.ToLower(kw.Name))
			}
		}
		return tags
	}})
	a.discoveryFeatures = &discovery.Features{Watchlist: a.watchlist, Ratings: a.ratings, Overlays: a.overlays, Personal: a.discoveryPersonal, LLM: a.llm, Localize: a.tmdb.Localized}
	a.lang = langfeat.New(langfeat.Options{
		Dir:        filepath.Join(a.opts.DataDir, "cache", "langfeat"),
		Gates:      a.languageGates,
		Translate:  a.languageChat,
		Transcribe: a.languageTranscribe,
		Subtitles:  a.subtitles,
		Audio:      a.languageAudio,
		SourceText: a.languageSourceText,
	})
	a.discoveryFeatures.OverviewTranslate = a.lang.Overview
}
func (a *App) overlayFacts(ctx context.Context, u *core.User, t *core.Title, status, quality bool) overlays.Facts {
	f := overlays.Facts{}
	if status {
		if expiry, err := a.collections.LastChance(ctx, u, t.Key); err == nil {
			f.LastChance = expiry
		}
	}
	if status && t.Type == core.TypeShow && u != nil {
		episodes, err := a.catalog.RecentEpisodes(ctx, t.Key, time.Now().Add(-30*24*time.Hour))
		if err == nil {
			keys := make([]core.RatingKey, len(episodes))
			for i, ep := range episodes {
				keys[i] = ep.Key
			}
			if watched, err := a.state.GetMany(ctx, u.ID, keys); err == nil {
				for _, ep := range episodes {
					if watched[ep.Key].ViewCount == 0 {
						f.NewEpisode = true
						if ep.Episode == 1 {
							f.NewSeason = true
						}
					}
				}
			}
		}
	}
	// Only exact facts belonging to the selected viewer can decorate their
	// poster. Historical probes survive a restart and are bound to the current
	// resume release by the playback package; no resolving/probing happens here.
	if quality && u != nil {
		if a.playback != nil {
			if observed, err := a.playback.ObservedMediaInfo(ctx, u.ID, t.Key); err == nil && observed != nil {
				applyOverlayQuality(&f, observed.Info)
			}
		} else if a.brokerPlayback != nil {
			// Remote preparation snapshots contain exact owner-probed metadata for an
			// immutable pinned revision. Do not resolve/probe/network during artwork.
			for _, session := range a.brokerPlayback.Sessions() {
				if session != nil && session.UserID == u.ID && session.Key == t.Key && !session.Info.Estimated {
					applyOverlayQuality(&f, session.Info)
					break
				}
			}
		}
	}
	return f
}
func (a *App) discoveryHandlers(m map[string]http.Handler) {
	base := a.watchlist.Router()
	m["discovery"] = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/translate" {
			a.translateOverview(w, r)
			return
		}
		base.ServeHTTP(w, r)
	})
	m["discovery/ratings"] = a.ratings.Router(a.catalog)
	m["discovery/overlays"] = a.overlays.Router(a.catalog, func(r *http.Request, id int64) (*core.User, error) {
		u, err := a.users.User(r.Context(), id)
		if err != nil {
			return nil, err
		}
		return a.safety.UserPolicy(u), nil
	})
}

func (a *App) discoveryToken(ctx context.Context, id int64) (string, error) {
	u, err := a.users.User(ctx, id)
	if err != nil {
		return "", err
	}
	return discoveryViewerToken(ctx, u, a.users.HomeMember(id), a.cfg.Read().Plex.Token, a.plextv.HomeUserToken)
}

func discoveryViewerToken(ctx context.Context, u *core.User, home bool, ownerToken string, switchHome func(context.Context, string, int64, string) (string, error)) (string, error) {
	if u.IsOwner {
		return ownerToken, nil
	}
	if home && u.PlexID > 0 {
		return switchHome(ctx, ownerToken, u.PlexID, "")
	}
	return "", core.ErrUnauthorized
}

func (a *App) artworkVariants(ctx context.Context, t *core.Title, lang string) (overlays.Artwork, error) {
	images, err := a.tmdb.ArtworkVariants(ctx, t, lang)
	if err != nil {
		return overlays.Artwork{}, err
	}
	out := overlays.Artwork{}
	for _, img := range images.Posters {
		out.Posters = append(out.Posters, overlays.Variant{Path: img.FilePath, Language: img.ISO639})
	}
	for _, img := range images.Backdrops {
		out.Backdrops = append(out.Backdrops, overlays.Variant{Path: img.FilePath, Language: img.ISO639})
	}
	return out, nil
}

// Keep one release's facts together: never combine 4K from one stream/release
// with Dolby Vision or Atmos from an unrelated one.
func applyOverlayQuality(f *overlays.Facts, info core.MediaInfo) {
	if info.Estimated {
		return
	}
	video := info.Video()
	if video == nil || video.Height <= 0 {
		return
	}
	f.Width = video.Width
	f.Height = video.Height
	f.HDR = video.ColorTransfer == "smpte2084" || video.ColorTransfer == "arib-std-b67"
	f.DolbyVision = video.DolbyVision
	f.Atmos = false
	for _, stream := range info.Streams {
		if stream.Type == "audio" && (strings.EqualFold(stream.Codec, "truehd") || strings.EqualFold(stream.Codec, "eac3")) {
			for _, word := range strings.FieldsFunc(strings.ToLower(stream.Profile), func(r rune) bool { return !unicode.IsLetter(r) }) {
				if word == "atmos" {
					f.Atmos = true
				}
			}
		}
	}
}
