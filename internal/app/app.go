// Package app wires Ether's components into one running server: the Plex
// front end and the owner UI on one port, and the background services
// (catalogue sync, plex.tv registration, prefetch, LAN discovery) behind it.
package app

import (
	"context"
	"crypto/tls"
	"database/sql"
	"errors"
	"fmt"
	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/artwork"
	artworkservice "github.com/braedonsaunders/ether/internal/artwork/service"
	"github.com/braedonsaunders/ether/internal/auth"
	"github.com/braedonsaunders/ether/internal/availability"
	"github.com/braedonsaunders/ether/internal/backup"
	"github.com/braedonsaunders/ether/internal/cachefill"
	"github.com/braedonsaunders/ether/internal/cardigann"
	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/cluster"
	"github.com/braedonsaunders/ether/internal/collections"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/debrid"
	"github.com/braedonsaunders/ether/internal/discovery"
	"github.com/braedonsaunders/ether/internal/episodeorder"
	"github.com/braedonsaunders/ether/internal/extras"
	"github.com/braedonsaunders/ether/internal/favorites"
	"github.com/braedonsaunders/ether/internal/handles/capability"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/livetv"
	"github.com/braedonsaunders/ether/internal/llm"
	"github.com/braedonsaunders/ether/internal/logredact"
	"github.com/braedonsaunders/ether/internal/markers"
	"github.com/braedonsaunders/ether/internal/migrate"
	"github.com/braedonsaunders/ether/internal/newznab"
	"github.com/braedonsaunders/ether/internal/notify"
	"github.com/braedonsaunders/ether/internal/notinterested"
	"github.com/braedonsaunders/ether/internal/overlays"
	"github.com/braedonsaunders/ether/internal/personalize"
	"github.com/braedonsaunders/ether/internal/playback"
	"github.com/braedonsaunders/ether/internal/plex"
	"github.com/braedonsaunders/ether/internal/plextv"
	"github.com/braedonsaunders/ether/internal/prefetch"
	"github.com/braedonsaunders/ether/internal/ratings"
	"github.com/braedonsaunders/ether/internal/recommend"
	"github.com/braedonsaunders/ether/internal/remote"
	"github.com/braedonsaunders/ether/internal/resolver"
	"github.com/braedonsaunders/ether/internal/safety"
	"github.com/braedonsaunders/ether/internal/segcache"
	"github.com/braedonsaunders/ether/internal/service"
	"github.com/braedonsaunders/ether/internal/sessions"
	"github.com/braedonsaunders/ether/internal/simkl"
	"github.com/braedonsaunders/ether/internal/spike"
	"github.com/braedonsaunders/ether/internal/state"
	"github.com/braedonsaunders/ether/internal/stats"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/stream"
	"github.com/braedonsaunders/ether/internal/stremio"
	"github.com/braedonsaunders/ether/internal/subtitles"
	"github.com/braedonsaunders/ether/internal/sysload"
	"github.com/braedonsaunders/ether/internal/tmdb"
	"github.com/braedonsaunders/ether/internal/trackprefs"
	"github.com/braedonsaunders/ether/internal/trakt"
	"github.com/braedonsaunders/ether/internal/transcode"
	"github.com/braedonsaunders/ether/internal/updater"
	"github.com/braedonsaunders/ether/internal/usenet"
	"github.com/braedonsaunders/ether/internal/viewerpolicy"
	"github.com/braedonsaunders/ether/internal/watchlist"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Options configure a run.
type Options struct {
	clusterSetup *clusterSetup
	// NotificationExecutable enables the independent crash watcher for executable runs.
	NotificationExecutable string
	DataDir                string
	Version                string
	// Logger receives everything; the admin log ring is teed onto it.
	Logger *slog.Logger
	// Spike forces spike mode (local files, no Usenet) on top of config.
	Spike bool
	// SpikeFiles are extra local files for spike mode.
	SpikeFiles []string
	// DBRetryMin / DBRetryMax bound the backoff while a clustered node
	// waits at startup for an unreachable PostgreSQL server (1s / 30s).
	DBRetryMin, DBRetryMax time.Duration
}

// App is a running Ether server.
type App struct {
	availability *availability.Service
	opts         Options
	log          *slog.Logger
	logs         *admin.LogBuffer
	cfg          *config.Store
	db           *sql.DB
	// databaseURL is the cluster database, placed in join tokens
	// (cluster_settings.go); empty on a standalone server.
	databaseURL string

	// cluster is Solo on SQLite and Postgres on a shared database; peers
	// caches the other nodes' URIs for the plex.tv publisher.
	cluster cluster.Cluster
	peers   *peerSet
	egress  *egressIP // this node's public address, heartbeated when clustered
	// clusterFatal receives the error that ended this node's cluster
	// membership (a duplicate node identity); nil on a standalone server.
	clusterFatal chan error
	// dbAvail is this node's view of shared-database reachability: the
	// heartbeat and the outboxes that hold progress, resume and play
	// records through an outage report to it. Nil on a standalone server.
	dbAvail *store.Availability
	// draining is set while this node drains before installing an update
	// (heartbeated; see rolling.go).
	draining atomic.Bool

	tmdb          *tmdb.Client
	catalog       *catalog.Catalog
	collections   *collections.Service
	images        *artwork.Images
	favorites     *favorites.Service
	notInterested *notinterested.Service
	playlists     core.Playlists
	state         *state.State
	trakt         *trakt.Client
	simklDevice   *simkl.DeviceManager
	llm           *llm.Service
	recommend     *recommend.Recommender
	personalize   *personalize.Engine
	subtitles     *subtitles.Service
	fetcher       *usenet.Fetcher
	cache         *segcache.Cache
	cacheConfig   atomic.Pointer[config.Cache]
	engine        *stream.Engine
	indexers      *newznab.Manager
	sites         *cardigann.Service // built-in indexer sites from owner-installed definitions
	addons        *stremio.Service   // Stremio stream addons configured as indexers
	// Debrid: torrent releases stream through owner debrid accounts. The
	// wrappers dispatch on the payload and delegate Usenet to engine and
	// indexers, so every consumer uses media and sources.
	debrid              *debrid.Service
	media               *debrid.Engine  // core.Engine + core.HealthChecker over engine
	sources             *debrid.Indexer // core.Indexer over indexers
	resolver            *resolver.Service
	transcoder          *transcode.Manager
	load                *sysload.Sampler // machine load for the Overview
	playback            *playback.Manager
	prefetch            *prefetch.Prefetcher
	cachefill           *cachefill.Manager
	plextv              *plextv.Client
	users               *auth.Service
	sessionEvents       *sessions.EventBus
	sessionCoordination *sessions.Coordination

	certs     *plextv.CertManager
	registrar *plextv.Registrar
	gdm       *remote.GDMResponder

	episodeorder      *episodeorder.Service
	watchlist         *watchlist.Service
	ratings           *ratings.Service
	overlays          *overlays.Service
	discoveryPersonal *personalize.Discovery
	discoveryFeatures *discovery.Features
	stats             *stats.Service
	notifications     *notify.Service
	livetv            *livetv.Service
	trackprefs        *trackprefs.Service
	markers           *markers.Service
	extras            *extras.Service
	backups           *backup.Service
	migration         *migrate.Service
	safety            *safety.Service
	updates           *updateState
	frontends         *frontendRuntime
	frontendClients   *frontendClients
	plexServer        *plex.Server
	artwork           *artworkservice.Source
	downloadRemuxer   core.DownloadRemuxer
	plexBuildMu       sync.Mutex
	plexClose         func()
	spikeLibrary      *spike.Library
	handler           http.Handler
	instanceLock      *instanceLock
	closers           []func()
	closeOnce         sync.Once
}

// New builds every component. Nothing listens until Run.
func New(ctx context.Context, opts Options) (*App, error) {
	if opts.DataDir == "" {
		opts.DataDir = config.DefaultDataDir()
	}
	if opts.Version == "" {
		opts.Version = updater.Version
	}
	a := &App{opts: opts, logs: admin.NewLogBuffer(4000)}
	base := opts.Logger
	if base == nil {
		base = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}
	// The in-memory buffer behind Logs gets the same scrubbing as the file.
	a.log = slog.New(logredact.Handler(admin.TeeHandler(base.Handler(), a.logs)))
	slog.SetDefault(a.log)
	var err error
	if a.instanceLock, err = acquireInstanceLock(opts.DataDir); err != nil {
		return nil, err
	}
	a.closers = append(a.closers, a.instanceLock.Release)
	started := false
	defer func() {
		if !started {
			a.Close()
		}
	}()
	if err := a.confirmUpdateStartup(); err != nil {
		return nil, err
	}

	// Opening the config may rewrite it for this version, so capture the
	// files as they are now: a pre-migrate database snapshot stores them, so
	// the snapshot can be restored together with its matching settings.
	snapshotFiles, snapshotErr := preUpgradeFiles(opts.DataDir)
	if snapshotErr != nil {
		a.log.Warn("pre-upgrade companion files incomplete; database snapshot alone may not restore settings and credentials", "err", snapshotErr)
	}
	if a.cfg, err = config.Open(opts.DataDir); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if warning := config.DataDirWarning(opts.DataDir); warning != "" {
		a.log.Warn(warning)
	}
	// ETHER_PORT sets the HTTP port before the UI is reachable, e.g. to run
	// next to a Plex Media Server already using 32400 with host networking.
	// It is saved, so plex.tv, LAN discovery and port mapping all agree.
	if v := strings.TrimSpace(os.Getenv("ETHER_PORT")); v != "" {
		port, perr := strconv.Atoi(v)
		if perr != nil || port < 1 || port > 65535 {
			return nil, fmt.Errorf("ETHER_PORT=%q: want a port between 1 and 65535", v)
		}
		if a.cfg.Get().Server.Port != port {
			if err := a.cfg.Update(func(c *config.Config) error { c.Server.Port = port; return nil }); err != nil {
				return nil, fmt.Errorf("ETHER_PORT: %w", err)
			}
			a.log.Info("server port set from ETHER_PORT", "port", port)
		}
	}
	cfg := a.cfg.Get()
	if opts.Spike {
		_ = a.cfg.Update(func(c *config.Config) error {
			c.Spike.Enabled = true
			c.Spike.LocalFiles = appendUnique(c.Spike.LocalFiles, opts.SpikeFiles...)
			return nil
		})
		cfg = a.cfg.Get()
	}
	dirs := []string{cfg.Transcode.TempDir}
	if cfg.Cache.Storage != "shared" {
		dirs = append(dirs, cfg.Cache.Dir)
	}
	for _, dir := range dirs {
		if dir != "" {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, err
			}
		}
	}
	dbCfg := cfg.Database.Effective(os.Getenv)
	var dialect store.Dialect
	// A clustered node whose PostgreSQL server is briefly unreachable at
	// boot waits for it here (logging every attempt) rather than exiting
	// and being restarted in a tight loop by the service manager. Nothing
	// listens meanwhile — on purpose: a node that cannot reach the shared
	// database must not answer Plex clients or GDM discovery, and it is out
	// of the published connections because it is not heartbeating. The
	// hang watchdog only starts in Run. Errors retrying cannot fix (bad
	// password, missing database, invalid URL) still fail at once.
	var gate store.MigrationGate
	if dbCfg.IsPostgres() {
		id, err := cluster.LoadIdentity(opts.DataDir)
		if err != nil {
			return nil, fmt.Errorf("cluster: %w", err)
		}
		gate = startupMigrationsGate(id.NodeID, cfg.Database.DeferMigrations, clusterLiveAfter, time.Now)
	}
	if a.db, dialect, err = store.OpenWithRetry(ctx, store.Options{
		DataDir: opts.DataDir, Driver: dbCfg.Driver, URL: dbCfg.URL,
		Logger: a.log.With("component", "store"), MigrationGate: gate,
		SnapshotFiles: snapshotFiles,
	}, store.RetryOptions{
		MinBackoff: a.opts.DBRetryMin, MaxBackoff: a.opts.DBRetryMax,
		OnRetry: func(attempt int, wait time.Duration, err error) {
			if errors.Is(err, store.ErrMigrationDeferred) {
				a.log.Warn("database: waiting before migrating the shared schema", "attempt", attempt, "retry", wait, "reason", err)
				return
			}
			a.log.Error("database: PostgreSQL is unreachable; waiting for it before starting",
				"url", dbCfg.Redacted().URL, "attempt", attempt, "retry", wait, "err", err)
		},
	}); err != nil {
		a.Close() // libera o instance lock (o db não foi aberto)
		return nil, fmt.Errorf("database: %w", err)
	}
	a.cfg.SetPolicyEpoch(func() error { return viewerpolicy.Bump(context.Background(), a.db) })
	if dialect == store.Postgres {
		a.log.Info("database: using PostgreSQL", "url", dbCfg.Redacted().URL)
		a.warnNewerSchema(ctx)
	}
	a.closers = append(a.closers, func() { a.db.Close() })
	if err := a.buildCluster(dialect); err != nil {
		a.Close()
		return nil, fmt.Errorf("cluster: %w", err)
	}
	// A restored PostgreSQL-style bundle leaves a logical dump to import.
	if imported, err := backup.ImportPending(ctx, a.db, opts.DataDir); err != nil {
		a.Close()
		return nil, fmt.Errorf("database: %w", err)
	} else if imported {
		a.log.Info("database: imported restored backup")
	}
	if err := a.checkCredentialKey(ctx); err != nil {
		a.Close()
		return nil, err
	}
	if dialect == store.Postgres {
		if err := a.attachCluster(ctx, dbCfg.URL); err != nil {
			a.Close()
			return nil, err
		}
		cfg = a.cfg.Get()
	}
	if err := a.build(ctx, cfg); err != nil {
		a.Close()
		return nil, err
	}
	a.connectClusterHooks()
	a.initUpdates()
	started = true
	return a, nil
}

