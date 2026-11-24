package plugins

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/agent/tools"
	"github.com/braedonsaunders/ether/internal/artwork"
	"github.com/braedonsaunders/ether/internal/blobcache"
	"github.com/braedonsaunders/ether/internal/clusterbus"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/egress"
	"github.com/braedonsaunders/ether/internal/events"
	"github.com/braedonsaunders/ether/internal/extend"
	"github.com/braedonsaunders/ether/internal/flows"
	"github.com/braedonsaunders/ether/internal/logredact"
	"github.com/braedonsaunders/ether/internal/notify"
	"github.com/braedonsaunders/ether/internal/onair"
	"github.com/braedonsaunders/ether/internal/plugins/wasm"
	"github.com/braedonsaunders/ether/internal/safefetch"
)

// Backends are the Ether services host calls end in. Any may be nil; the
// matching host call then returns ErrUnavailable. App wiring fills them.
type Backends struct {
	CatalogSearch func(ctx context.Context, query string, limit, offset int) (json.RawMessage, error)
	CatalogGet    func(ctx context.Context, id string) (json.RawMessage, error)
	ViewerHistory func(ctx context.Context, viewerID int64, limit, offset int) (json.RawMessage, error)
	ViewerNames   func(ctx context.Context, ids []int64) (json.RawMessage, error)
	// SetWatchState applies a watch-state write and returns its inverse
	// (the previous state) for the journal.
	SetWatchState func(ctx context.Context, viewerID int64, titleID string, watched bool) (previous bool, err error)
	// CollectionsUpsert creates or updates a collection and returns the
	// previous document (empty when new) for the journal.
	CollectionsUpsert func(ctx context.Context, doc json.RawMessage) (previous json.RawMessage, err error)
	// CollectionsDelete removes a collection by the id in its document;
	// used by undo for a create.
	CollectionsDelete func(ctx context.Context, doc json.RawMessage) error
	// Notice posts a console notice ("console notice" in SPEC §5.2).
	Notice func(text string)
	// Kind host services (spec §3.6, §4.5, §5.4): plugin-item store,
	// source resolver and per-library blob cache.
	KindUpsert           func(ctx context.Context, pluginID, kindKey string, items []core.KindItem) (accepted int, err error)
	KindUpsertStats      func(ctx context.Context, pluginID, kindKey string, items []core.KindItem) (KindUpsertResult, error)
	KindDelete           func(ctx context.Context, pluginID, kindKey string, externalIDs []string) error
	KindItems            func(ctx context.Context, pluginID, kindKey, cursor string, limit int) (core.KindPage, error)
	KindItemsByID        func(ctx context.Context, pluginID, kindKey string, externalIDs []string) ([]core.KindItem, error)
	KindItemCount        func(ctx context.Context, pluginID string) (int, error)
	KindVisible          func(ctx context.Context, pluginID string, viewerID int64, kindKey string, ids []string) ([]string, error)
	KindBrowse           func(ctx context.Context, pluginID string, viewerID int64, kindKey string, q core.KindBrowseRequest) (core.KindPage, error)
	ResolveSources       func(ctx context.Context, pluginID string, q core.SourceQuery) ([]core.SourceCandidate, error)
	ResolveSourcesResult func(ctx context.Context, pluginID string, q core.SourceQuery) (core.SourceResolveResult, error)
	// PeekUsenetCheck returns a cached shared Usenet verdict by keyless NZB URL
	// before admitting a background check against the plugin's daily allowance.
	PeekUsenetCheck func(q core.SourceQuery) (core.SourceCandidate, bool)
	// AdmitBackgroundUsenetCheck admits one background usenet_check candidate
	// check against the plugin's daily allowance in availability_budget.
	AdmitBackgroundUsenetCheck func(ctx context.Context, pluginID string) (outCtx context.Context, refund func(), resetAt time.Time, err error)
	CachePut                   func(ctx context.Context, pluginID, libraryKey, name string, r io.Reader) (blobRef string, err error)
	CacheOpen                  func(ctx context.Context, pluginID, blobRef string) (io.ReadSeekCloser, error)
	CacheDelete                func(ctx context.Context, pluginID, blobRef string) error
	CachePartial               func(ctx context.Context, pluginID, libraryKey, key string) (*blobcache.Partial, error)
	// StreamURL mints a scoped stream ticket for one of a plugin's own kind
	// items for one viewer and returns the Ether stream URL (spec §6.5).
	StreamURL        func(ctx context.Context, pluginID string, viewerID int64, itemRef string) (string, error)
	StreamPartURL    func(ctx context.Context, pluginID string, viewerID int64, itemRef string, partIndex *int) (string, error)
	StreamURLOptions func(ctx context.Context, pluginID string, viewerID int64, itemRef string, opts StreamURLOptions) (string, error)
	// ResolveBadgeTitles maps (type, tmdb, imdb) refs to local canonical rating keys (#594).
	ResolveBadgeTitles func(ctx context.Context, refs []BadgeTitleRef) ([]string, error)
	// AIComplete runs one completion on the owner's configured language model (#669).
	// Returns ErrNotConfigured when no model is enabled/configured.
	AIComplete func(ctx context.Context, req AICompleteBackendRequest) (string, error)
	// MediaProbe probes container metadata for a PlayableSource (#712).
	MediaProbe func(ctx context.Context, pluginID string, src core.PlayableSource) (core.MediaProbeResult, error)
}

// Deps are the plugin host's explicit dependencies.
type Deps struct {
	Busy              func() bool
	ReadRuntimeConfig func() config.Plugins
	SaveRuntimeConfig func(string, config.PluginRuntimeLimits) error
	ImportStylePack   func(context.Context, string, map[string][]byte) error
	OnAirContent      *onair.PluginContentCache
	DB                *sql.DB
	Sealer            config.FieldSealer
	Registry          *extend.Registry
	Tools             *tools.Catalogue
	Notifications     *notify.Service
	// EventBus, EventRegistry, and FlowActions wire structured plugin lifecycle
	// events and Flows integration (F6): plugins publish schema-validated events
	// to EventBus, register event schemas in EventRegistry, and contribute Flow
	// actions to FlowActions.
	EventBus      *events.Bus
	EventRegistry *events.Registry
	FlowActions   flows.ActionRegistry
	Logger        *slog.Logger
	// IsLeader reports cluster leadership; nil means always leader.
	IsLeader func() bool
	// Bus broadcasts invalidate:plugins; nil disables it.
	Bus clusterbus.Bus
	// FetchClient fetches manifests and artifacts; nil builds one from
	// safefetch and egress CapPlugins. Tests inject an httptest client.
	FetchClient *http.Client
	// CheckPackage runs the conformance kit during install previews.
	CheckPackage func(context.Context, []byte, map[string][]byte) error
	Backends     Backends
	Now          func() time.Time
}

