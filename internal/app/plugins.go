package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/clusterbus"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/extend"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/livetv"
	"github.com/braedonsaunders/ether/internal/notify"
	"github.com/braedonsaunders/ether/internal/playback"
	"github.com/braedonsaunders/ether/internal/plugins"
	"github.com/braedonsaunders/ether/internal/plugins/adapters"
	"github.com/braedonsaunders/ether/internal/plugins/catalogue"
	"github.com/braedonsaunders/ether/internal/plugins/kit"
	"github.com/braedonsaunders/ether/internal/plugins/remote"
	"github.com/braedonsaunders/ether/internal/plugins/studio"
	"github.com/braedonsaunders/ether/internal/plugins/wasm"
	"github.com/braedonsaunders/ether/internal/state"
)

// pluginRuntime is the plugin host and its adapters.
type pluginRuntime struct {
	mgr           *plugins.Manager
	cat           *catalogue.Service
	studio        *studio.Studio
	tunnel        *remote.TunnelServer
	pairing       *remote.Pairing
	pluginHost    http.Handler
	subtitleProbe *adapters.SubtitleProbe
}

func (a *App) buildPlugins(ctx context.Context, cfg config.Config) (*pluginRuntime, error) {
	mgr, err := plugins.New(plugins.Deps{
		OnAirContent: a.pluginContent,
		ImportStylePack: func(ctx context.Context, id string, files map[string][]byte) error {
			if a.onair == nil {
				return fmt.Errorf("On Air is unavailable")
			}
			_, err := a.onair.ImportStyleFiles(ctx, id, files)
			return err
		},
		DB: a.db, Sealer: a.cfg,
		CheckPackage: kit.CheckInstall,
		Registry:     a.extendRT.registry, Tools: a.extendRT.tools, Notifications: a.notifications,
		Logger:   a.log.With("component", "plugins"),
		IsLeader: func() bool { return a.cluster == nil || a.cluster.IsLeader() },
		Bus:      clusterbus.FromCluster(a.cluster),
		Backends: plugins.Backends{
			CollectionsUpsert: a.pluginCollectionUpsert,
			CollectionsDelete: a.pluginCollectionDelete,
			ViewerNames: func(ctx context.Context, ids []int64) (json.RawMessage, error) {
				out := make([]map[string]any, 0, len(ids))
				for _, id := range ids {
					u, err := a.users.ActiveUser(ctx, id)
					if errors.Is(err, core.ErrNotFound) || errors.Is(err, core.ErrUnauthorized) || (err == nil && u == nil) {
						continue
					}
					if err != nil {
						return nil, err
					}
					name := u.Title
					if name == "" {
						name = u.Username
					}
					out = append(out, map[string]any{"id": u.ID, "name": name})
				}
				return json.Marshal(out)
			},
			CatalogSearch: func(ctx context.Context, query string, limit, offset int) (json.RawMessage, error) {
				if offset < 0 || offset > 2000 {
					return nil, errors.New("catalog search offset out of range")
				}
				if offset > 0 {
					limit += offset
				}
				titles, err := a.catalog.Search(ctx, query, []core.MediaType{core.TypeMovie, core.TypeShow}, limit)
				if err != nil {
					return nil, err
				}
				if offset > 0 {
					if offset >= len(titles) {
						titles = nil
					} else {
						titles = titles[offset:]
					}
				}
				type titleSummary struct {
					ID     int64  `json:"id"`
					Title  string `json:"title"`
					Type   string `json:"type"`
					Year   int    `json:"year,omitempty"`
					TMDBID int64  `json:"tmdbId,omitempty"`
					IMDBID string `json:"imdbId,omitempty"`
				}
				out := make([]titleSummary, 0, len(titles))
				for _, t := range titles {
					out = append(out, titleSummary{ID: int64(t.Key), Title: t.Title, Type: string(t.Type), Year: t.Year, TMDBID: t.IDs.TMDB, IMDBID: t.IDs.IMDB})
				}
				return json.Marshal(map[string]any{"titles": out})
			},
			CatalogGet: func(ctx context.Context, id string) (json.RawMessage, error) {
				var title *core.Title
				var err error
				switch {
				case strings.HasPrefix(id, "imdb:"):
					title, err = a.catalog.ByExternalID(ctx, "imdb", strings.TrimPrefix(id, "imdb:"))
				case strings.HasPrefix(id, "tmdb:"):
					var n int64
					n, err = strconv.ParseInt(strings.TrimPrefix(id, "tmdb:"), 10, 64)
					if err == nil && n > 0 {
						title, err = a.catalog.Get(ctx, core.MovieKey(n))
						if errors.Is(err, core.ErrNotFound) {
							title, err = a.catalog.Get(ctx, core.ShowKey(n))
						}
					} else if err == nil {
						err = errors.New("invalid TMDB id")
					}
				default:
					var key core.RatingKey
					key, err = core.ParseRatingKey(id)
					if err == nil {
						title, err = a.catalog.Get(ctx, key)
					}
				}
				if err != nil {
					return nil, err
				}
				if title == nil {
					return nil, core.ErrNotFound
				}
				return json.Marshal(map[string]any{"id": title.Key.String(), "title": title.Title, "type": title.Type, "year": title.Year, "tmdbId": title.IDs.TMDB, "imdbId": title.IDs.IMDB, "genres": title.Genres, "posterPath": title.PosterPath, "summary": title.Summary})
			},
			ViewerHistory: func(ctx context.Context, viewerID int64, limit, offset int) (json.RawMessage, error) {
				if viewerID <= 0 || limit <= 0 || limit > 200 || offset < 0 || offset > 2000 {
					return nil, errors.New("invalid history page")
				}
				history, err := a.state.History(ctx, viewerID, time.Time{}, limit+offset)
				if err != nil {
					return nil, err
				}
				if offset >= len(history) {
					return json.RawMessage(`[]`), nil
				}
				history = history[offset:]
				keys := make([]core.RatingKey, len(history))
				for i, entry := range history {
					keys[i] = entry.Key
				}
				statuses, err := a.state.GetMany(ctx, viewerID, keys)
				if err != nil {
					return nil, err
				}
				out := make([]map[string]any, 0, limit)
				for _, entry := range history {
					k := entry.Key
					kind := "movie"
					if k.Kind() == core.KindEpisode {
						kind = "episode"
					} else if k.Kind() == core.KindShow {
						kind = "show"
					}
					progress := float64(0)
					status := statuses[k]
					if status.Duration > 0 {
						progress = float64(status.ViewOffset) / float64(status.Duration)
					}
					out = append(out, map[string]any{"titleId": k.String(), "type": kind, "watchedAt": entry.ViewedAt, "progress": progress, "completed": entry.Completed})
				}
				return json.Marshal(out)
			},
			Notice: func(text string) {
				a.log.Warn("plugin notice", "text", text)
				if a.streamRT != nil {
					_ = a.streamRT.Publish(context.Background(), admin.StreamNotice, map[string]any{"text": text})
				}
			},
		},
	})
	if err != nil {
		return nil, err
	}
	cat, err := catalogue.New(catalogue.Options{
		DB:        a.db,
		Manager:   mgr,
		IndexURL:  cfg.Plugins.IndexURL,
		PublicKey: cfg.Plugins.PublicKey,
		Log:       a.log.With("component", "plugins-catalogue"),
	})
	if err != nil {
		return nil, err
	}
	mgr.SetCatalogue(cat)

	// Hosted plugins run sandboxed in-process (SPEC §5.2).
	engine := wasm.NewEngine(a.log.With("component", "plugins-wasm"))
	mgr.RegisterRuntime("hosted", plugins.HostedRuntimeFactory(engine))
	if err := cat.EnforceCachedRevocations(ctx); err != nil {
		_ = engine.Close()
		return nil, err
	}
	a.closers = append(a.closers, func() { _ = engine.Close() })
	grants, err := identity.NewGrants(a.db, a.users, 30*24*time.Hour, nil)
	if err != nil {
		a.log.Warn("plugin credentials unavailable", "err", err)
	}
	auth := remote.CredentialResolverFunc(func(ctx context.Context, credential string) (string, error) {
		if grants == nil {
			return "", remote.ErrUnavailable
		}
		_, device, _, err := grants.ResolveGrantDevice(ctx, "plugin", credential, "")
		if err != nil || device.ID == "" {
			return "", remote.ErrUnauthorized
		}
		rec, err := mgr.Get(ctx, device.ID)
		if err != nil || !rec.Enabled || rec.Manifest.Ether == nil || rec.Manifest.Ether.Runtime.Kind != "remote" {
			return "", remote.ErrUnauthorized
		}
		return device.ID, nil
	})
	log := a.log.With("component", "plugins-remote")
	tunnel := remote.NewTunnelServer(remote.TunnelOptions{Auth: auth, Logger: log})
	factory := &remote.Factory{Tunnel: tunnel, TrustedLAN: remote.GrantsLAN(mgr.GrantsOf), Logger: log}
	mgr.RegisterRuntime("remote", factory.RuntimeFactory())
	host := remote.NewHostHandler(remote.HostOptions{Host: remote.HostAdapter{HostFor: mgr.HostFor, SettingsWithSecrets: mgr.PluginSettings}, Auth: auth, Logger: log})
	pairing := remote.NewPairing(a.db, grants, func(ctx context.Context) (int64, error) {
		owner, err := a.users.Owner(ctx)
		if err != nil {
			return 0, err
		}
		if owner == nil || !owner.IsOwner {
			return 0, core.ErrUnauthorized
		}
		return owner.ID, nil
	}, func(ctx context.Context, id string) bool {
		rec, err := mgr.Get(ctx, id)
		return err == nil && rec.Enabled && rec.Manifest.Ether != nil && rec.Manifest.Ether.Runtime.Kind == "remote"
	}, tunnel)
	pairing.Exists = func(ctx context.Context, id string) bool { _, err := mgr.Get(ctx, id); return err == nil }
	pairing.TunnelURL = func(r *http.Request) string {
		scheme, host := "ws", portalSanitizeHost(r.Host)
		if r.TLS != nil {
			scheme = "wss"
		}
		if portalProxyTrusted(r, identity.ProxyPrefixes(a.cfg.TrustedProxyCIDRs())) {
			if forwarded := portalForwardedHost(r); forwarded != "" {
				host = forwarded
			}
			if proto := portalForwardedProto(r); proto == "https" {
				scheme = "wss"
			} else if proto == "http" {
				scheme = "ws"
			}
		}
		if host == "" {
			return ""
		}
		return scheme + "://" + host + remote.TunnelPath
	}
	a.closers = append(a.closers, func() { _ = tunnel.Close() })
	adapters.Attach(mgr, a.collections, a.personalize, a.livetv)
	adapters.AttachSubtitles(mgr, a.subtitles)
	adapters.AttachMetadata(mgr, a.catalog)
	adapters.AttachDebrid(mgr, a.debrid)
	subtitleProbe := adapters.NewSubtitleProbe(mgr, a.catalog)
	a.closers = append(a.closers, subtitleProbe.Close)
	markerCtx, cancelMarkers := context.WithCancel(context.Background())
	markerBridge := adapters.AttachMarkers(markerCtx, mgr, a.markers, a.catalog, func(id string) bool {
		for _, source := range a.cfg.Get().Markers.SourceOrder {
			if source == id {
				return true
			}
		}
		return false
	})
	a.closers = append(a.closers, func() { cancelMarkers(); markerBridge.Wait() })
	if err := mgr.Start(ctx); err != nil {
		return nil, err
	}
	a.bindStreamSources(mgr)
	// Installed plugins that serve streams are run-time indexer rows: keep
	// the running search manager in step with installs, removals and role
	// switches.
	mgr.OnSync(func(context.Context, plugins.Record) { a.reapplyAddonIndexers() })
	mgr.OnRemove(func(context.Context, string) { a.reapplyAddonIndexers() })

	if a.resolver != nil {
		a.resolver.SetRankingHooks(adapters.Ranking{Manager: mgr, Registry: a.extendRT.registry})
	}
	unsubscribe := mgr.SubscribeInvalidations()

	studioSvc := studio.New(studio.Deps{
		Manager: mgr,
		Engine:  engine,
		Publish: func(pubCtx context.Context, event string, data any) error {
			if a.streamRT != nil {
				a.streamRT.Publish(pubCtx, event, data)
			}
			return nil
		},
		Logger: a.log.With("component", "plugins-studio"),
		Installed: func(_ context.Context, rec plugins.Record) {
			a.startPluginLiveTV(rec)
		},
	})

	if err := studioSvc.OpenStorage(ctx, a.db); err != nil {
		return nil, err
	}
	// Removing a plugin removes the drafts it was installed from.
	mgr.OnRemove(func(ctx context.Context, id string) { studioSvc.ForgetPlugin(ctx, id) })

	// Event sources (SPEC §5.7): playback, watch state and notifications.
	publish := func(ev plugins.Event) {
		pctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		mgr.Publish(pctx, ev)
	}
	stopPlayback := func() {}
	if a.playback != nil {
		stopPlayback = a.playback.Subscribe(func(ev playback.Event) {
			if ev.Kind == "start" && ev.Session != nil {
				markerBridge.Schedule(ev.Session.Key, ev.Session.Info.Duration)
			}
			name := plugins.NormalizePlayback(ev.Kind, "")
			if name == "" {
				return
			}
			payload, _ := json.Marshal(map[string]any{"kind": ev.Kind})
			var viewer int64
			if ev.Session != nil {
				viewer = ev.Session.UserID
				payload, _ = json.Marshal(map[string]any{
					"kind": ev.Kind, "ratingKey": int64(ev.Session.Key),
				})
			}
			publish(plugins.Event{Name: name, At: ev.At, ViewerID: viewer, Payload: payload})
		})
	}
	stopState := func() {}
	if a.state != nil {
		stopState = a.state.Subscribe(func(ev state.Event) {
			var name string
			switch ev.Kind {
			case state.EventCompleted, state.EventScrobble:
				name = plugins.EventPlaybackCompleted
			case state.EventProgress:
				name = plugins.NormalizePlayback("update", ev.State)
			}
			if name == "" {
				return
			}
			payload, _ := json.Marshal(map[string]any{"ratingKey": int64(ev.Key), "completed": ev.Completed})
			publish(plugins.Event{Name: name, At: ev.At, ViewerID: ev.UserID, Payload: payload})
		})
	}
	stopNotify := func() {}
	if a.notifications != nil {
		stopNotify = a.notifications.Subscribe(func(ev notify.Event) {
			if ev.Kind != "title.available" && ev.Kind != "user.joined" {
				return
			}
			payload := json.RawMessage(`{}`)
			if ev.Kind == "title.available" {
				payload, _ = json.Marshal(map[string]any{"title": ev.Title, "message": ev.Message})
			}
			publish(plugins.Event{Name: plugins.NormalizeNotify(ev.Kind), At: ev.At, Payload: payload})
		})
	}

	runCtx, cancelRun := context.WithCancel(context.Background())
	go mgr.Run(runCtx)
	go cat.Run(runCtx, catalogue.DefaultRefreshInterval)
	a.closers = append(a.closers, func() {
		cancelRun()
		stopPlayback()
		stopState()
		stopNotify()
		unsubscribe()
	})
	return &pluginRuntime{subtitleProbe: subtitleProbe, mgr: mgr, cat: cat, studio: studioSvc, tunnel: tunnel, pairing: pairing, pluginHost: host}, nil
}