func (a *App) build(ctx context.Context, cfg config.Config) error {
	cfg.Cache = cfg.EffectiveCache()
	var err error
	if a.safety, err = safety.New(ctx, a.db); err != nil {
		return fmt.Errorf("safety: %w", err)
	}
	// Identity precedes model construction: background builders receive a fully
	// initialized auth service. Scorer consumers are built inside the model's
	// dependency phase before its workers start.
	for _, build := range []func(context.Context, config.Config) error{
		a.buildCatalogue, func(context.Context, config.Config) error { a.safety.SetCatalogPolicy(a.catalog.Visible); return nil }, a.buildIdentity, a.buildSessionEvents, a.buildPersonalization,
		a.buildSources, a.buildPlayback, a.buildPrefetch, a.buildIntegrations,
	} {
		if err := build(ctx, cfg); err != nil {
			return err
		}
	}
	return nil
}

// applyConfig pushes an edited config into the running components.
func (a *App) applyConfig(c config.Config) {
	c.Cache = c.EffectiveCache()
	a.tmdb.UpdateConfig(c.Catalog)
	a.llm.UpdateConfig(c.LLM)
	a.fetcher.UpdateConfig(c.Usenet)
	a.sites.ApplyConfig(c)
	a.indexers.ApplyConfig(c)
	a.engine.UpdateRepair(c.Usenet.Repair)
	a.debrid.UpdateConfig(c.Debrid)
	a.resolver.UpdateConfig(c)
	a.catalog.UpdateConfig(c.Catalog)
	a.configureCache(c.Cache)
	c.Cache = a.runningCacheConfig()
	if a.cachefill != nil {
		a.cachefill.Wake()
	}
	a.playback.SetConfig(c)
	a.prefetch.SetConfig(c.Cache)
	if a.transcoder != nil {
		a.transcoder.UpdateConfig(c.Transcode)
	}
	a.trakt.UpdateConfig(c.Trakt)
	a.subtitles.UpdateConfig(c.OpenSubtitles)
	// Re-publish section titles when Movies / TV Shows library names change.
	a.publishLibrarySections()
	a.registrar.Trigger()
}