// Manager is the plugin host. It is safe for concurrent use.
type Manager struct {
	replaceMu          sync.Mutex
	replaceLibs        map[string]bool
	archiveDirectories archiveDirectoryCache
	hashMu             sync.Mutex
	hashJobs           map[string]*folderHashWork
	hashSequence       uint64
	remoteHashMu       sync.Mutex
	remoteHashes       map[string]*remoteHashWork
	// relay holds the add-on settings pages open in the console.
	relay        *addonRelay
	relayOnce    sync.Once
	iconRaster   *artwork.SVGRasterizer
	store        *storage
	deps         Deps
	log          *slog.Logger
	now          func() time.Time
	client       *http.Client
	fetchHosts   *fetchHostLimiter
	mu           sync.Mutex
	sidecarState SidecarState
	catalogue    Catalogue
	fatories     map[string]RuntimeFactory
	busOrigin    string
	runtimes     map[string]Runtime
	installHooks []func(ctx context.Context, r Record)
	removeHooks  []func(context.Context, string)
	syncHooks    []func(ctx context.Context, r Record)
	eventSubs    map[string]map[string]Record
	feedLogs     sync.Map // feed key -> *faillog.Tracker
	notifyLocks  sync.Map // plugin id -> *sync.Mutex
	// schedule bookkeeping: "plugin|call" -> next run, in-flight call, and continuation job
	nextRun          map[string]time.Time
	scheduleInFlight map[string]*scheduleFlight
	scheduleJobs     map[string]*actionJob
	scheduleWG       sync.WaitGroup
	// rate-limit bookkeeping for host log, notify, custom event publish, usenet_check, and ai.complete
	notifyTimes       map[string][]time.Time
	logTimes          map[string][]time.Time
	eventPublishTimes map[string][]time.Time
	eventPublishLimit int
	usenetCheckTimes  map[string][]time.Time
	usenetCheckLimit  int
	aiCompleteTimes   map[string][]time.Time
	aiCompleteLimit   int
	mediaProbeTimes   map[string][]time.Time
	mediaProbeLimit   int
	aiSearchTimes     map[string][]time.Time
	aiSearchLimit     int
	aiSearchCacheMu   sync.Mutex
	aiSearchCache     map[string]map[string]aiSearchCacheEntry
	aiSearchInflight  map[string]*aiSearchFlight
	aiSearchTTL       time.Duration
	// eventBridgeMu guards the single events.Bus bridge subscription for plugins.
	eventBridgeMu  sync.Mutex
	eventBridgeSub *events.Subscription
	// previewMu caches recent verified package downloads and CheckPackage
	// verdicts so PreviewInstall -> Install does not re-download remote
	// packages or re-run the conformance kit twice back-to-back.
	previewMu      sync.Mutex
	checkedDigests map[string]time.Time
	sourceCache    map[string]cachedSourceEntry
	// healthMu serializes health read-modify-writes so concurrent calls
	// (resolves on the hot path, scheduled jobs) never lose an update.
	healthMu sync.Mutex
	// failing caches which plugins last recorded an error, so a successful
	// call clears it without a health read on every call (nil = not loaded).
	failing map[string]bool
	// degradedSince tracks when a plugin entered an error/attention state
	// (guarded by healthMu) so recovery reports accurate downtimeMs.
	degradedSince map[string]time.Time
	// rankingSkipping caches whether a plugin's ranking step is currently
	// being left out (Health.RankingSkipped > 0), so a resolve whose
	// plugins all ranked in time writes nothing (nil = not loaded).
	rankingSkipping     map[string]bool
	pickMu              sync.Mutex
	picks               map[string]subtitlePick
	adapterViewerMu     sync.Mutex
	adapterViewerTokens map[string]adapterViewerGrant
	// warming holds plugin ids whose runtime is loading in the background
	// (guarded by mu), so the hot path never waits for a load.
	warming map[string]bool
	// jobs holds bounded "plugin|path" action-job snapshots.
	jobs         sync.Map
	jobsMu       sync.Mutex
	runtimeEpoch map[string]uint64 // guarded by mu; rejects stale in-flight loads
	// shadow mirrors each plugin's shadow flag (guarded by mu) so the play
	// path can skip shadow plugins without a database read.
	shadow map[string]bool
	// blockedLocal records up to maxBlockedLocalHosts distinct private hosts
	// blocked per plugin (guarded by mu) so the console can offer a one-click allow.
	blockedLocal map[string][]string
	// blockedOperator records up to maxBlockedOperatorHosts distinct hosts
	// blocked for plugins declaring "operator" in permissions.network.
	blockedOperator map[string][]string
	// posterBadgeSets caches active poster badge sets per plugin (guarded by mu).
	posterBadgesLoaded bool
	posterBadgeSets    map[string][]ActivePosterBadgeSet
	badgeQuotaTitles   int64
	// liveFeeds caches active live event feeds per plugin (guarded by mu).
	liveFeedsLoaded bool
	liveFeeds       map[string][]ActiveLiveFeed
}