// pluginHandlers mounts the owner plugin API under /api/plugins (WS7, WS11).
func (a *App) pluginHandlers(m map[string]http.Handler) {
	if a.pluginsRT != nil && a.pluginsRT.mgr != nil {
		h := a.pluginsRT.mgr.Handler()
		if a.pluginsRT.studio != nil {
			h = a.pluginsRT.studio.WrapPluginHandler(h)
		}
		if a.pluginsRT.pairing != nil {
			wrapped := a.pluginsRT.pairing.WrapOwner(h)
			h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
				if len(parts) >= 2 && (parts[1] == "pair" || parts[1] == "pairings") {
					access, ok := admin.RequestAccess(r.Context())
					if !ok || !access.Owner {
						http.Error(w, "owner authentication required", http.StatusUnauthorized)
						return
					}
				}
				wrapped.ServeHTTP(w, r)
			})
		}
		m["plugins"] = h
		if a.pluginsRT.subtitleProbe != nil {
			tests := a.pluginsRT.subtitleProbe.Handler()
			m["plugin-tests"] = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				access, ok := admin.RequestAccess(r.Context())
				if !ok || !access.Owner {
					http.Error(w, "owner authentication required", http.StatusUnauthorized)
					return
				}
				tests.ServeHTTP(w, r)
			})
		}
	}
}

