package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/braedonsaunders/ether/internal/cardigann"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/debrid"
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
	// Usenet, cache, stream engine.
	usenetDial, err := usenetProxyDialer(os.Getenv("ETHER_USENET_PROXY"))
	if err != nil {
		return err
	}
	a.fetcher = usenet.New(cfg.Usenet.Providers, usenet.Options{Logger: log.With("component", "usenet"), MinIdle: cfg.Usenet.MinIdle, DialContext: usenetDial})
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
	a.indexers = newznab.NewManager(a.db, cfg.Indexers, newznab.Options{
		Logger:         log.With("component", "newznab"),
		SearchCacheTTL: cfg.Resolver.SearchCacheTTL.D(),
		NZBDir:         filepath.Join(dataDir, "nzb"),
		Backend: func(ix config.Indexer) (newznab.Backend, error) {
			if ix.IsStremio() {
				return a.addons.Backend(ix)
			}
			return a.sites.Backend(ix)
		},
	})
	a.debrid = debrid.NewService(cfg.Debrid, debrid.Options{DB: a.db, Logger: log.With("component", "debrid")})
	a.media = debrid.NewEngine(a.debrid, debrid.EngineOptions{Inner: a.engine, Health: a.engine, Cache: a.cache, Logger: log.With("component", "debrid")})
	a.closers = append(a.closers, func() { a.media.Close() })
	a.sources = debrid.NewIndexer(a.indexers, a.debrid)
	a.sources.SetUsenetReady(func() bool {
		for _, p := range a.cfg.Get().Usenet.Providers {
			if p.Enabled {
				return true
			}
		}
		return false
	})
	a.resolver = resolver.New(parityIndexer(a.sources, dataDir), parityHealth(a.media, dataDir), a.db, cfg, resolver.Options{Now: parityClock(), SearchObserved: a.observeSearch, Discovered: a.discoveredReleases, Observed: a.observeRelease, Invalidated: a.invalidateRelease, Logger: log.With("component", "resolver")})
	a.resolver.SetSourceSignals(a.debrid)
	a.closers = append(a.closers, func() { a.resolver.Close() })

	a.load = sysload.New(sysload.Options{Logger: log.With("component", "sysload")})

	return nil
}