func (m *Manager) notificationLock(id string) *sync.Mutex {
	lock, _ := m.notifyLocks.LoadOrStore(id, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

// New builds the host and re-registers descriptors for enabled plugins.
func New(deps Deps) (*Manager, error) {
	if deps.OnAirContent == nil {
		deps.OnAirContent = onair.NewPluginContentCache()
	}
	if deps.DB == nil {
		return nil, fmt.Errorf("plugins: a database is required")
	}
	now := deps.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}
	client := deps.FetchClient
	if client == nil {
		client = (&safefetch.Guard{}).Client(egress.Client(egress.CapPlugins, 30*time.Second))
	}
	m := &Manager{
		busOrigin:  newEventID(),
		iconRaster: artwork.NewSVGRasterizer(),
		store:      &storage{db: deps.DB, sealer: deps.Sealer, now: now}, deps: deps,
		log: log, now: now, client: client,
		fetchHosts: newFetchHostLimiter(DefaultMaxFetchPerHost),
		fatories:   map[string]RuntimeFactory{}, runtimes: map[string]Runtime{}, warming: map[string]bool{}, shadow: map[string]bool{},
		eventSubs:         map[string]map[string]Record{},
		nextRun:           map[string]time.Time{},
		scheduleInFlight:  map[string]*scheduleFlight{},
		scheduleJobs:      map[string]*actionJob{},
		notifyTimes:       map[string][]time.Time{},
		logTimes:          map[string][]time.Time{},
		eventPublishTimes: map[string][]time.Time{},
		usenetCheckTimes:  map[string][]time.Time{},
		aiCompleteTimes:   map[string][]time.Time{},
		mediaProbeTimes:   map[string][]time.Time{},
		aiSearchTimes:     map[string][]time.Time{},
		aiSearchCache:     map[string]map[string]aiSearchCacheEntry{},
		aiSearchInflight:  map[string]*aiSearchFlight{},
	}
	return m, nil
}

// Start re-registers descriptors and subscribes to cluster invalidations.
// Run starts the health, scheduler and outbox loops.
func (m *Manager) Start(ctx context.Context) error {
	recs, err := m.store.list(ctx)
	if err != nil {
		return err
	}
	for _, r := range recs {
		m.syncDescriptors(ctx, r)
	}
	m.mu.Lock()
	m.posterBadgesLoaded = true
	m.mu.Unlock()
	return nil
}

// OnSync registers a callback notified whenever a plugin's enabled state or
// manifest changes (install, update, rollback, enable, disable, auto-disable,
// or remove). Any already-installed plugins are synced immediately.
func (m *Manager) OnSync(fn func(ctx context.Context, r Record)) {
	if fn == nil {
		return
	}
	m.mu.Lock()
	m.syncHooks = append(m.syncHooks, fn)
	m.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if recs, err := m.store.list(ctx); err == nil {
		for _, r := range recs {
			fn(ctx, r)
		}
	}
}

// RecordFailure reports an adapter-observed failure (such as an HTTP 5xx or
// malformed payload) into the plugin's health record and failure policy.
func (m *Manager) RecordFailure(ctx context.Context, pluginID, reason string) {
	m.recordFailure(ctx, pluginID, reason, false)
}

// RegisterRuntime adds a runtime factory for a runtime kind ("hosted" or
// "remote"). WS8 calls it at wiring time. Safe to call after Start.
func (m *Manager) RegisterRuntime(kind string, f RuntimeFactory) {
	m.mu.Lock()
	m.fatories[kind] = f
	m.mu.Unlock()
}

// SetCatalogue attaches the signed plugin catalogue (WS12).
func (m *Manager) SetCatalogue(c Catalogue) {
	m.mu.Lock()
	m.catalogue = c
	m.mu.Unlock()
}

func (m *Manager) getCatalogue() Catalogue {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.catalogue
}

func (m *Manager) isLeader() bool {
	if m.deps.IsLeader == nil {
		return true
	}
	return m.deps.IsLeader()
}

func (m *Manager) broadcast(ctx context.Context) {
	if m.deps.Bus != nil {
		_ = m.deps.Bus.Publish(ctx, "invalidate:plugins", []byte(m.busOrigin))
	}
}

// SubscribeInvalidations re-syncs descriptors when another node changes a
// plugin. Returns an unsubscribe function (nil bus: no-op).
func (m *Manager) SubscribeInvalidations() func() {
	if m.deps.Bus == nil {
		return func() {}
	}
	return m.deps.Bus.Subscribe("invalidate:plugins", func(origin []byte) {
		if m.busOrigin != "" && string(origin) == m.busOrigin {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		recs, err := m.store.list(ctx)
		if err != nil {
			return
		}
		seen := map[string]bool{}
		for _, r := range recs {
			seen[r.ID] = true
			m.closeRuntime(r.ID)
			m.syncDescriptors(ctx, r)
		}
		m.mu.Lock()
		for _, subscribers := range m.eventSubs {
			for id := range subscribers {
				if !seen[id] {
					delete(subscribers, id)
				}
			}
		}
		for id := range m.posterBadgeSets {
			if !seen[id] {
				delete(m.posterBadgeSets, id)
			}
		}
		m.posterBadgesLoaded = true
		for id := range m.liveFeeds {
			if !seen[id] {
				delete(m.liveFeeds, id)
			}
		}
		m.liveFeedsLoaded = true
		m.mu.Unlock()
		if m.deps.Notifications != nil {
			for _, kind := range m.deps.Notifications.Kinds().List() {
				if kind.Owner != "" && kind.Owner != extend.Builtin && !seen[kind.Owner] {
					m.deps.Notifications.Kinds().RemovePlugin(kind.ID, kind.Owner)
				}
			}
		}
		reg, actions := m.flowsRegistries()
		if reg != nil {
			for _, s := range reg.List() {
				if s.Owner != "" && s.Owner != events.OwnerBuiltin && !seen[s.Owner] {
					reg.RemovePlugin(s.Owner)
				}
			}
		}
		if actions != nil {
			for _, spec := range actions.List() {
				if spec.Owner != "" && spec.Owner != events.OwnerBuiltin && !seen[spec.Owner] {
					actions.RemovePlugin(spec.Owner)
				}
			}
		}
	})
}

// List returns every installed plugin.
func (m *Manager) List(ctx context.Context) ([]Record, error) { return m.store.list(ctx) }

// Get returns one installed plugin.
func (m *Manager) Get(ctx context.Context, id string) (Record, error) {
	return m.store.record(ctx, id)
}

// HealthOf returns one plugin's health record.
func (m *Manager) HealthOf(ctx context.Context, id string) (Health, error) {
	r, err := m.store.record(ctx, id)
	if err != nil {
		return Health{}, err
	}
	h, err := m.store.health(ctx, id)
	if err != nil {
		return Health{}, err
	}
	return m.liveHealth(r, h), nil
}

// GrantsOf returns one plugin's grants.
func (m *Manager) GrantsOf(ctx context.Context, id string) (Grants, error) {
	if _, err := m.store.record(ctx, id); err != nil {
		return Grants{}, err
	}
	return m.store.grants(ctx, id)
}

// SetGrants replaces one plugin's grants. Host calls re-check grants on
// every call, so a revoke applies mid-call.
func (m *Manager) SetGrants(ctx context.Context, id string, g Grants) error {
	r, err := m.store.record(ctx, id)
	if err != nil {
		return err
	}
	if g.LocalHosts != nil {
		normalized, err := normalizeLocalHosts(g.LocalHosts)
		if err != nil {
			return &ValidationError{Fields: []FieldError{{Field: "localHosts", Message: err.Error()}}}
		}
		g.LocalHosts = normalized
	}
	if g.OperatorHosts != nil {
		normalized, err := normalizeOperatorHosts(g.OperatorHosts)
		if err != nil {
			return &ValidationError{Fields: []FieldError{{Field: "operatorHosts", Message: err.Error()}}}
		}
		g.OperatorHosts = normalized
	}
	if err := m.store.putGrants(ctx, id, g); err != nil {
		return err
	}
	m.pruneBlockedLocalHosts(id, g.LocalHosts)
	m.pruneBlockedOperatorHosts(id, g.OperatorHosts)
	m.syncPosterBadgeSets(ctx, r)
	m.syncLiveFeeds(ctx, r)
	m.syncFlowsPlugin(ctx, r)
	m.clearAISearchCache(id)
	m.closeRuntime(id)
	m.broadcast(ctx)
	return nil
}

// HostFor returns the host-services handle for one plugin (SPEC §5.1.5).
func (m *Manager) HostFor(pluginID string) Host { return &host{m: m, plugin: pluginID} }

// TextResolver resolves a plugin string for a locale, falling back to en.
func (m *Manager) TextResolver() extend.TextResolver {
	return func(plugin, key, locale string) (string, bool) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return m.ResolveText(ctx, plugin, key, locale)
	}
}

// ResolveText looks up key in the plugin's strings for locale, then en.
func (m *Manager) ResolveText(ctx context.Context, plugin, key, locale string) (string, bool) {
	row, err := m.store.settings(ctx, plugin)
	if err != nil {
		return "", false
	}
	for _, loc := range []string{locale, strings.SplitN(locale, "-", 2)[0], "en"} {
		if loc == "" {
			continue
		}
		if s, ok := row.Strings[loc][key]; ok {
			return s, true
		}
	}
	return "", false
}

// --- runtime plumbing ---

func (m *Manager) runtimeFor(ctx context.Context, r Record) (Runtime, error) {
	rt, _, err := m.runtimeForWithEpoch(ctx, r)
	return rt, err
}

func (m *Manager) runtimeForWithEpoch(ctx context.Context, r Record) (Runtime, uint64, error) {
	m.mu.Lock()
	rt, ok := m.runtimes[r.ID]
	epoch := m.runtimeEpoch[r.ID]
	f, hasFactory := m.fatories[r.Manifest.RuntimeKind()]
	m.mu.Unlock()
	if ok {
		return rt, epoch, nil
	}
	if !hasFactory {
		return nil, 0, ErrNoRuntime
	}
	inst := Installed{
		RuntimeLimits: m.grantedRuntimeLimits(r.Manifest), Busy: m.deps.Busy,
		ID: r.ID, Manifest: r.Manifest, SourceURL: r.SourceURL, Host: m.HostFor(r.ID),
		Files: func(path string) ([]byte, bool) {
			fctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			return m.store.file(fctx, r.ID, r.Version, path)
		},
	}
	rt, err := f(inst)
	if err != nil {
		return nil, 0, err
	}
	m.mu.Lock()
	if epoch != m.runtimeEpoch[r.ID] {
		m.mu.Unlock()
		_ = rt.Close()
		return nil, 0, ErrUnavailable
	}
	if existing, ok := m.runtimes[r.ID]; ok {
		m.mu.Unlock()
		_ = rt.Close()
		return existing, epoch, nil
	}
	m.runtimes[r.ID] = rt
	m.mu.Unlock()
	m.deliverSettings(ctx, r.ID, rt)
	return rt, epoch, nil
}

// deliverSettings sends a newly opened runtime the plugin's stored
// settings (secrets included; the plugin owns them) through
// /ether/configure, so a plugin that keeps its configuration in memory has
// it after a start, restart or update. Failures are logged, not fatal: the
// plugin can still read settings.get per request.
func (m *Manager) deliverSettings(ctx context.Context, pluginID string, rt Runtime) {
	if ctx.Err() != nil {
		return
	}
	settings, err := m.mergedSettings(ctx, pluginID)
	if err != nil || len(settings) == 0 {
		return
	}
	body, err := json.Marshal(settings)
	if err != nil {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, DeadlineFor("/ether/configure"))
	defer cancel()
	if _, err := rt.Call(cctx, Request{PluginID: pluginID, Method: "POST", Path: "/ether/configure", Body: body}); err != nil {
		m.log.Warn("plugins: deliver settings to new runtime", "plugin", pluginID, "err", err)
	}
}

// PluginSettings returns a plugin's stored settings with its own secrets,
// for delivery to that plugin only (remote sidecars read it from the
// authenticated plugin-host API after a restart or reconnect).
func (m *Manager) PluginSettings(ctx context.Context, pluginID string) (map[string]any, error) {
	return m.mergedSettings(ctx, pluginID)
}

// ErrStreamUnsupported indicates that a runtime cannot relay file bodies.
var ErrStreamUnsupported = errors.New("plugins: runtime does not support streaming")

type streamRuntime interface {
	CallStream(context.Context, Request) (Response, io.ReadCloser, error)
}

type streamBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *streamBody) Close() error { err := b.ReadCloser.Close(); b.cancel(); return err }

