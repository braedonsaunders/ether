package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/cardigann"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/debrid"
	"github.com/braedonsaunders/ether/internal/egress"
	"github.com/braedonsaunders/ether/internal/livetv"
	"github.com/braedonsaunders/ether/internal/newznab"
	"github.com/braedonsaunders/ether/internal/resolver"
	"github.com/braedonsaunders/ether/internal/segcache"
	"github.com/braedonsaunders/ether/internal/stream"
	"github.com/braedonsaunders/ether/internal/stremio"
	"github.com/braedonsaunders/ether/internal/sysload"
	"github.com/braedonsaunders/ether/internal/usenet"
)

func (a *App) buildSources(ctx context.Context, cfg config.Config) error {
	log := a.log
	dataDir := a.opts.DataDir
	var err error
	// Usenet, cache, stream engine. The dialer reads the live proxy policy.
	a.applyOutbound(cfg)
	a.fetcher = usenet.New(cfg.Usenet.Providers, usenet.Options{Logger: log.With("component", "usenet"), MinIdle: cfg.Usenet.MinIdle, DialContext: egress.Dial, OnStateChange: a.publishDashboard})
	a.closers = append(a.closers, func() { a.fetcher.Close() })
	if cfg.Cache.Storage == "shared" {
		if cfg.SharedCache == nil {
			return errors.New("configure the shared cache policy in Server → Cache before starting a NAS-backed node")
		}
		a.cache, err = segcache.OpenShared(cfg.Cache.Dir, cfg.Cache.BudgetBytes, a.db, a.cacheNodeID(), log)
	} else {
		a.cache, err = segcache.Open(cfg.Cache.Dir, cfg.Cache.BudgetBytes, log)
	}
	if err != nil {
		return fmt.Errorf("segment cache: %w", err)
	}
	a.closers = append(a.closers, func() { a.cache.Close() })
	// Keep part of the cache's volume free whatever the budget says.
	cacheDir := a.cache.Directory()
	diskUsage := a.opts.diskUsage
	if diskUsage == nil {
		diskUsage = func() (int64, int64, error) { return admin.DiskUsage(cacheDir) }
	}
	a.cache.SetDiskUsage(diskUsage)
	a.configureCache(cfg.Cache)
	a.engine = stream.New(a.fetcher, a.cache, stream.OptionsFromConfig(cfg), log.With("component", "stream"))
	a.closers = append(a.closers, func() { a.engine.Close() })

	// Indexers and resolver.
	siteOpts := cardigann.Options{DB: a.db, Sealer: a.cfg, Logger: log.With("component", "cardigann"), Sites: cfg.IndexerSites}
	if a.clustered() {
		siteOpts.LoginLock = a.clusterLoginLock // one login per site across nodes
	}
	a.sites = cardigann.New(siteOpts)
	a.addons = stremio.NewService(nil)
	a.indexers = newznab.NewManager(a.db, a.addonIndexers(context.Background(), cfg.Indexers), newznab.Options{
		Logger:         log.With("component", "newznab"),
		SearchCacheTTL: cfg.Resolver.SearchCacheTTL.D(),
		NZBDir:         filepath.Join(dataDir, "nzb"),
		ClientIdentity: cfg.ClientIdentity,
		Backend: func(ix config.Indexer) (newznab.Backend, error) {
			if ix.IsStremio() {
				return a.addons.Backend(ix)
			}
			// An installed plugin's streams (made at run time, never saved).
			if ix.IsAddon() {
				if ss := a.streamSources(); ss != nil {
					return ss.Backend(ix)
				}
				return nil, fmt.Errorf("indexer %q: add-ons are not available yet", ix.Name)
			}
			return a.sites.Backend(ix)
		},
	})
	a.indexers.Subscribe(func(ev newznab.Event) {
		if !ev.Success || ev.BudgetExhausted || (a.streamRT != nil && a.streamRT.Subscribers() > 0) {
			a.publishDashboard()
		}
	})
	a.debrid = debrid.NewService(cfg.Debrid, debrid.Options{AcquireStreamProvider: func(ctx context.Context, id string) (func(), error) {
		if a.livetv == nil {
			return nil, core.ErrSourceUnavailable
		}
		return a.livetv.AcquireProvider(ctx, id)
	}, DB: a.db, Logger: log.With("component", "debrid"), OnChange: a.publishDashboard})
	ss := a.initStreamSources(a.indexers)
	ss.SetAnimeIDs(a.addons.AnimeIDs())
	usenetReady := func() bool {
		if a.fetcher != nil {
			return a.fetcher.HasUsableProvider()
		}
		for _, p := range a.cfg.Get().Usenet.Providers {
			if p.Enabled {
				return true
			}
		}
		return false
	}
	ss.SetUsenetReady(usenetReady)
	eng := ss.WrapEngine(a.engine)
	a.media = debrid.NewEngine(a.debrid, debrid.EngineOptions{Inner: eng, Health: eng, Cache: a.cache, Logger: log.With("component", "debrid")})
	a.closers = append(a.closers, func() { a.media.Close() })
	a.sources = debrid.NewIndexer(&livetv.VODIndexer{Inner: ss, Service: func() *livetv.Service { return a.livetv }}, a.debrid)
	a.sources.SetPackNamer(a.engine)
	a.sources.SetUsenetReady(func() bool {
		return usenetReady() || a.hasEnabledPluginSources()
	})
	indexer := core.Indexer(parityIndexer(a.sources, dataDir))
	health := core.HealthChecker(parityHealth(a.media, dataDir))
	animeCour := resolver.AnimeCour(a.addons)
	if a.opts.FixtureMedia != nil {
		indexer, health = a.opts.FixtureMedia, a.opts.FixtureMedia
		animeCour = nil
	}
	a.resolver = resolver.New(indexer, health, a.db, cfg, resolver.Options{Now: parityClock(), SearchObserved: a.observeSearch, Discovered: a.discoveredReleases, Observed: a.observeRelease, Invalidated: a.invalidateRelease, SeriesYearConflict: a.seriesYearConflict, AnimeCour: animeCour, LibraryProfiles: newLibraryProfiles(a), Logger: log.With("component", "resolver")})
	a.resolver.SetSourceSignals(a.debrid)
	a.closers = append(a.closers, func() { a.resolver.Close() })

	a.load = sysload.New(sysload.Options{Logger: log.With("component", "sysload")})

	return nil
}