// protectedReleases is every release that must survive cache eviction:
// live sessions (playback pins those itself) plus pinned in-progress titles.
func (a *App) protectedReleases() map[string]bool {
	out := map[string]bool{}
	for _, s := range a.playback.Sessions() {
		if s.Resolution != nil {
			out[s.Resolution.Release.ID] = true
		}
	}
	return out
}

// Handler is the combined Plex + owner UI handler (tests).
func (a *App) Handler() http.Handler { return a.handler }

// Config is the settings store.
func (a *App) Config() *config.Store { return a.cfg }

// Run serves until ctx is cancelled.
func (a *App) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cfg := a.cfg.Get()

	// Leave the cluster last, after background work has stopped.
	defer a.cluster.Close()
	if err := a.startCluster(ctx); err != nil {
		if errors.Is(err, context.Canceled) {
			return a.runResult()
		}
		return err
	}
	var background backgroundGroup
	defer background.Wait()
	defer cancel()

	// Singleton work runs only on the cluster leader (always, standalone).
	// trakt.Start stays on every node: it delivers this node's own
	// scrobbles, and its import queue is claimed per row.
	// Tracked like every other background loop, so Run returns (and the
	// caller closes the database) only after they have stopped.
	if parityRunCatalogueWorkers() {
		background.start(func() { a.cluster.RunAsLeader(ctx, "catalogue sync", a.catalog.Run) })
		background.start(func() { a.cluster.RunAsLeader(ctx, "trending charts", a.catalog.RunTrends) })
	}
	background.start(func() { a.cluster.RunAsLeader(ctx, "release availability", a.availability.Run) })
	background.start(func() { a.cluster.RunAsLeader(ctx, "collection refresh", a.collections.Run) })
	background.start(func() { a.trakt.Start(ctx) })
	background.start(func() {
		a.cluster.RunAsLeader(ctx, "simkl delivery", func(ctx context.Context) {
			simkl.RunDelivery(ctx, a.simklService, func() bool { return a.cfg.Get().Simkl.Enabled }, a.log.With("component", "simkl"))
		})
	})
	background.start(func() { a.load.Run(ctx) })
	background.start(func() { a.cache.Run(ctx) })
	background.start(func() { a.cachefill.Run(ctx) })
	if parityRunRegistrar() {
		if a.clustered() {
			background.start(func() { a.runRegistrarAsLeader(ctx) })
			background.start(func() { a.registrar.RunPortMapping(ctx) })
		} else if err := a.registrar.Start(ctx); err != nil {
			a.log.Warn("plex.tv registrar", "err", err)
		}
	}

	background.start(func() { a.housekeeping(ctx) })
	background.start(func() { a.cluster.RunAsLeader(ctx, "database maintenance", a.maintainDB) })
	background.start(func() { a.stats.Run(ctx) })
	background.start(func() { a.cluster.RunAsLeader(ctx, "notification delivery", a.notifications.Run) })
	background.start(func() { a.cluster.RunAsLeader(ctx, "favorite episode alerts", a.runFavoriteEpisodeAlerts) })
	background.start(func() { a.cluster.RunAsLeader(ctx, "viewer push delivery", a.runViewerPush) })
	if a.clustered() {
		background.start(func() { a.sampleHealth(ctx) })
	}
	if parityRunCatalogueWorkers() {
		background.start(func() { a.cluster.RunAsLeader(ctx, "watchlist sync", a.watchlist.Run) })
	}
	background.start(func() { a.ratings.Run(ctx) })
	background.start(func() { a.discoveryFeatures.Run(ctx) })
	background.start(func() {
		a.cluster.RunAsLeader(ctx, "scheduled backups", func(ctx context.Context) {
			alerts := &backupAlerts{log: a.log}
			if a.notifications != nil {
				alerts.notify = func(err error) error { return a.notifications.BackupFailed(ctx, err) }
			}
			a.backups.Run(ctx, alerts.observe)
		})
	})
	background.start(func() { a.runUpdates(ctx, cancel) })
	background.start(func() { a.runClusterSync(ctx) })
	background.start(func() { a.runBudgets(ctx) })
	background.start(func() { a.runEgress(ctx) })
	background.start(func() { a.runCredentialUpgrade(ctx) })

	ln, err := a.listen(cfg)
	if err != nil {
		return err
	}
	a.frontends.start(ctx, ln.Addr())
	defer a.frontends.stop()
	cfg = a.cfg.Get() // the port may have moved off a busy default
	// Publish the actual listener, including automatic port fallback, without
	// exposing the service's protected configuration to the desktop user.
	if err := service.PublishLocalURL(a.opts.DataDir, strings.TrimSuffix(probeURL(ln.Addr()), "/identity")); err != nil {
		a.log.Warn("publish local tray endpoint", "err", err)
	}
	dl := newDualListener(ln, func() *tls.Config { return a.certs.TLSConfig() })
	srv := &http.Server{
		Handler:           a.handler,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(a.log.Handler(), slog.LevelDebug),
	}
	a.log.Info("Ether listening", "addr", ln.Addr().String(), "ui", fmt.Sprintf("http://localhost:%d/ether/", cfg.Server.Port),
		"version", a.opts.Version, "spike", cfg.Spike.Enabled)

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(dl) }()
	a.notifyLifecycle("server.started", "Ether is online", cfg.Server.FriendlyName+" is ready for connections.")
	// Hang watchdog: restart via the supervisor if we stop answering.
	watchdogErr := make(chan error, 1)
	go func() {
		watchdog := newWatchdog(probeURL(ln.Addr()), a.log.With("component", "watchdog"))
		probe := watchdog.probe
		watchdog.probe = func(ctx context.Context) error {
			if err := probe(ctx); err != nil {
				return err
			}
			if err := a.notifications.Heartbeat(ctx); err != nil && ctx.Err() == nil {
				a.log.Warn("external uptime heartbeat failed")
			}
			return nil
		}
		watchdogErr <- watchdog.run(ctx)
	}()
	var runErr error
	var setupStop <-chan struct{}
	if a.opts.clusterSetup != nil {
		setupStop = a.opts.clusterSetup.stop
	}
	select {
	case <-setupStop:
		runErr = errClusterSetup
	case <-ctx.Done():
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case runErr = <-watchdogErr:
	case runErr = <-a.clusterFatal: // nil channel (never ready) when standalone
	}
	if runErr == nil {
		a.notifyLifecycle("server.stopping", "Ether is stopping", "The server is shutting down or restarting.")
	}
	// Hand leadership over first: another node takes over singleton work
	// and plex.tv publication while this one finishes shutting down.
	a.stepDownForShutdown()
	shutdown, done := context.WithTimeout(context.Background(), 10*time.Second)
	defer done()
	_ = srv.Shutdown(shutdown)
	a.registrar.Stop()
	if runErr != nil {
		return runErr
	}
	return a.runResult()
}