// CallStream returns a streaming response. The call deadline remains active
// until the body is closed; callers must close it on every path.
func (m *Manager) CallStream(ctx context.Context, pluginID string, req Request) (Response, io.ReadCloser, error) {
	if req.Path == "/ether/subtitles/download" {
		return Response{Status: http.StatusForbidden}, nil, ErrPermissionDenied
	}
	r, err := m.store.record(ctx, pluginID)
	if err != nil {
		return Response{}, nil, err
	}
	if !r.Enabled {
		return Response{}, nil, ErrDisabled
	}
	rt, err := m.runtimeFor(ctx, r)
	if err != nil {
		return Response{}, nil, err
	}
	s, ok := rt.(streamRuntime)
	if !ok {
		return Response{}, nil, ErrStreamUnsupported
	}
	req.Headers = cloneHeaders(req.Headers)
	req.PluginID = pluginID

	deadline := DeadlineFor(req.Path)
	vctx, releaseViewer := m.bindAdapterViewer(ctx, pluginID, &req, deadline)
	vctx = withCallSearchQuery(withCallPath(vctx, req.Path), extractCallSearchQuery(req))
	ctx, cancel := context.WithTimeout(vctx, deadline)
	cleanup := func() {
		releaseViewer()
		cancel()
	}
	resp, body, err := s.CallStream(ctx, req)
	if err != nil {
		cleanup()
		return Response{}, nil, err
	}
	if body == nil {
		cleanup()
		return Response{}, nil, errors.New("plugins: streaming runtime returned no body")
	}
	return resp, &streamBody{ReadCloser: body, cancel: cleanup}, nil
}

func (m *Manager) runtimeEpochMatches(id string, epoch uint64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.runtimeEpoch[id] == epoch
}

func (m *Manager) closeRuntime(id string) {
	m.mu.Lock()
	if m.runtimeEpoch == nil {
		m.runtimeEpoch = map[string]uint64{}
	}
	m.runtimeEpoch[id]++
	rt := m.runtimes[id]
	addon := m.runtimes[id+addonRuntimeSuffix]
	delete(m.runtimes, id)
	delete(m.runtimes, id+addonRuntimeSuffix)
	m.mu.Unlock()
	m.cancelActionJobs(id)
	m.cancelRemoteHashes(id)
	for _, r := range []Runtime{rt, addon} {
		if r != nil {
			_ = r.Close()
		}
	}
}

func (m *Manager) closeRuntimeAtEpoch(id string, epoch uint64) bool {
	m.mu.Lock()
	if m.runtimeEpoch[id] != epoch {
		m.mu.Unlock()
		return false
	}
	if m.runtimeEpoch == nil {
		m.runtimeEpoch = map[string]uint64{}
	}
	m.runtimeEpoch[id]++
	rt := m.runtimes[id]
	addon := m.runtimes[id+addonRuntimeSuffix]
	delete(m.runtimes, id)
	delete(m.runtimes, id+addonRuntimeSuffix)
	m.mu.Unlock()
	m.cancelActionJobs(id)
	m.cancelRemoteHashes(id)
	for _, r := range []Runtime{rt, addon} {
		if r != nil {
			_ = r.Close()
		}
	}
	return true
}

func isCallTimeout(err error, callCtx context.Context) bool {
	var we *wasm.Error
	if errors.As(err, &we) && we.Status == wasm.StatusTimeout {
		return true
	}
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(callCtx.Err(), context.DeadlineExceeded)
}

func stremioDeadline(path string) time.Duration {
	return wasm.StremioDeadline(path)
}

// DeadlineFor returns the per-route deadline from SPEC §5.1.2.
func DeadlineFor(path string) time.Duration {
	return wasm.DeadlineFor(path)
}