// publicExtensionHandlers are console API subtrees that authorise each
// request themselves: the add-on settings relay checks its own token.
func (a *App) publicExtensionHandlers() map[string]http.Handler {
	if a.pluginsRT == nil || a.pluginsRT.mgr == nil {
		return nil
	}
	return map[string]http.Handler{"addon-relay": a.pluginsRT.mgr.RelayHandler()}
}

// mountPlugins mounts the plugin tunnel and plugin host API (WS8b).
func (a *App) mountPlugins(mux *http.ServeMux) {
	if a.pluginsRT == nil {
		return
	}
	if a.pluginsRT.tunnel != nil {
		mux.Handle("GET "+remote.TunnelPath, a.pluginsRT.tunnel.Handler())
	}
	if a.pluginsRT.pluginHost != nil {
		mux.Handle(remote.HostPath, a.pluginsRT.pluginHost)
	}
	if a.pluginsRT.pairing != nil {
		mux.Handle(remote.PairExchangePath, lanOnly(http.HandlerFunc(a.pluginsRT.pairing.Exchange), a.cfg.TrustedProxyCIDRs, nil))
	}
}

// pluginTextResolver resolves plugin strings for the registry (WS7).
func (a *App) pluginTextResolver() extend.TextResolver {
	if a.pluginsRT == nil || a.pluginsRT.mgr == nil {
		return nil
	}
	return a.pluginsRT.mgr.TextResolver()
}

// startPluginLiveTV adds a Live TV source for a channels plugin installed
// from Studio, so its lineup lands in the guide without a second setup step.
// It runs once per install; an owner who later removes the source keeps
// that choice.
func (a *App) startPluginLiveTV(rec plugins.Record) {
	if a.livetv == nil || !rec.Manifest.HasCapability(plugins.CapLiveTV) {
		return
	}
	kind := extend.PluginID(rec.ID, "channels")
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		// The source kind registers when the install syncs; wait for it.
		for attempt := 0; attempt < 10; attempt++ {
			sources, err := a.livetv.Sources(ctx)
			if err == nil {
				for _, s := range sources {
					if s.Kind == kind {
						return
					}
				}
			}
			if _, err = a.livetv.ImportSource(ctx, livetv.Source{Name: rec.Name, Kind: kind}); err == nil {
				return
			}
			a.log.Debug("plugin live tv source not ready", "plugin", rec.ID, "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
	}()
}