func (a *App) housekeeping(ctx context.Context) {
	pins := time.NewTicker(5 * time.Minute)
	defer pins.Stop()
	imageTimer := time.NewTicker(5 * time.Second)
	defer imageTimer.Stop()
	var imageSweep imageMaintenance
	// The PMS release check is cluster-wide: leader only. Pins and
	// seen-token flushes are this node's own.
	lastCompat := time.Now()
	if a.cluster.IsLeader() {
		a.refreshCompatibility(ctx)
	}
	_ = a.playback.ReconcilePins(ctx)
	for {
		select {
		case <-ctx.Done():
			_ = a.users.FlushSeen(context.Background())
			return
		case <-imageTimer.C:
			err := imageSweep.pass(ctx, a.cluster.IsLeader(), time.Now, func(ctx context.Context, now time.Time, shard int64) error {
				_, err := identity.PruneImageGrantShard(ctx, a.db, now, shard)
				return err
			})
			if err != nil && ctx.Err() == nil {
				a.log.Warn("image grant maintenance failed", "err", err)
			}
		case <-pins.C:
			if a.cluster.IsLeader() {
				pruneCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				_, err := capability.PruneExpired(pruneCtx, a.db, time.Now())
				cancel()
				if err != nil && ctx.Err() == nil {
					a.log.Warn("ticket maintenance failed", "err", err)
				}
			}
			if a.cluster.IsLeader() && time.Since(lastCompat) > 6*time.Hour {
				lastCompat = time.Now()
				a.refreshCompatibility(ctx)
			}
			_ = a.playback.ReconcilePins(ctx)
			_ = a.users.FlushSeen(ctx)
		}
	}
}