// callOnce makes one call to a plugin through its runtime, records it in the
// inspector ring (secrets redacted) and feeds failures into health.
// Adapters (WS9) call plugins only through here.
func (m *Manager) callOnce(ctx context.Context, pluginID string, req Request) (Response, error) {
	req.Headers = cloneHeaders(req.Headers)
	if req.Path == "/ether/subtitles/download" {
		var body struct {
			ID string `json:"id"`
		}
		if req.Method != http.MethodPost || json.Unmarshal(req.Body, &body) != nil || !m.consumeSubtitlePick(req.Headers["X-Ether-Pick"], pluginID, req.Headers["X-Ether-Title"], body.ID) {
			return Response{Status: http.StatusForbidden}, ErrPermissionDenied
		}
	}
	r, err := m.store.record(ctx, pluginID)
	if err != nil {
		return Response{}, err
	}
	if !r.Enabled {
		return Response{}, ErrDisabled
	}
	req.PluginID = pluginID
	if req.Locale == "" {
		for _, key := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
			if value := os.Getenv(key); value != "" {
				req.Locale = strings.ReplaceAll(strings.Split(value, ".")[0], "_", "-")
				break
			}
		}
	}
	start := m.now()
	grantedDeadline := m.runtimeCeilings(pluginID).deadline
	if l := m.grantedRuntimeLimits(r.Manifest); l != nil {
		if d, err := time.ParseDuration(l.Deadline); err == nil && d > 0 {
			grantedDeadline = d
		}
	}
	deadline := wasm.EffectiveDeadline(req.Path, grantedDeadline, r.Manifest.SpeaksStremio())
	searchQuery := extractCallSearchQuery(req)
	if isClientRequestPath(req.Path) && isActiveSearchRoute(r.Manifest, req.Path, searchQuery) && m.aiSearchGrantedRecord(ctx, r) {
		target := AISearchRouteDeadline
		if grantedDeadline > 0 && grantedDeadline < target {
			target = grantedDeadline
		}
		if target > deadline {
			deadline = target
			req.AISearch = true
		}
	}
	req.Deadline = deadline
	req.Headers = cloneHeaders(req.Headers)
	vctx, releaseViewer := m.bindAdapterViewer(ctx, pluginID, &req, deadline)
	defer releaseViewer()
	req.Headers["X-Ether-Deadline"] = start.Add(deadline).UTC().Format(time.RFC3339)
	if req.Locale != "" {
		req.Headers["X-Ether-Locale"] = req.Locale
	}
	rt, epoch, err := m.runtimeForWithEpoch(ctx, r)
	if err != nil {
		message := m.store.redactKnown(ctx, pluginID, err.Error())
		m.recordFailure(ctx, pluginID, "runtime unavailable: "+message, false)
		if message != err.Error() {
			err = errors.New(message)
		}
		return Response{}, err
	}
	// Hosted playback resolves bound waiting and execution separately inside
	// wasm.Plugin.CallDetailed (up to deadline to acquire an instance, then a
	// full deadline to execute once acquired), so give the outer call context
	// room for both phases rather than cancelling mid-execution after a queue
	// wait.
	callTimeout := deadline
	if _, hosted := rt.(*hostedRuntime); hosted && wasm.ResolvePoolPath(req.Path) {
		callTimeout = 2 * deadline
	}
	cctx, cancel := context.WithTimeout(withCallSearchQuery(withCallPath(vctx, req.Path), searchQuery), callTimeout)
	defer cancel()
	resp, err := rt.Call(cctx, req)
	ms := m.now().Sub(start).Milliseconds()
	rec := CallRecord{Plugin: pluginID, Route: req.Path, Method: req.Method, Status: resp.Status, MS: ms,
		Request: m.store.redactKnown(ctx, pluginID, redactJSON(req.Body)), Response: m.store.redactKnown(ctx, pluginID, redactJSON(resp.Body))}
	if req.Path == "/ether/notify" || req.Path == "/ether/configure" || strings.HasPrefix(req.Path, "/subtitles/") || req.Path == "/ether/subtitles/download" {
		rec.Request = ""
		rec.Response = ""
	}
	if err != nil && ctx.Err() != nil {
		// The caller gave up (a search that already had its answer, a
		// viewer who left): not the plugin's failure.
		rec.Error = "cancelled by Ether"
		m.store.addCall(ctx, rec)
		return Response{}, ctx.Err()
	}
	if err != nil && errors.Is(err, wasm.ErrRequestEncode) {
		// Ether could not encode the request, so the guest never ran: keep
		// the runtime and its jobs intact and record no health failure.
		rec.Error = m.store.redactKnown(ctx, pluginID, err.Error())
		m.store.addCall(ctx, rec)
		return Response{}, fmt.Errorf("%w: %w", ErrUnsupportedBody, err)
	}
	if err != nil && errors.Is(err, wasm.ErrPoolBusy) {
		// Waiting for a free instance is pool contention, not a plugin crash:
		// keep the runtime and any running jobs intact and do not record a
		// health failure.
		rec.Error = m.store.redactKnown(ctx, pluginID, err.Error())
		m.store.addCall(ctx, rec)
		return Response{}, fmt.Errorf("%w: %w", ErrBusy, err)
	}
	if err != nil && (errors.Is(err, wasm.ErrClosed) || !m.runtimeEpochMatches(pluginID, epoch)) {
		// Ether closed or replaced this runtime while the call was in flight
		// (a settings change, update, or another call's crash). Leave any
		// replacement runtime intact and do not count this collateral error
		// as a separate plugin crash.
		if req.Path == "/ether/notify" || req.Path == "/ether/configure" {
			err = errors.New("notification delivery failed")
		}
		rec.Error = m.store.redactKnown(ctx, pluginID, err.Error())
		m.store.addCall(ctx, rec)
		if rec.Error != err.Error() {
			err = errors.New(rec.Error)
		}
		return Response{}, err
	}
	if err != nil && isCallTimeout(err, cctx) {
		// A single timed-out call cancels only that call (and its instance
		// when torn down by the runtime), not the pool or other in-flight
		// work. Record it as slow rather than as a runtime crash.
		if req.Path == "/ether/notify" || req.Path == "/ether/configure" {
			err = errors.New("notification delivery failed")
		}
		rec.Error = m.store.redactKnown(ctx, pluginID, err.Error())
		m.store.addCall(ctx, rec)
		m.recordTimeout(ctx, pluginID, rec.Error)
		if rec.Error != err.Error() {
			err = errors.New(rec.Error)
		}
		return Response{}, err
	}
	if err != nil {
		if req.Path == "/ether/notify" || req.Path == "/ether/configure" {
			err = errors.New("notification delivery failed")
		}
		rec.Error = m.store.redactKnown(ctx, pluginID, err.Error())
		m.store.addCall(ctx, rec)
		// Crash policy: drop the runtime for this epoch so the next call
		// restarts it without disturbing a newer runtime.
		m.closeRuntimeAtEpoch(pluginID, epoch)
		m.recordFailure(ctx, pluginID, rec.Error, true)
		if rec.Error != err.Error() {
			err = errors.New(rec.Error)
		}
		return Response{}, err
	}
	if len(rec.Request) > 64*1024 {
		rec.Request = ""
	}
	if len(rec.Response) > 64*1024 {
		rec.Response = ""
	}
	if resp.Status == 0 {
		resp.Status = http.StatusOK
		rec.Status = resp.Status
	}
	m.store.addCall(ctx, rec)
	var pending PendingReply
	_ = json.Unmarshal(resp.Body, &pending)
	if !pending.Pending && ms > deadline.Milliseconds()/2 && ms > 1000 {
		m.RecordSlow(ctx, pluginID, 1)
	}
	if resp.Status < 400 {
		m.recoverHealth(ctx, pluginID)
	}
	return resp, nil
}

// recoverHealth clears a recorded error once the plugin answers again.
// Without it an add-on with no runtime health check (Stremio add-ons) kept
// showing a problem forever after one bad response.
func (m *Manager) recoverHealth(ctx context.Context, pluginID string) {
	m.healthMu.Lock()
	if m.failing == nil {
		m.failing = map[string]bool{}
	}
	failing, known := m.failing[pluginID]
	m.healthMu.Unlock()
	if known && !failing {
		return
	}
	if !known {
		h, err := m.store.health(ctx, pluginID)
		if err != nil {
			return
		}
		m.healthMu.Lock()
		m.failing[pluginID] = h.Status == "error"
		m.healthMu.Unlock()
		if h.Status != "error" {
			return
		}
	}
	m.recordOK(ctx, pluginID)
}

// redactJSON masks values under secret-looking keys before anything is
// stored in the inspector ring.
func redactJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	v = redactValue(v)
	out, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(out)
}

func redactValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			lk := strings.ToLower(k)
			if strings.Contains(lk, "secret") || strings.Contains(lk, "token") || strings.Contains(lk, "password") || strings.Contains(lk, "api_key") || strings.Contains(lk, "apikey") || lk == "key" || logredact.Sensitive(k) {
				t[k] = "Saved"
				continue
			}
			t[k] = redactValue(val)
		}
	case []any:
		for i, item := range t {
			t[i] = redactValue(item)
		}
	case string:
		// Signed URLs and free text can carry credentials in query
		// parameters or as bare JWTs.
		return logredact.String(t)
	}
	return v
}

// recordFailure applies the SPEC §5.2 failure policy: exponential backoff
// from 1s to 5 minutes, and five failures in ten minutes disables the
// plugin with a console notice.
func (m *Manager) recordFailure(ctx context.Context, pluginID, reason string, restart bool) {
	// A cancelled call (Ether shutting down or restarting the plugin, or a
	// caller that stopped waiting) says nothing about the plugin; counting it
	// switched plugins off on every restart that interrupted a sync.
	if errors.Is(ctx.Err(), context.Canceled) || strings.Contains(reason, context.Canceled.Error()) {
		return
	}
	// A timed-out call still needs its health failure persisted.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	m.healthMu.Lock()
	defer m.healthMu.Unlock()
	h, err := m.store.health(ctx, pluginID)
	if err != nil {
		return
	}
	now := m.now()
	cutoff := now.Add(-10 * time.Minute).Unix()
	kept := h.Failures[:0:0]
	for _, t := range h.Failures {
		if t >= cutoff {
			kept = append(kept, t)
		}
	}
	kept = append(kept, now.Unix())
	h.Failures = kept
	if restart {
		h.Restarts++
	}
	backoff := time.Second << min(len(kept)-1, 8)
	if backoff > 5*time.Minute {
		backoff = 5 * time.Minute
	}
	h.NextRetry = now.Add(backoff)
	h.LastError = reason
	h.Status = "error"
	h.Reason = reason
	if m.failing == nil {
		m.failing = map[string]bool{}
	}
	m.failing[pluginID] = true
	m.markDegradedSinceLocked(pluginID, now)
	_ = m.store.putHealth(ctx, pluginID, h)
	pluginName := m.pluginDisplayName(ctx, pluginID)
	autoDisabled := false
	if len(kept) >= 5 {
		// A Stremio add-on is one of the owner's sources, edited in Studio
		// or not: an outage backs it off and shows on its health, but never
		// switches it off.
		if r, err := m.store.record(ctx, pluginID); err == nil && r.Enabled && !r.Manifest.SpeaksStremio() {
			r.Enabled = false
			_ = m.store.upsert(ctx, r)
			h.Status = "disabled"
			h.Reason = "Disabled after repeated failures. Last error: " + reason
			_ = m.store.putHealth(ctx, pluginID, h)
			m.closeRuntime(pluginID)
			m.syncDescriptors(ctx, r)
			m.broadcast(ctx)
			pluginName = r.Name
			autoDisabled = true
			msg := "Plugin " + r.Name + " was disabled after repeated failures."
			m.log.Warn(msg, "plugin", pluginID, "reason", reason)
			if m.deps.Backends.Notice != nil {
				m.deps.Backends.Notice(msg)
			}
		}
	}
	m.emitBusEvent(events.EventPluginHealthDegraded, pluginID, events.PluginHealthDegradedPayload{
		PluginID:            pluginID,
		Name:                pluginName,
		Status:              h.Status,
		Restarts:            h.Restarts,
		ConsecutiveFailures: len(kept),
		LastError:           reason,
	}, true)
	if autoDisabled {
		m.emitBusEvent(events.EventPluginStateDisabled, pluginID, events.PluginStateDisabledPayload{
			PluginID: pluginID,
			Name:     pluginName,
			Reason:   "auto_disabled: " + reason,
		}, true)
	}
}

// recordTimeout records a call that exceeded its deadline as slow rather
// than as a runtime crash. The plugin stays enabled, Failures and Restarts
// are not incremented, and LastError retains the timeout reason.
func (m *Manager) recordTimeout(ctx context.Context, pluginID, reason string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	m.healthMu.Lock()
	defer m.healthMu.Unlock()
	h, err := m.store.health(ctx, pluginID)
	if err != nil {
		return
	}
	h.Slow++
	h.LastError = reason
	becameDegraded := false
	if h.Status == "ok" {
		h.Status = "attention"
		h.Reason = "Recent calls were slow."
		m.markDegradedSinceLocked(pluginID, m.now())
		becameDegraded = true
	}
	_ = m.store.putHealth(ctx, pluginID, h)
	if becameDegraded {
		m.emitBusEvent(events.EventPluginHealthDegraded, pluginID, events.PluginHealthDegradedPayload{
			PluginID:  pluginID,
			Name:      m.pluginDisplayName(ctx, pluginID),
			Status:    h.Status,
			Restarts:  h.Restarts,
			LastError: reason,
		}, false)
	}
}

// RecordSlow adds n slow calls or slow hot-path candidates to a plugin's
// health and flags it for the owner's attention. It is safe for concurrent
// use; the ranking adapters call it after a resolve.
func (m *Manager) RecordSlow(ctx context.Context, pluginID string, n int) {
	if n <= 0 {
		return
	}
	m.healthMu.Lock()
	defer m.healthMu.Unlock()
	h, err := m.store.health(ctx, pluginID)
	if err != nil {
		return
	}
	h.Slow += int64(n)
	becameDegraded := false
	if h.Status == "ok" {
		h.Status = "attention"
		h.Reason = "Recent calls were slow."
		m.markDegradedSinceLocked(pluginID, m.now())
		becameDegraded = true
	}
	_ = m.store.putHealth(ctx, pluginID, h)
	if becameDegraded {
		m.emitBusEvent(events.EventPluginHealthDegraded, pluginID, events.PluginHealthDegradedPayload{
			PluginID:  pluginID,
			Name:      m.pluginDisplayName(ctx, pluginID),
			Status:    h.Status,
			Restarts:  h.Restarts,
			LastError: h.Reason,
		}, false)
	}
}

// RankingReport is what one resolve saw of a plugin on the hot path.
type RankingReport struct {
	// Slow counts candidates and conditions that went neutral on budget.
	Slow int
	// Skipped counts ranking steps left out because the plugin ran past
	// its time budget; Ranked counts ranking steps that finished in time.
	Skipped, Ranked int
}

// rankingSkippedReason is the health reason while a plugin's ranking step
// is being left out. It says "slow" so the console's headline reads Slow;
// the drawer explains it from Health.RankingSkipped.
const rankingSkippedReason = "Its ranking step was too slow, so releases were ranked without it."

// sidecarWaitingReason is the health reason while a plugin that runs as its
// own program has no sidecar connected.
const sidecarWaitingReason = "It runs as its own program, and that program is not connected."

// SidecarState reports whether an installed plugin is served by a sidecar
// and whether one is connected now. The app wires it from the remote
// runtime; nil means no plugin needs a sidecar.
type SidecarState func(Record) (needsSidecar, connected bool)

// SetSidecarState installs the live sidecar check used for console health.
func (m *Manager) SetSidecarState(f SidecarState) {
	m.mu.Lock()
	m.sidecarState = f
	m.mu.Unlock()
}