// Close releases every component in reverse order of construction.
func (a *App) Close() {
	a.closeOnce.Do(func() {
		for i := len(a.closers) - 1; i >= 0; i-- {
			a.closers[i]()
		}
	})
}

func appendUnique(xs []string, more ...string) []string {
	seen := map[string]bool{}
	for _, x := range xs {
		seen[x] = true
	}
	for _, m := range more {
		if m = strings.TrimSpace(m); m != "" && !seen[m] {
			xs = append(xs, m)
			seen[m] = true
		}
	}
	return xs
}

// fallbackPorts are tried, in order, when the default port 32400 is already
// taken, normally by a Plex Media Server on the same machine.
var fallbackPorts = []int{32450, 32451, 32452, 32453, 32454, 32455, 32456, 32457, 32458, 32459}

// listen binds the configured port. When that is the default 32400 and it
// is in use, Ether moves to the first free fallback port and saves it, so
// it runs next to Plex out of the box; plex.tv publishes the port, so Plex
// apps follow. A port the owner chose (or ETHER_PORT) is never moved.
func (a *App) listen(cfg config.Config) (net.Listener, error) {
	addr := net.JoinHostPort(cfg.Server.BindAddress, strconv.Itoa(cfg.Server.Port))
	ln, err := net.Listen("tcp", addr)
	if err == nil {
		return ln, nil
	}
	if cfg.Server.Port != config.DefaultPort || !addrInUse(err) || os.Getenv("ETHER_PORT") != "" {
		return nil, fmt.Errorf("listen %s: %w", addr, err)
	}
	for _, port := range fallbackPorts {
		alt, aerr := net.Listen("tcp", net.JoinHostPort(cfg.Server.BindAddress, strconv.Itoa(port)))
		if aerr != nil {
			continue
		}
		if uerr := a.cfg.Update(func(c *config.Config) error { c.Server.Port = port; return nil }); uerr != nil {
			alt.Close()
			return nil, fmt.Errorf("save port %d: %w", port, uerr)
		}
		a.log.Warn("port 32400 is in use (usually by Plex Media Server); Ether moved to another port and saved it",
			"port", port, "ui", fmt.Sprintf("http://localhost:%d/ether/", port))
		return alt, nil
	}
	return nil, fmt.Errorf("listen %s: %w (and ports %d-%d are also in use)", addr, err, fallbackPorts[0], fallbackPorts[len(fallbackPorts)-1])
}

// addrInUse reports a bind failure because the port is taken: EADDRINUSE,
// or WSAEADDRINUSE (10048), which Windows reports instead.
func addrInUse(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	return errno == syscall.EADDRINUSE || errno == 10048
}