// liveHealth overlays the live sidecar state on a stored health record: an
// enabled plugin whose sidecar is not connected needs attention however its
// last check went, so the console never calls it healthy.
func (m *Manager) sidecarWaiting(r Record) bool {
	m.mu.Lock()
	f := m.sidecarState
	m.mu.Unlock()
	if f == nil || !r.Enabled {
		return false
	}
	needs, connected := f(r)
	return needs && !connected
}

// PoolStats returns a live instance-pool snapshot for a running hosted plugin.
func (m *Manager) PoolStats(pluginID string) (wasm.PoolStats, bool) {
	m.mu.Lock()
	rt := m.runtimes[pluginID]
	m.mu.Unlock()
	type poolStatsProvider interface {
		PoolStats() wasm.PoolStats
	}
	if p, ok := rt.(poolStatsProvider); ok {
		return p.PoolStats(), true
	}
	return wasm.PoolStats{}, false
}

func (m *Manager) liveHealth(r Record, h Health) Health {
	if m.sidecarWaiting(r) {
		h.Status, h.Reason, h.Sidecar = "attention", sidecarWaitingReason, "waiting"
	}
	if snap, ok := m.PoolStats(r.ID); ok && snap.Size > 0 {
		ph := &PoolHealth{
			Size:        snap.Size,
			InUse:       snap.InUse,
			Idle:        snap.Idle,
			Building:    snap.Building,
			Waiting:     snap.Waiting,
			Exhaustions: snap.Exhaustions,
		}
		if len(snap.Holders) > 0 {
			ph.Holders = make([]PoolHolderHealth, len(snap.Holders))
			for i, holder := range snap.Holders {
				ph.Holders[i] = PoolHolderHealth{
					Route:  holder.Route,
					HeldMS: holder.Held.Milliseconds(),
				}
			}
		}
		h.Pool = ph
	}
	return h
}

// RecordRanking records a resolve's hot-path outcome for one plugin. A
// ranking step left out for running past its budget flags the plugin for
// the owner until one of its rankings finishes in time again; the
// periodic health check does not clear it, since the plugin's own health
// answer says nothing about its ranking. A resolve where the plugin kept
// to time and was not being skipped writes nothing. Safe for concurrent
// use; the ranking adapters call it when a resolve ends.
func (m *Manager) RecordRanking(ctx context.Context, pluginID string, r RankingReport) {
	if r.Slow <= 0 && r.Skipped <= 0 && r.Ranked <= 0 {
		return
	}
	m.healthMu.Lock()
	defer m.healthMu.Unlock()
	if m.rankingSkipping == nil {
		m.rankingSkipping = map[string]bool{}
	}
	if skipping, known := m.rankingSkipping[pluginID]; known && !skipping && r.Slow <= 0 && r.Skipped <= 0 {
		return
	}
	h, err := m.store.health(ctx, pluginID)
	if err != nil {
		return
	}
	before := h
	h.Slow += int64(r.Slow)
	switch {
	case r.Skipped > 0:
		h.RankingSkipped += int64(r.Skipped)
		h.RankingSkippedAt = m.now()
		if h.Status == "ok" || h.Status == "attention" {
			h.Status, h.Reason = "attention", rankingSkippedReason
		}
	case r.Ranked > 0 && h.RankingSkipped > 0:
		// It kept to time again: its ranking counts and the flag clears.
		h.RankingSkipped = 0
		if h.Status == "attention" && h.Reason == rankingSkippedReason {
			h.Status, h.Reason = "ok", ""
		}
	case r.Slow > 0 && h.Status == "ok":
		h.Status, h.Reason = "attention", "Recent calls were slow."
	}
	m.rankingSkipping[pluginID] = h.RankingSkipped > 0
	if h.Slow == before.Slow && h.RankingSkipped == before.RankingSkipped && h.Status == before.Status {
		return
	}
	_ = m.store.putHealth(ctx, pluginID, h)
}

// okUnlessSkipping marks a plugin healthy, unless its ranking step is
// being left out: only a ranking that finishes in time clears that.
func okUnlessSkipping(h *Health) {
	h.Status, h.Reason = "ok", ""
	if h.RankingSkipped > 0 {
		h.Status, h.Reason = "attention", rankingSkippedReason
	}
}

func (m *Manager) recordOK(ctx context.Context, pluginID string) {
	m.healthMu.Lock()
	defer m.healthMu.Unlock()
	h, err := m.store.health(ctx, pluginID)
	if err != nil {
		return
	}
	wasDegraded := h.Status == "error" || h.Status == "attention"
	now := m.now()
	okUnlessSkipping(&h)
	h.LastOK = now
	_ = m.store.putHealth(ctx, pluginID, h)
	if m.failing == nil {
		m.failing = map[string]bool{}
	}
	m.failing[pluginID] = false
	if wasDegraded && h.Status == "ok" {
		downtimeMS := m.clearDegradedSinceLocked(pluginID, now)
		m.emitBusEvent(events.EventPluginHealthRecovered, pluginID, events.PluginHealthRecoveredPayload{
			PluginID:   pluginID,
			Name:       m.pluginDisplayName(ctx, pluginID),
			DowntimeMS: downtimeMS,
		}, false)
	}
}

// Revoke disables an installed plugin that was marked revoked in the
// signed catalogue, records the reason in its health, and posts a console
// notice (SPEC WS12, §10). It returns true when an installed plugin was
// newly disabled or updated with the revocation reason.
func (m *Manager) Revoke(ctx context.Context, id, reason string) (bool, error) {
	r, err := m.store.record(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "This plugin was revoked by the catalogue."
	}
	healthReason := "Revoked: " + reason
	h, _ := m.store.health(ctx, id)
	if !r.Enabled && h.Status == "disabled" && h.Reason == healthReason {
		return false, nil
	}
	r.Enabled = false
	if err := m.store.upsert(ctx, r); err != nil {
		return false, err
	}
	h.Status = "disabled"
	h.Reason = healthReason
	h.LastError = reason
	_ = m.store.putHealth(ctx, id, h)
	m.closeRuntime(id)
	m.syncDescriptors(ctx, r)
	m.broadcast(ctx)
	msg := "Plugin " + r.Name + " was disabled because it was revoked: " + reason
	m.log.Warn(msg, "plugin", id, "reason", reason)
	if m.deps.Backends.Notice != nil {
		m.deps.Backends.Notice(msg)
	}
	m.emitBusEvent(events.EventPluginStateDisabled, id, events.PluginStateDisabledPayload{
		PluginID: id,
		Name:     r.Name,
		Reason:   "revoked: " + reason,
	}, true)
	return true, nil
}

// SetEnabled enables or disables a plugin and refreshes its descriptors.
func (m *Manager) SetEnabled(ctx context.Context, id string, enabled bool) error {
	r, err := m.store.record(ctx, id)
	if err != nil {
		return err
	}
	if enabled {
		if cat := m.getCatalogue(); cat != nil {
			if _, cerr := cat.Resolve(ctx, id, ""); errors.Is(cerr, ErrRevoked) {
				return cerr
			}
		}
	}
	r.Enabled = enabled
	if err := m.store.upsert(ctx, r); err != nil {
		return err
	}
	if enabled {
		h, _ := m.store.health(ctx, id)
		h.Failures = nil
		if h.Status == "disabled" {
			h.Status, h.Reason = "ok", ""
		}
		_ = m.store.putHealth(ctx, id, h)
	} else {
		m.closeRuntime(id)
	}
	m.syncDescriptors(ctx, r)
	m.broadcast(ctx)
	if enabled {
		m.emitBusEvent(events.EventPluginStateEnabled, id, events.PluginStateEnabledPayload{
			PluginID: id,
			Name:     r.Name,
			Shadow:   r.Shadow,
		}, false)
	} else {
		m.emitBusEvent(events.EventPluginStateDisabled, id, events.PluginStateDisabledPayload{
			PluginID: id,
			Name:     r.Name,
			Reason:   "owner",
		}, false)
	}
	return nil
}

// SetRoles switches an installed add-on's roles on or off: off lists the
// capabilities it should stop playing (its catalogs, streams, subtitles,
// Live TV channels…). Only roles its manifest offers can be named.
func (m *Manager) SetRoles(ctx context.Context, id string, off []Capability) (Record, error) {
	r, err := m.store.record(ctx, id)
	if err != nil {
		return Record{}, err
	}
	offered := map[Capability]bool{}
	for _, c := range r.Manifest.AddonRoles() {
		offered[c] = true
	}
	seen := map[Capability]bool{}
	var clean []Capability
	// Roles the add-on does not offer yet but was set to keep off stay off.
	for _, c := range r.RolesOff {
		if !offered[c] && !seen[c] {
			seen[c] = true
			clean = append(clean, c)
		}
	}
	for _, c := range off {
		if !offered[c] {
			return Record{}, &ValidationError{[]FieldError{{"roles", fmt.Sprintf("%s is not a role this add-on offers", c)}}}
		}
		if !seen[c] {
			seen[c] = true
			clean = append(clean, c)
		}
	}
	if err := m.store.setRolesOff(ctx, id, clean); err != nil {
		return Record{}, err
	}
	saved, err := m.store.record(ctx, id)
	if err != nil {
		return Record{}, err
	}
	m.closeRuntime(id)
	m.syncDescriptors(ctx, saved)
	m.broadcast(ctx)
	return saved, nil
}

// SetShadow turns shadow mode on or off.
func (m *Manager) SetShadow(ctx context.Context, id string, shadow bool) error {
	r, err := m.store.record(ctx, id)
	if err != nil {
		return err
	}
	r.Shadow = shadow
	if err := m.store.upsert(ctx, r); err != nil {
		return err
	}
	m.mu.Lock()
	m.shadow[id] = shadow
	m.mu.Unlock()
	m.syncPosterBadgeSets(ctx, r)
	m.syncLiveFeeds(ctx, r)
	m.broadcast(ctx)
	return nil
}

// Remove uninstalls a plugin: it offers undo first (when undoFirst, every
// live journal inverse is replayed), then purges KV, secrets, grants,
// artifacts, logs and its registry entries.
func (m *Manager) Remove(ctx context.Context, id string, undoFirst bool) error {
	r, err := m.store.record(ctx, id)
	if err != nil {
		return err
	}
	if undoFirst {
		if _, err := m.UndoAll(ctx, id); err != nil && !errors.Is(err, ErrUnavailable) {
			m.log.Warn("plugin remove: undo had problems", "plugin", id, "err", err)
		}
	}
	// Revoke host calls before canceling workers so an in-flight caller
	// cannot register new work between cancellation and the final purge.
	r.Enabled = false
	if err := m.store.upsert(ctx, r); err != nil {
		return err
	}
	m.closeRuntime(id)
	m.syncDescriptors(ctx, r)
	m.indexEventSubscriptions(Record{ID: id})
	if m.deps.Registry != nil {
		m.deps.Registry.RemoveOwner(id)
	}
	if m.deps.Tools != nil {
		_ = m.deps.Tools.ReplaceOwner(id, nil)
	}
	if m.deps.Notifications != nil {
		m.deps.Notifications.Kinds().RemovePlugin(extend.PluginID(id, "notify"), id)
	}
	reg, actions := m.flowsRegistries()
	if reg != nil {
		reg.RemovePlugin(id)
	}
	if actions != nil {
		actions.RemovePlugin(id)
	}
	m.jobs.Range(func(key, value any) bool {
		if strings.HasPrefix(key.(string), id+"|") {
			m.jobs.Delete(key)
		}
		return true
	})
	if err := m.store.purge(ctx, id); err != nil {
		return err
	}
	m.clearBlockedLocalHosts(id)
	m.clearBlockedOperatorHosts(id)
	m.clearAISearchCache(id)
	m.mu.Lock()
	delete(m.liveFeeds, id)
	delete(m.eventPublishTimes, id)
	delete(m.usenetCheckTimes, id)
	delete(m.aiCompleteTimes, id)
	delete(m.mediaProbeTimes, id)
	for key := range m.aiSearchTimes {
		if strings.HasPrefix(key, id+"\x00") {
			delete(m.aiSearchTimes, key)
		}
	}
	for key := range m.nextRun {
		if strings.HasPrefix(key, id+"|") {
			delete(m.nextRun, key)
		}
	}
	for key := range m.scheduleInFlight {
		if strings.HasPrefix(key, id+"|") {
			delete(m.scheduleInFlight, key)
		}
	}
	for key := range m.scheduleJobs {
		if strings.HasPrefix(key, id+"|") {
			delete(m.scheduleJobs, key)
		}
	}
	hooks := append([]func(context.Context, string){}, m.removeHooks...)
	m.mu.Unlock()
	for _, fn := range hooks {
		fn(ctx, id)
	}
	m.broadcast(ctx)
	m.emitBusEvent(events.EventPluginPackageRemoved, id, events.PluginPackageRemovedPayload{
		PluginID: id,
		Name:     r.Name,
		Version:  r.Version,
	}, false)
	return nil
}

// OnInstall registers a callback run once when a plugin is newly installed.
func (m *Manager) OnInstall(fn func(ctx context.Context, r Record)) {
	if fn == nil {
		return
	}
	m.mu.Lock()
	m.installHooks = append(m.installHooks, fn)
	m.mu.Unlock()
}

// OnRemove registers a callback run after a plugin is uninstalled.
func (m *Manager) OnRemove(fn func(ctx context.Context, id string)) {
	m.mu.Lock()
	m.removeHooks = append(m.removeHooks, fn)
	m.mu.Unlock()
}

// Prune applies the plugin retention that is not bounded at write time:
// settled journal entries past JournalSettledMaxAge.
func (m *Manager) Prune(ctx context.Context) (int64, error) {
	if m == nil || m.store == nil || m.store.db == nil {
		return 0, nil
	}
	return m.store.pruneJournal(ctx)
}

// markReplaceLibraries records that the owner confirmed rebuilding pluginID's
// changed kinds; the next library sync takes it (TakeReplaceLibraries).
func (m *Manager) markReplaceLibraries(pluginID string) {
	m.replaceMu.Lock()
	defer m.replaceMu.Unlock()
	if m.replaceLibs == nil {
		m.replaceLibs = map[string]bool{}
	}
	m.replaceLibs[pluginID] = true
}

// TakeReplaceLibraries reports, once, whether the owner confirmed rebuilding
// pluginID's changed kinds at its last install.
func (m *Manager) TakeReplaceLibraries(pluginID string) bool {
	if m == nil {
		return false
	}
	m.replaceMu.Lock()
	defer m.replaceMu.Unlock()
	ok := m.replaceLibs[pluginID]
	delete(m.replaceLibs, pluginID)
	return ok
}
