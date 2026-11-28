package resolver

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/braedonsaunders/ether/internal/clusterbus"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/events"
	"github.com/braedonsaunders/ether/internal/nzb"
	"github.com/braedonsaunders/ether/internal/releaseparse"
	"github.com/braedonsaunders/ether/internal/releasepolicy"
	"github.com/braedonsaunders/ether/internal/vault"
)

const (
	defaultMaxTraces  = 50
	maxHealthAttempts = 32
	// speculativeHealthAttempts caps a warm-up nobody has pressed Play on:
	// every health check downloads an NZB, and indexers count grabs. A Play
	// that takes the warm-up over lifts the cap (#224).
	speculativeHealthAttempts = 4
	defaultSearchCacheTTL     = 6 * time.Hour
	defaultUnavailableTTL     = 24 * time.Hour
	defaultMaxCandidates      = 3
	// Newly aired episodes and releases (<72h since release date) use a short
	// unavailable cooloff and empty-search TTL so checking at 8 PM before an
	// upload drops never blocks playback at 10 PM.
	freshReleaseWindow  = 72 * time.Hour
	freshReleaseCooloff = 3 * time.Minute
	// unavailableRecheck bounds how long a remembered "unavailable" answer
	// is trusted when debrid cached-only is off: an uncached torrent sent
	// to download may become ready after the remembered failure. The
	// decision is based on lastUnavailableChecked, which peeks at the
	// checked time of the most recent unavailable entry even after that
	// entry has expired: within this window the remembered answer still
	// serves in the fast-path shape (no Exclude, Prefer, pin or
	// discovered releases), and past it that shape does one fresh search,
	// bypassing the search cache — which may queue the download or serve
	// it ready. Availability entries keep their own TTLs (the
	// health-failed write is capped at failedHealthTTL, fresh releases
	// use freshReleaseCooloff), so Available/KnownUnavailableSince stop
	// hiding a title when the entry expires, without losing the recheck
	// timestamp. With cached-only on, or in the other request shapes, the
	// remembered answer stands until its entry expires. The resolver
	// itself never polls: the recheck runs only when a caller invokes
	// Resolve, which includes user-initiated playback as well as
	// PreResolve and the watchlist Check, each bounded by that caller's
	// own budgets and gates.
	unavailableRecheck = 5 * time.Minute
)

// AnimeCour maps an offset TMDB anime season episode to its 1-based cour
// (part) number, cour-relative episode, and anime-lists TVDB season/episode.
type AnimeCour interface {
	LookupCour(ctx context.Context, req core.SearchRequest) (core.CourMapping, bool, error)
}

// LibraryProfiles answers library membership for quality rules: whether a
// title is in a library a rule names. Implementations cache per title and
// read only the libraries asked about (one indexed read each on a miss).
type LibraryProfiles interface {
	InLibrary(ctx context.Context, t *core.Title, id core.LibraryID) bool
}

// Options tunes a Service. Zero values get sensible defaults.
type Options struct {
	Vault    vault.Store
	Verifier vault.Verifier
	// EpisodeNumbering reads independent release numbering. cachedOnly forbids upstream work.
	EpisodeNumbering func(context.Context, core.SearchRequest, bool) (core.ReleaseNumbering, error)
	// EpisodeIdentity must read only bounded local catalogue data; no upstream calls.
	EpisodeIdentity func(context.Context, core.RatingKey, int, int, []string) (core.EpisodeIdentity, error)
	SpecialTitles   func(context.Context, int64) []string
	SearchObserved  func(context.Context, *core.Title, []core.Release)
	Discovered      func(context.Context, *core.Title) []core.Release
	Observed        func(context.Context, *core.Title, core.Release)
	Invalidated     func(context.Context, string)
	// SeriesNamesake reports a same-name show with a different premiere year.
	SeriesNamesake func(ctx context.Context, show core.RatingKey, title string, year int) (bool, error)
	// SeriesYearConflict reports whether the catalogue holds a different
	// show titled title that first aired in year (see series_year.go).
	SeriesYearConflict func(ctx context.Context, show core.RatingKey, title string, year int) (bool, error)
	// ReleaseTitleIDs independently resolves unmatched names; ambiguity returns no IDs.
	ReleaseTitleIDs func(context.Context, core.MediaType, string) (core.IDs, error)
	// VerifyTitleIDs verifies conflicting indexer title IDs against TMDB/TVDB
	// external ID mappings when title/alias and episode/air-date already match.
	VerifyTitleIDs func(context.Context, core.MediaType, core.IDs, core.IDs) (TitleIDVerdict, error)
	// AnimeCour maps multi-cour TMDB anime season episodes to their cour
	// and cour-relative episode number; nil disables cour mapping.
	AnimeCour AnimeCour
	// AnimeIDs maps anime titles to AniList IDs for SeaDex release ranking;
	// nil disables SeaDex lookups.
	AnimeIDs AnimeIDs
	// SeaDexURL overrides DefaultSeaDexURL; empty uses DefaultSeaDexURL.
	SeaDexURL string
	// SeaDexClient queries SeaDex; nil builds a public-only safefetch client.
	SeaDexClient *http.Client
	// SeaDexTTL bounds how long a SeaDex entry stays fresh before background refresh (0 = 24h).
	SeaDexTTL time.Duration
	// LibraryProfiles resolves a title's per-library quality profile; nil
	// leaves selection to the existing home/away defaults.
	LibraryProfiles LibraryProfiles
	// UsenetServing reports whether at least one primary Usenet provider is
	// able to serve new picks right now (false when all are in backoff).
	UsenetServing func() bool
	// ProbedMedia returns cached probed MediaInfo for a release from memory
	// (no I/O) so ranking and variant lists can use real tracks when available.
	ProbedMedia   func(core.Release) (core.MediaInfo, bool)
	Logger        *slog.Logger
	Now           func() time.Time
	MaxTraces     int
	RetentionDays int // posts older than this are penalised; 0 = no extra penalty
	// HealthTTL is how long a passed health check is trusted before the
	// release is sampled again (default 6h; negative disables the cache).
	HealthTTL time.Duration
	// HistoryMax bounds the persisted resolution history (default 5000
	// rows); HistoryMaxAge drops older rows (default 60 days).
	HistoryMax    int
	HistoryMaxAge time.Duration
	// Events receives non-blocking resolver.* domain events when set.
	Events *events.Bus
}

// CandidateTrace explains one candidate's fate in a resolution.
type CandidateTrace struct {
	IndexerID     string                `json:"indexerId,omitempty"`
	PackID        string                `json:"packId,omitempty"`
	Preferences   *releasepolicy.Result `json:"preferences,omitempty"`
	ReleaseID     string                `json:"releaseId"`
	Name          string                `json:"name"`
	Indexer       string                `json:"indexer"`
	Score         float64               `json:"score"`
	CustomFormats []FormatMatch         `json:"customFormats,omitempty"`
	// MinimumFormatScore records a soft score shortfall, including accepted releases.
	MinimumFormatScore  *int   `json:"minimumFormatScore,omitempty"`
	CompatibilityReason string `json:"compatibilityReason,omitempty"`
	// Route explains a move by source routing (routing.go), e.g. "faster
	// source: cached on debrid".
	Route          string        `json:"route,omitempty"`
	Rejected       bool          `json:"rejected"`
	Reason         string        `json:"reason,omitempty"`
	Size           int64         `json:"size,omitempty"`
	Height         int           `json:"height,omitempty"`    // parsed resolution, 0 unknown
	Pinned         bool          `json:"pinned,omitempty"`    // the owner's pin for the title
	Attempted      bool          `json:"attempted,omitempty"` // fetch or health validation actually ran
	HealthChecked  bool          `json:"healthChecked"`
	Healthy        bool          `json:"healthy"`
	FetchDuration  time.Duration `json:"fetchDuration"`
	HealthDuration time.Duration `json:"healthDuration"`
	// err is the validation error behind Reason (uncached.go).
	err error
}

// Trace is one recorded resolution attempt, for the admin UI.
type Trace struct {
	PluginKind                bool             `json:"pluginKind,omitempty"`
	Discovery                 []DiscoveryTrace `json:"discovery,omitempty"`
	Limits                    *TraceLimits     `json:"limits,omitempty"`
	TitleKey                  core.RatingKey   `json:"titleKey"`
	TitleName                 string           `json:"titleName"`
	Profile                   string           `json:"profile"`
	ProfileSource             ProfileSource    `json:"profileSource,omitempty"`
	ProfileRuleID             string           `json:"profileRuleId,omitempty"`
	LimitRuleIDs              []string         `json:"limitRuleIds,omitempty"`
	At                        time.Time        `json:"at"`
	Candidates                []CandidateTrace `json:"candidates"`
	Winner                    string           `json:"winner,omitempty"` // release id
	Err                       string           `json:"err,omitempty"`
	SearchDuration            time.Duration    `json:"searchDuration"`
	SourcePreparationDuration time.Duration    `json:"sourcePreparationDuration"`
	TotalDuration             time.Duration    `json:"totalDuration"`
	// Failover marks a resolution that excluded already-tried releases.
	Failover bool `json:"failover,omitempty"`
	// Pinned is the owner-pinned release id considered for the title;
	// PinStatus says what became of it ("used", "unhealthy: …", …).
	Pinned    string `json:"pinned,omitempty"`
	PinStatus string `json:"pinStatus,omitempty"`
	// Omitted counts candidates dropped from a persisted trace.
	Omitted int `json:"omitted,omitempty"`
	// Routing summarises the source-routing decision (routing.go).
	Routing string `json:"routing,omitempty"`
	// Downgrade explains a winner well below the best quality on offer
	// (failover.go): what failed and what plays instead.
	Downgrade string `json:"downgrade,omitempty"`
	// Ceiling explains a winner below 4K (explain.go): the profile or
	// client limit, or why each 4K release lost.
	Ceiling string `json:"ceiling,omitempty"`
	// Chooser is who selected the winner: "auto" (Ether's ranking) or
	// "viewer" (an explicit Play Version pick that played). Empty on
	// traces recorded before this field, and when nothing was picked.
	Chooser string `json:"chooser,omitempty"`
	// ChosenVersion is the release name when Chooser is "viewer".
	ChosenVersion string `json:"chosenVersion,omitempty"`
	// Unverified marks a winner played after every health check timed
	// out without a confirmed failure (unverified.go).
	Unverified bool `json:"unverified,omitempty"`
	// Queries are the indexer requests this resolution made (capped),
	// QueriesDropped counts the ones past the cap. A support capture
	// exports them with the indexer names replaced.
	Queries        []core.SearchQuery `json:"queries,omitempty"`
	QueriesDropped int                `json:"queriesDropped,omitempty"`
	// ClientID and UserID tie the resolution to the device and viewer
	// that asked, so a support capture includes only that device's
	// searches. Speculative marks warm-ups and detail discovery.
	ClientID    string `json:"clientId,omitempty"`
	UserID      int64  `json:"userId,omitempty"`
	Speculative bool   `json:"speculative,omitempty"`
}

// DiscoveryTrace records only the origin and number of returned releases.
type DiscoveryTrace struct {
	Path   string `json:"path"`
	Count  int    `json:"count"`
	Rescue bool   `json:"rescue,omitempty"`
}
type TraceLimits struct {
	ProfileMinHeight int `json:"profileMinHeight"`
	// ProfileMaxHeight is the effective (client-clamped) ceiling;
	// ProfileConfiguredMaxHeight is the profile's own ceiling before
	// that clamp, so a rejection can name whoever actually limited it.
	ProfileMaxHeight int `json:"profileMaxHeight"`
	// ProfileConfiguredMaxHeight carries presence: nil means the
	// trace predates the field (configured ceiling unknown), while a
	// pointer to 0 is an explicitly unlimited ("Any") profile.
	ProfileConfiguredMaxHeight *int `json:"profileConfiguredMaxHeight,omitempty"`
	ClientMaxHeight            int  `json:"clientMaxHeight"`
	ClientMaxBitrateKbps       int  `json:"clientMaxBitrateKbps"`
	// AllowHDR and AllowDolbyVision are the profile's HDR policy after
	// the client clamp. A support capture uses them to show an SDR-only
	// ceiling without reading the owner's settings file.
	AllowHDR         bool `json:"allowHdr,omitempty"`
	AllowDolbyVision bool `json:"allowDolbyVision,omitempty"`
}

type searchCacheEntry struct {
	forgotten bool
	refreshed time.Time
	results   []core.Release
	expires   time.Time
	// req is the request the results answer, with the enrichment its
	// writer applied.
	req core.SearchRequest
}

// serves reports whether e answers req. Each part of the enrichment a
// request carries (release numbering, cour mapping) must be the one the
// results were searched with; a part it lacks takes what the entry has,
// because the enriched search only adds queries.
func (e searchCacheEntry) serves(req core.SearchRequest) bool {
	if n := numberingEnrichment(req); n != "" && n != numberingEnrichment(e.req) {
		return false
	}
	if c := courEnrichment(req); c != "" && c != courEnrichment(e.req) {
		return false
	}
	return true
}

type inflight struct {
	done      chan struct{}
	res       *core.Resolution
	err       error
	abandoned bool   // the leader's own context ended the resolution
	play      func() // marks the shared resolution as an explicit Play
}

// Service implements core.Resolver.
type Service struct {
	vault                     vault.Store
	verifier                  vault.Verifier
	releaseTitles             releaseTitleCache
	releaseTitleIDs           func(context.Context, core.MediaType, string) (core.IDs, error)
	titleIDs                  titleIDVerifications
	verifyTitleIDs            func(context.Context, core.MediaType, core.IDs, core.IDs) (TitleIDVerdict, error)
	episodeNumbering          func(context.Context, core.SearchRequest, bool) (core.ReleaseNumbering, error)
	episodeIdentity           func(context.Context, core.RatingKey, int, int, []string) (core.EpisodeIdentity, error)
	specialTitles             func(context.Context, int64) []string
	years                     seriesYears
	seriesNamesake            func(context.Context, core.RatingKey, string, int) (bool, error)
	seriesYearConflict        func(ctx context.Context, show core.RatingKey, title string, year int) (bool, error)
	animeCour                 AnimeCour
	animeIDs                  AnimeIDs
	seaDexURL                 string
	seaDexClient              *http.Client
	seaDexTTL                 time.Duration
	libProfiles               LibraryProfiles
	availabilityGeneration    uint64
	availabilityDeadlines     availabilityDeadlines
	availabilityDeadlineByKey map[int64]*availabilityDeadline

	subtitleChecks map[string]bool
	subMiss        map[string]time.Time
	subVerdicts    map[string]bool
	rejections     map[core.RatingKey][]core.Release
	searchObserved func(context.Context, *core.Title, []core.Release)
	discovered     func(context.Context, *core.Title) []core.Release
	observed       func(context.Context, *core.Title, core.Release)
	invalidated    func(context.Context, string)
	usenetServing  func() bool
	indexer        core.Indexer
	health         core.HealthChecker
	db             *sql.DB
	log            *slog.Logger
	now            func() time.Time

	cfgMu                sync.RWMutex
	rcfg                 config.Resolver
	quality              config.Quality
	catalogLang          string // catalogue metadata language ("en-US"): the server's language
	debridCachedOnly     bool
	videoTranscodingOff  bool
	remoteMaxBitrateKbps int
	probedMedia          func(core.Release) (core.MediaInfo, bool)
	localCacheHas        func(releaseID string) bool
	scorer               *FormatScorer
	indexerPriority      map[string]int  // indexer id → Priority (lower ranks first)
	indexerAddonOrder    map[string]bool // indexer id → keep Stremio add-on's own result order
	indexerSourceBand    map[string]int  // indexer id → priority band when any add-on keeps its own order
	defaultSourceBand    int             // band for sources not listed in cfg.Indexers
	scorerErr            error
	sources              string // sourcesKey of the applied config

	mu               sync.RWMutex
	bad              map[string]badMark     // release id -> blocklist mark
	badContent       map[string]badMark     // cross-indexer content id -> blocklist mark
	contentByRelease map[string]contentLink // release id -> learned content identities
	failedGrabs      map[string]failedGrabMark
	reps             map[string]reputation
	avail            map[int64]availEntry
	searchCache      map[string]searchCacheEntry
	retentionDays    int
	verified         map[string]time.Time // release id -> last passed health check
	verifiedMiss     map[string]time.Time // release id -> when resolver_verified last had no fresh check
	failedHealth     map[string]time.Time // confirmed source failures, briefly skipped on subsequent resolves
	healthTTL        time.Duration
	seaDexMem        map[int64]*seaDexEntry
	seaDexDBMiss     map[int64]time.Time
	seaDexKeys       map[string]seaDexKeyCache
	seaDexKeyMiss    map[string]time.Time
	seaDexFail       map[int64]time.Time

	tracesMu  sync.Mutex
	traces    []Trace
	maxTraces int

	// liveMu is a leaf lock guarding in-flight search streaming for
	// concurrent version-slot observation (WatchVersionSlots). Never acquire
	// s.mu, s.bgMu, or ls.mu while holding s.liveMu.
	liveMu       sync.Mutex
	liveSearches map[string]*liveSearch
	liveWaiters  map[string][]chan struct{}

	bgCtx    context.Context
	bgCancel context.CancelFunc
	bgMu     sync.Mutex
	// availPersistMu orders a queued availability write against forgetAvailability's
	// delete, so a revoked verdict is never written back after it was cleared.
	availPersistMu           sync.Mutex
	bgClosed                 bool
	bookkeeping              []bookkeepingJob
	bookkeepingSize          int
	bookkeepingRunning       bool
	bookkeepingIdle          chan struct{}
	bgWG                     sync.WaitGroup
	detailVariants           map[[32]byte]*detailVariantSnapshot        // bgMu; lazy detail labels
	detailVariantWorkers     int                                        // bgMu
	refills                  map[string]searchRefill                    // search key -> in-flight discovery; guarded by bgMu
	refillRetryAfter         map[string]refillMark                      // next detail discovery allowed; guarded by bgMu
	speculativeBudgetUntil   map[string]time.Time                       // purpose -> cooloff after discovery-share exhaustion; guarded by bgMu
	seaDexQueue              []seaDexJob                                // guarded by bgMu
	seaDexPending            map[string]bool                            // guarded by bgMu
	seaDexRunning            bool                                       // guarded by bgMu
	seaDexIdle               chan struct{}                              // guarded by bgMu
	releaseTitleQueue        []releaseTitleWarmJob                      // guarded by bgMu
	releaseTitlePending      map[releaseTitleKey]bool                   // guarded by bgMu
	releaseTitleRunning      bool                                       // guarded by bgMu
	releaseTitleIdle         chan struct{}                              // guarded by bgMu
	releaseTitleWarmInterval time.Duration                              // 0 = defaultReleaseTitleWarmInterval
	releaseTitleWarmWait     func(context.Context, time.Duration) error // nil = timer wait
	releaseTitleMaxEntries   int                                        // 0 = maxReleaseTitleEntries

	flightMu sync.Mutex
	flights  map[string]*inflight

	delivery       deliveryState
	clientDelivery clientDeliveryState

	route routingState // source routing: startup measurements and signals

	con consoleState // pins, release memory, health results, history (console.go)

	extend extendState // descriptor registry for ranking/rules (extend_registry.go)

	invalMu sync.Mutex
	inval   *clusterbus.Invalidator // nil on a standalone server

	usenetVerdicts *UsenetVerdictCache

	events *events.Bus
}

var _ core.Resolver = (*Service)(nil)

// New builds a resolver Service. indexer is the aggregate indexer
// (typically *newznab.Manager); health may be nil (health checks are
// then skipped); db may be nil (state then lives in memory).
func New(indexer core.Indexer, health core.HealthChecker, db *sql.DB, cfg config.Config, opts Options) *Service {
	s := &Service{vault: opts.Vault, verifier: opts.Verifier, releaseTitleIDs: opts.ReleaseTitleIDs, verifyTitleIDs: opts.VerifyTitleIDs, episodeNumbering: opts.EpisodeNumbering, episodeIdentity: opts.EpisodeIdentity, specialTitles: opts.SpecialTitles, seriesNamesake: opts.SeriesNamesake, seriesYearConflict: opts.SeriesYearConflict, animeCour: opts.AnimeCour, libProfiles: opts.LibraryProfiles, searchObserved: opts.SearchObserved, discovered: opts.Discovered, observed: opts.Observed, invalidated: opts.Invalidated, usenetServing: opts.UsenetServing, probedMedia: opts.ProbedMedia,
		indexer: indexer, health: health, db: db,
		bad: map[string]badMark{}, badContent: map[string]badMark{},
		contentByRelease: map[string]contentLink{}, failedGrabs: map[string]failedGrabMark{},
		reps:  map[string]reputation{},
		avail: map[int64]availEntry{}, searchCache: map[string]searchCacheEntry{},
		flights:        map[string]*inflight{},
		retentionDays:  opts.RetentionDays,
		maxTraces:      opts.MaxTraces,
		verified:       map[string]time.Time{},
		healthTTL:      opts.HealthTTL,
		usenetVerdicts: NewUsenetVerdictCache(),
		events:         opts.Events,
	}
	s.bgCtx, s.bgCancel = context.WithCancel(context.Background())
	if s.healthTTL == 0 {
		s.healthTTL = 6 * time.Hour
	}
	s.log = opts.Logger
	if s.log == nil {
		s.log = slog.Default()
	}
	s.now = opts.Now
	if s.now == nil {
		s.now = time.Now
	}
	if s.maxTraces <= 0 {
		s.maxTraces = defaultMaxTraces
	}
	s.UpdateConfig(cfg)
	s.con.init(opts)
	s.initSeaDex(opts)
	s.loadPersisted(context.Background())
	return s
}

// SetProbedMedia wires a zero-I/O lookup returning cached probed container
// MediaInfo for a release so ranking and version lists use real tracks when
// available.
func (s *Service) SetProbedMedia(fn func(core.Release) (core.MediaInfo, bool)) {
	if s == nil {
		return
	}
	s.cfgMu.Lock()
	s.probedMedia = fn
	s.cfgMu.Unlock()
}

// ContentIDForRelease returns the known W116 content ID for releaseID from
// in-memory links without touching the database.
func (s *Service) ContentIDForRelease(releaseID string) string {
	if s == nil || releaseID == "" {
		return ""
	}
	s.mu.RLock()
	link, ok := s.contentByRelease[releaseID]
	s.mu.RUnlock()
	if !ok {
		return ""
	}
	if link.contentID != "" {
		return link.contentID
	}
	return link.preFetchID
}

func (s *Service) probedMediaFor(rel core.Release) (core.MediaInfo, bool) {
	if s == nil {
		return core.MediaInfo{}, false
	}
	s.cfgMu.RLock()
	fn := s.probedMedia
	s.cfgMu.RUnlock()
	if fn == nil {
		return core.MediaInfo{}, false
	}
	if rel.ContentID == "" && rel.ID != "" {
		if cid := s.ContentIDForRelease(rel.ID); strings.HasPrefix(cid, nzb.ContentIDPrefix) && !strings.Contains(cid, "|file:") {
			rel.ContentID = cid
		}
	}
	info, ok := fn(rel)
	if !ok || info.Estimated || len(info.Streams) == 0 {
		return core.MediaInfo{}, false
	}
	return info, true
}

func (s *Service) probedAudioLanguagesFor(rel core.Release) []string {
	info, ok := s.probedMediaFor(rel)
	if !ok {
		return nil
	}
	return releasepolicy.ProbedAudioLanguages(info.Streams)
}

func withProbedAudioLanguages(rel core.Release, probedLangs []string) core.Release {
	if len(probedLangs) == 0 {
		return rel
	}
	out := rel
	out.Info.Languages = probedLangs
	return out
}

// UpdateConfig swaps the resolver/quality config snapshot. When the owner
// changes where releases come from (indexers, Usenet or debrid accounts,
// debrid's cached-only switch), cached searches and "not available"
// answers describe the old sources and are forgotten.
func (s *Service) UpdateConfig(cfg config.Config) {
	key := sourcesKey(cfg)
	s.cfgMu.Lock()
	s.rcfg = cfg.Resolver
	s.quality = cfg.Quality
	s.catalogLang = cfg.Catalog.Language
	s.debridCachedOnly = cfg.Debrid.CachedOnly
	s.videoTranscodingOff = cfg.Transcode.VideoTranscodingOff()
	s.remoteMaxBitrateKbps = cfg.Transcode.RemoteMaxBitrateKbps
	s.scorer, s.scorerErr = NewFormatScorer(cfg.Resolver.CustomFormats)
	s.indexerPriority = make(map[string]int, len(cfg.Indexers))
	for _, ix := range cfg.Indexers {
		s.indexerPriority[ix.ID] = ix.Priority
	}
	s.indexerAddonOrder, s.indexerSourceBand, s.defaultSourceBand = computeSourceBands(cfg.Indexers)
	changed := s.sources != "" && s.sources != key
	s.sources = key
	s.cfgMu.Unlock()
	s.InvalidateDetailVariants()
	if changed {
		s.forgetSourceResults(context.Background())
	}
}

// sourcesKey fingerprints the settings that decide which releases a search
// can find and which of them can play.
func sourcesKey(cfg config.Config) string {
	var b strings.Builder
	for _, ix := range cfg.Indexers {
		fmt.Fprintf(&b, "i|%s|%t|%s|%s|%s|%t\n", ix.ID, ix.Enabled, ix.URL, ix.Kind, ix.Protocol, ix.UseAddonOrder)
	}
	for _, p := range cfg.Usenet.Providers {
		fmt.Fprintf(&b, "u|%s|%t\n", p.ID, p.Enabled)
	}
	fmt.Fprintf(&b, "hardsub|%s\n", cfg.Resolver.HardSubtitles)
	fmt.Fprintf(&b, "d|%t\n", cfg.Debrid.CachedOnly)
	for _, d := range cfg.Debrid.Providers {
		fmt.Fprintf(&b, "d|%s|%s|%t\n", d.ID, d.Kind, d.Enabled)
	}
	return b.String()
}

// forgetSourceResults drops cached searches, "not available" answers and
// briefly-skipped failed sources, so the next play searches the current
// sources afresh.
func (s *Service) forgetSourceResults(ctx context.Context) {
	s.mu.Lock()
	for k, e := range s.avail {
		if !e.available {
			s.forgetAvailabilityEpochLocked(k)
			delete(s.avail, k)
		}
	}
	clear(s.searchCache)
	clear(s.failedHealth)
	clear(s.failedGrabs)
	s.mu.Unlock()
	s.usenetVerdicts.Clear()
	if s.db != nil {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM resolver_availability WHERE available = 0`); err != nil {
			s.log.Warn("forget unavailable titles", "err", err)
		}
		if _, err := s.db.ExecContext(ctx, `DELETE FROM resolver_failed_grabs`); err != nil {
			s.log.Warn("forget failed grabs", "err", err)
		}
	}
	s.log.Info("sources changed: cleared cached searches and unavailable titles")
}

// VODCatalogChanged is called when an IPTV provider's VOD catalogue changed.
// VOD items join search results, so cached searches and "not available"
// answers may be stale; failed grabs, health failures and Usenet verdicts
// are about indexer and Usenet releases and stay, so a catalogue change does
// not make Ether re-grab NZBs it already knows are broken.
func (s *Service) VODCatalogChanged(ctx context.Context) {
	s.mu.Lock()
	for k, e := range s.avail {
		if !e.available {
			s.forgetAvailabilityEpochLocked(k)
			delete(s.avail, k)
		}
	}
	clear(s.searchCache)
	s.mu.Unlock()
	if s.db != nil {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM resolver_availability WHERE available = 0`); err != nil {
			s.log.Warn("forget unavailable titles", "err", err)
		}
	}
	s.log.Info("IPTV VOD catalogue changed: cleared cached searches and unavailable titles")
}

func (s *Service) resolverCfg() config.Resolver {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.rcfg
}

func (s *Service) qualityCfg() config.Quality {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.quality
}

// profileAllowsCam reports whether any quality profile plays pre-release
// copies (cinema recordings, screeners).
func (s *Service) profileAllowsCam() bool {
	for _, p := range s.qualityCfg().Profiles {
		if p.AllowCam {
			return true
		}
	}
	return false
}

func (s *Service) cachedOnly() bool {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.debridCachedOnly
}

func (s *Service) searchTTL() time.Duration {
	if d := s.resolverCfg().SearchCacheTTL.D(); d > 0 {
		return d
	}
	return defaultSearchCacheTTL
}

func (s *Service) cooloff() time.Duration {
	if d := s.resolverCfg().UnavailableCooloff.D(); d > 0 {
		return d
	}
	return defaultUnavailableTTL
}

func isFreshRelease(t *core.Title, now time.Time) bool {
	if t == nil || t.Released.IsZero() {
		return false
	}
	age := now.Sub(t.Released)
	return age >= -24*time.Hour && age <= freshReleaseWindow
}

func (s *Service) cooloffFor(t *core.Title) time.Duration {
	d := s.cooloff()
	if isFreshRelease(t, s.now()) && d > freshReleaseCooloff {
		return freshReleaseCooloff
	}
	return d
}

func (s *Service) maxCandidates() int {
	if n := s.resolverCfg().MaxCandidates; n > 0 {
		return n
	}
	return defaultMaxCandidates
}

// Available implements core.Resolver.
func (s *Service) Available(ctx context.Context, key core.RatingKey) (bool, bool) {
	e, ok := s.getAvailability(ctx, int64(key))
	if !ok {
		return false, false
	}
	return true, e.available
}

// MarkBad implements core.Resolver.
func (s *Service) MarkBad(ctx context.Context, releaseID string, reason string) error {
	if releaseID == "" {
		return fmt.Errorf("resolver: empty release id")
	}
	contentKeys := s.idContentKeysDB(ctx, releaseID)
	s.mu.Lock()
	m := badMark{reason: reason, at: s.now()}
	if prev, ok := s.bad[releaseID]; ok && prev.manual {
		// An automatic mark never downgrades an owner block (persistBad
		// keeps the owner's reason too); it only refreshes the clock.
		m.reason, m.manual = prev.reason, true
	}
	s.bad[releaseID] = m
	s.markBadContentLocked(contentKeys, m)
	s.mu.Unlock()
	s.forgetVerifiedWithContent(ctx, releaseID, contentKeys...)
	if urlKey, nzbHash, contentID := s.usenetKeysForReleaseID(releaseID, contentKeys); urlKey != "" || nzbHash != "" || contentID != "" {
		s.usenetVerdicts.RecordPlaybackFailure(s.now(), urlKey, nzbHash, contentID, reason, false)
	}
	s.notePinsBlocked(ctx, releaseID, reason)
	e := s.blockEntryFor(releaseID, reason, false)
	if e.ContentID == "" {
		e.ContentID = preferredContentKey(contentKeys)
	}
	if err := s.persistBad(ctx, e); err != nil {
		return err
	}
	s.emitReleaseBlocklisted(e, BlockAuto)
	return nil
}

func (s *Service) isBad(id string) bool {
	_, ok := s.badReason(id)
	return ok
}

// badMark is one in-memory blocklist mark. Automatic marks expire
// badRetention after they were last marked; owner (manual) blocks never do.
type badMark struct {
	reason string
	at     time.Time // when it was (last) marked
	manual bool
}

func (m badMark) live(now time.Time) bool {
	return m.manual || now.Sub(m.at) < badRetention
}

// badReason reports whether a release is on the live blocklist and why.
func (s *Service) badReason(id string) (string, bool) {
	if id == "" {
		return "", false
	}
	now := s.now()
	s.mu.RLock()
	if m, ok := s.bad[id]; ok && m.live(now) {
		s.mu.RUnlock()
		return m.reason, true
	}
	if m, ok := s.badContent[id]; ok && m.live(now) {
		s.mu.RUnlock()
		return m.reason, true
	}
	s.mu.RUnlock()
	keys := s.idContentKeys(id)
	if len(keys) == 0 {
		return "", false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, k := range keys {
		if m, ok := s.badContent[k]; ok && m.live(now) {
			return m.reason, true
		}
	}
	return "", false
}

// Traces returns the most recent resolution traces, newest first.
func (s *Service) Traces() []Trace {
	s.tracesMu.Lock()
	defer s.tracesMu.Unlock()
	out := make([]Trace, len(s.traces))
	for i, tr := range s.traces {
		out[len(s.traces)-1-i] = tr
	}
	return out
}

func (s *Service) addTrace(tr Trace) {
	s.tracesMu.Lock()
	defer s.tracesMu.Unlock()
	s.traces = append(s.traces, tr)
	if len(s.traces) > s.maxTraces {
		s.traces = append([]Trace(nil), s.traces[len(s.traces)-s.maxTraces:]...)
	}
}

// ProfileFor exposes SelectProfile against the live config.
func (s *Service) ProfileFor(caps core.ClientCaps, user *core.User) core.QualityProfile {
	return SelectProfile(s.qualityCfg(), caps, user)
}

func searchRequestFor(t *core.Title) core.SearchRequest {
	if t.Type == core.TypeEpisode || t.Key.Kind() == core.KindEpisode {
		title := t.ShowTitle
		if title == "" {
			title = t.Title
		}
		return core.SearchRequest{Type: core.TypeEpisode, IDs: t.ShowIDs, EpisodeSource: t.EpisodeOrder.Source, Title: title, OriginalTitle: t.ShowOriginalTitle, EnglishTitle: t.ShowEnglishTitle, Year: t.ShowYear, Season: t.Season, Episode: t.Episode, EpisodeTitle: t.Title,
			AbsoluteEpisode: t.AbsoluteEpisode, Anime: t.IsAnime(), AlternativeTitles: t.ShowAlternativeTitles, Released: cmp.Or(t.EpisodeAirDate, t.Released)}
	}
	return core.SearchRequest{Type: core.TypeMovie, IDs: t.IDs, Title: t.Title, OriginalTitle: t.OriginalTitle, EnglishTitle: t.EnglishTitle, Year: t.Year, Anime: t.IsAnime(), AlternativeTitles: t.AlternativeTitles, Released: t.Released}
}

func (s *Service) searchRequest(ctx context.Context, t *core.Title) core.SearchRequest {
	req := searchRequestFor(t)
	if s != nil && s.specialTitles != nil && req.Type == core.TypeEpisode && req.Season == 0 && req.EpisodeSource != core.MetadataTVDB && req.IDs.TMDB > 0 {
		req.SpecialTitles = s.specialTitles(ctx, req.IDs.TMDB)
	}
	if s != nil && s.animeCour != nil && req.EpisodeSource != core.MetadataTVDB && req.Anime && req.Type == core.TypeEpisode && req.Season > 0 && req.Episode > 1 && req.IDs.TMDB > 0 {
		if m, ok, err := s.animeCour.LookupCour(ctx, req); err == nil && ok {
			req.Cour, req.CourEpisode = m.Cour, m.Episode
			req.CourTVDBSeason, req.CourTVDBEpisode = m.TVDBSeason, m.TVDBEpisode
		}
	}
	return req
}

// isAnime reports a title whose releases follow anime conventions
// (absolute episode numbers, fansub groups): TMDB's "anime" keyword, or
// Japanese-language animation.
// absoluteEpisode is the requested episode's number counted from the
// series start: the catalogue's count, or the episode number itself in
// the first season. 0 when unknown.
func absoluteEpisode(sreq core.SearchRequest) int {
	switch {
	case sreq.AbsoluteEpisode > 0:
		return sreq.AbsoluteEpisode
	case sreq.Season == 1:
		return sreq.Episode
	}
	return 0
}

func excludedByRelease(excluded []core.Release, rel core.Release) bool {
	for _, ex := range excluded {
		if core.SameReleaseContent(ex, rel) {
			return true
		}
	}
	return false
}

func flightKey(req core.ResolveRequest, profile core.QualityProfile) string {
	ex := append([]string(nil), req.Exclude...)
	sort.Strings(ex)
	var exRel []string
	if len(req.ExcludeReleases) > 0 {
		exRel = make([]string, len(req.ExcludeReleases))
		for i, r := range req.ExcludeReleases {
			exRel[i] = r.ReleaseContentKey()
		}
		sort.Strings(exRel)
	}
	capsJSON, _ := json.Marshal(req.Client)
	profileJSON, _ := json.Marshal(profile)
	// Viewers with different audio or subtitle rules can pick different
	// releases from the same search results.
	var audio, subtitles []string
	if req.User != nil {
		audio, subtitles = req.User.AudioLanguages, req.User.SubtitleLanguages
	}
	return fmt.Sprintf("%d|%s|%s|%s|%s|%s|%s|%t|%t|%s|%s|%t", req.Title.Key, profile.Name, strings.Join(ex, ","), strings.Join(exRel, ","), capsJSON, profileJSON, req.Prefer+"|"+req.PreferVideoCodec+"|"+req.PlayedRelease+"|"+req.PlayedSelector, req.PreferredOnly, req.ExactHEVCTranscode, strings.Join(audio, ","), strings.Join(subtitles, ","), releasepolicy.PrefersOriginalAudio(profile, req.User))
}

// Resolve implements core.Resolver. Concurrent calls for the same
// title+profile share one search via a singleflight.
func (s *Service) Resolve(ctx context.Context, req core.ResolveRequest) (*core.Resolution, error) {
	if req.Title == nil {
		return nil, fmt.Errorf("resolver: nil title")
	}
	if s.indexer == nil {
		return nil, fmt.Errorf("resolver: no indexer configured")
	}
	q := s.qualityCfg()
	inLib := s.membershipFor(ctx, req.Title)
	dec := decideProfile(q, req.Client, req.User, titleTarget(req.Title, inLib))
	req.Client = dec.caps
	profile := dec.profile
	req = s.withClientDeliveryBudget(req)
	videoOff, _ := s.transcodeSettingsFor(req.Client)
	key := fmt.Sprintf("%s|pin:%s|vc:%s|vt:%t|link:%d", flightKey(req, profile), s.pinFlightKey(req), s.viewerChoiceFlightKey(req), videoOff, req.ClientDeliveryBudgetKbps)
	if channelAvailabilityCheck(ctx) {
		key += "/channel-availability"
	}
	if core.SpeculativeIndexerWork(ctx) || core.IsBackgroundCheck(ctx) {
		key += "/background"
	}

	for {
		s.flightMu.Lock()
		if f, ok := s.flights[key]; ok {
			s.flightMu.Unlock()
			if core.ExplicitPlay(ctx) {
				f.play() // a Play joined: what the search prepares from now on is for a Play
			}
			select {
			case <-f.done:
				if f.abandoned && ctx.Err() == nil {
					// The leading caller went away (a client cancelling a
					// duplicate request); its cancellation is not this
					// caller's answer. Resolve again.
					continue
				}
				if f.err != nil && errors.Is(f.err, core.ErrForegroundOnly) && ctx.Err() == nil && core.OnDemandStreamsAllowed(ctx) {
					// A warm-up or background check stopped at a source
					// only a Play may open; this caller may open it.
					continue
				}
				return f.res, f.err
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		f := &inflight{done: make(chan struct{})}
		ctx, f.play = core.WithExplicitPlayLatch(ctx)
		s.flights[key] = f
		s.flightMu.Unlock()

		ctx = context.WithValue(ctx, observationTitleKey{}, req.Title)
		res, err := s.resolve(ctx, req, profile, q, dec)
		f.res, f.err, f.abandoned = res, err, err != nil && ctx.Err() != nil
		s.flightMu.Lock()
		delete(s.flights, key)
		s.flightMu.Unlock()
		close(f.done)
		return res, err
	}
}

func (s *Service) resolve(ctx context.Context, req core.ResolveRequest, profile core.QualityProfile, q config.Quality, dec profileDecision) (resolved *core.Resolution, resolveErr error) {
	var fp string
	if s.vault != nil {
		fp = ProfileFingerprint(profile, wantedAudio(profile, req.User, s.catalogLanguage()))
		choiceFP := fp
		if req.Prefer != "" && !autoVersion(req.Prefer) {
			choiceFP = ""
		}
		defer func() {
			if resolved != nil {
				r := resolved.Release
				var payload []byte
				if r.Protocol == core.ProtocolTorrent {
					payload = resolved.NZB
				}
				s.persistChoice(ctx, func(ctx context.Context) { s.recordChoice(ctx, r, choiceFP, payload) })
			}
		}()
	}
	var selector, slotRelease string
	var slotAutoFulfilled bool
	var failedReleases []string
	var failedPreference string
	defer func() {
		if resolved != nil {
			copy := *resolved
			copy.VersionSelector = ""
			if (slotRelease != "" && copy.Release.ID == slotRelease) || (slotAutoFulfilled && selector != "") {
				copy.VersionSelector = selector
			}
			if len(failedReleases) > 0 {
				copy.FailedReleases = failedReleases
			}
			copy.PreferenceFailed = failedPreference
			resolved = &copy
		}
	}()
	ctx = context.WithValue(ctx, requestBookkeepingKey{}, true)
	ctx = context.WithValue(ctx, speculativeChecksKey{}, new(atomic.Int64))
	started := time.Now()
	// An indexer that was paused or did not answer leaves the search
	// incomplete; nothing found then does not prove the title unavailable.
	ctx, coverage := core.WithSearchCoverage(ctx)
	ctx, queryLog := core.WithSearchLog(ctx)
	t := req.Title
	hasExclude := len(req.Exclude) > 0 || len(req.ExcludeReleases) > 0
	profSource, profRuleID, limitRuleIDs := dec.ruleDecision(q)
	trace := Trace{TitleKey: t.Key, TitleName: consoleTitleName(t), Profile: profile.Name,
		ProfileSource: profSource, ProfileRuleID: profRuleID, LimitRuleIDs: limitRuleIDs,
		At: s.now(), Failover: hasExclude,
		ClientID: req.Client.ClientID, Speculative: core.IsSpeculative(ctx) || core.SpeculativeIndexerWork(ctx)}
	if req.User != nil {
		trace.UserID = req.User.ID
	}
	trace.Limits = &TraceLimits{ProfileMinHeight: profile.MinHeight, ProfileMaxHeight: profile.MaxHeight, ProfileConfiguredMaxHeight: traceConfiguredMaxHeight(profile), ClientMaxHeight: req.Client.MaxHeight, ClientMaxBitrateKbps: req.Client.MaxBitrateKbps, AllowHDR: profile.AllowHDR, AllowDolbyVision: profile.AllowDolbyVision}
	defer func() {
		trace.TotalDuration = time.Since(started)
		trace.Queries, trace.QueriesDropped = queryLog.Queries()
		if resolved != nil {
			failedReleases = rejectedAttemptedReleaseIDs(&trace, resolved.Release.ID)
		}
		if !core.IsSpeculative(ctx) && !channelAvailabilityCheck(ctx) {
			s.logPlayDecision(trace)
		}
		trace.Ceiling = s.explainCeiling(&trace, profile, req)
		noteChooser(&trace, req)
		if trace.Ceiling != "" {
			s.log.Info("resolver: below 4K", "title", trace.TitleName, "profile", profile.Name, "why", trace.Ceiling)
		}
		s.addTrace(trace)
		s.queueHistory(trace, nil)
		s.emitResolveOutcome(req, profile, trace, resolved, resolveErr)
	}()

	exclude := make(map[string]bool, len(req.Exclude))
	for _, id := range req.Exclude {
		exclude[id] = true
	}

	// Precedence in the Auto slot: viewer choice, then owner pin, then owner
	// quality rules. A viewer choice retains full safety gates and falls back
	// silently when unhealthy, blocklisted or unavailable.
	if titleVC := s.viewerTitleChoiceForResolve(req, exclude); titleVC != nil {
		sreq := s.numberedRequest(ctx, s.searchRequest(ctx, t), true)
		if s.pinRejectReason(titleVC.Release, sreq, req.Client, exclude) == "" {
			vResults := withRelease(nil, titleVC.Release)
			vReq := req
			vReq.Prefer, vReq.PreferredOnly = titleVC.Release.ID, true
			rankProfile := profile
			if s.resolverCfg().OfferHigherVersions {
				if relaxed, ok := relaxedVariantProfile(profile, req.Client); ok {
					rankProfile = relaxed
				}
			}
			if cands, _, rankErr := s.rankCandidatesLocal(ctx, vReq, sreq, rankProfile, vResults, &trace, "", false); rankErr == nil && len(cands) > 0 {
				if out, vErr := s.resolvePreferred(ctx, vReq, nil, cands, &trace, 1); vErr == nil && out.resolution != nil {
					if autoVersion(req.Prefer) {
						selector, slotRelease = req.Prefer, titleVC.Release.ID
					}
					return out.resolution, nil
				}
			}
		}
	}

	showVC := s.viewerShowChoiceForResolve(req)

	// The owner's pin for this title, when it applies to this request
	// (pins.go). It is authoritative, including when it cannot be played,
	// unless a viewer's show-level episode choice matches a healthy candidate.
	// A preferred pin (Fallback) is tried the same way, but when it cannot be
	// used this request continues as ordinary Auto without it.
	pin, pinNote := s.pinForResolve(req)
	trace.PinStatus = pinNote
	if pin != nil && pin.Fallback && exclude[pin.Release.ID] {
		// This playback already tried the preference; do not record a
		// second outcome for it.
		trace.PinStatus = "skipped: preferred release already tried"
		failedPreference = pin.Release.ID
		pin = nil
	} else if pin != nil && pin.Fallback && !s.preferenceEligible(t.Key, pin.Release) {
		// Inside its failure memory: neither validate it again nor record a
		// new failure, so the original window is not extended.
		trace.PinStatus = "skipped: preferred release failed recently"
		failedPreference = pin.Release.ID
		ctx = context.WithValue(ctx, skipPreferenceKey{}, pin.Release.ID)
		pin = nil
	}
	pinID := ""
	resolveOwnerPin := func() (*core.Resolution, error) {
		pinID = pin.Release.ID
		trace.Pinned = pinID
		// A stored pin already has the complete source. Do not search, rescue,
		// race other releases or return alternates for automatic failover.
		sreq := s.numberedRequest(ctx, s.searchRequest(ctx, t), true)
		results := withRelease(nil, pin.Release)
		cands, _, err := s.rankCandidatesLocal(ctx, req, sreq, profile, results, &trace, pinID, false)
		if err != nil {
			trace.Err = err.Error()
			return nil, err
		}
		if autoVersion(req.Prefer) {
			selector, slotRelease = req.Prefer, pinID
		}
		req.Prefer, req.PreferredOnly = pinID, true
		out, err := s.resolvePreferred(ctx, req, pin, cands, &trace, 1)
		if err != nil || out.resolution == nil {
			reason := candidateReason(&trace, pinID)
			if reason == ReasonNeedsVideoTranscode && (err == nil || errors.Is(err, core.ErrNotAvailable)) {
				err = core.ErrVideoTranscodingDisabled
			} else if err == nil {
				err = core.ErrNotAvailable
			}
			trace.Err = fmt.Sprintf("pinned release %s cannot be played: %s", pinID, reason)
			return nil, fmt.Errorf("%s: %w", trace.Err, err)
		}
		return out.resolution, nil
	}
	// preferOwnerPin tries a preferred release exactly as resolveOwnerPin
	// tries a strict one. done reports an answer: the preference played,
	// the request was cancelled, or a warm-up stopped at a source only a
	// Play may open. Otherwise the outcome is on the pin and the request
	// continues, unchanged, as ordinary Auto that skips the preference.
	preferOwnerPin := func() (res *core.Resolution, err error, done bool) {
		origReq, origSelector, origSlot := req, selector, slotRelease
		res, err = resolveOwnerPin()
		if err == nil || ctx.Err() != nil || errors.Is(err, core.ErrForegroundOnly) {
			return res, err, true
		}
		id := pin.Release.ID
		reason := candidateReason(&trace, id)
		if trace.PinStatus != "" {
			reason = trace.PinStatus
		}
		req, selector, slotRelease = origReq, origSelector, origSlot
		trace.Err, trace.Pinned = "", ""
		trace.PinStatus = "fell back: " + reason
		failedPreference = id
		pin, pinID = nil, ""
		ctx = context.WithValue(ctx, skipPreferenceKey{}, id)
		return nil, nil, false
	}
	if pin != nil && (showVC == nil || s.profileMatchesRelease(showVC.Profile, pin.Release)) {
		if !pin.Fallback {
			return resolveOwnerPin()
		}
		if res, err, done := preferOwnerPin(); done {
			return res, err
		}
	}

	ctx, inconclusive := withUnverifiedPool(ctx)
	sreq := s.explicitNumberedRequest(ctx, s.searchRequest(ctx, t))
	forgotten := false
	if s.vault != nil {
		key := searchCacheKey(searchRequestFor(t))
		s.mu.Lock()
		if s.searchCache[key].forgotten {
			forgotten = true
			delete(s.searchCache, key)
		}
		s.mu.Unlock()
	}
	library, hit, retry, libErr := s.libraryCandidates(ctx, req, sreq, profile, fp, &trace, forgotten)
	if libErr != nil {
		trace.Err = libErr.Error()
		return nil, libErr
	}
	if hit != nil && (showVC == nil || s.profileMatchesRelease(showVC.Profile, hit.Release)) {
		return hit, nil
	}
	early := newSearchPreparation(ctx, s, req, sreq, profile, pinID)
	defer early.close()
	ctx = context.WithValue(ctx, searchPreparationKey{}, early)
	var discovered []core.Release
	if !retry && !forgotten && s.discovered != nil && pin == nil && showVC == nil && req.Prefer == "" && !hasExclude {
		discovered = filterOutPlaceholders(s.discovered(ctx, t))
		if len(discovered) > 0 {
			trace.Discovery = append(trace.Discovery, DiscoveryTrace{Path: "feed", Count: len(discovered)})
		}
		early.arrived(discovered)
		early.startFeed()

		// A locally verified feed answer can avoid a search. A slow feed
		// gets only 100ms head start, then keeps checking alongside search.
		if len(library) == 0 && len(discovered) > 0 && !early.disabled {
			deadline := time.NewTimer(100 * time.Millisecond)
			tick := time.NewTicker(5 * time.Millisecond)
		feedWait:
			for {
				if ready := early.readyResultsAfter(0); len(ready) > 0 {
					candidates, _, rankErr := s.rankCandidatesLocal(ctx, req, sreq, profile, ready, &trace, "", false)
					if rankErr == nil && len(candidates) > 0 && !candidates[0].fallback && !candidates[0].belowScore {
						early.attach(candidates)
						candidates = groupRepostCandidates(candidates)
						idx, payload, checkErr := s.validateCandidates(ctx, candidates, &trace, 0)
						if checkErr == nil && idx >= 0 {
							deadline.Stop()
							tick.Stop()
							trace.Winner = candidates[idx].rel.ID
							s.setAvailability(ctx, int64(t.Key), true, s.searchTTL())
							return s.resolution(t.Key, candidates, idx, payload, &trace), nil
						}
					}
					break feedWait
				}
				select {
				case <-deadline.C:
					break feedWait
				case <-ctx.Done():
					deadline.Stop()
					tick.Stop()
					return nil, ctx.Err()
				case <-tick.C:
				}
			}
			deadline.Stop()
			tick.Stop()
		}
	}

	// Nothing to search at all (no enabled indexer, or debrid as the only
	// source without a torrent indexer) is a setup problem: say so instead
	// of "not available", and record nothing about the title.
	if pin == nil && len(discovered) == 0 && len(library) == 0 {
		if err := s.missingSources(); err != nil {
			trace.Err = err.Error()
			return nil, err
		}
	}

	// Fast path: cached unavailable answer, unless this is a failover
	// request that already knows about specific releases, the viewer
	// picked a specific version, or the owner pinned a release.
	forceFresh := retry || forgotten
	if !forceFresh && !hasExclude && req.Prefer == "" && pin == nil && len(discovered) == 0 && len(library) == 0 {
		if e, ok := s.getAvailability(ctx, int64(t.Key)); ok && !e.available {
			// With cached-only off, a remembered unavailable answer older
			// than unavailableRecheck may predate a download that has since
			// become ready: skip the fast path and fall through to a fresh
			// search. With cached-only on, the long cooloff stands.
			if s.cachedOnly() || s.now().Sub(e.checked) <= unavailableRecheck {
				trace.Err = "cached unavailable (cooloff)"
				return nil, fmt.Errorf("%q: %w", t.Title, core.ErrNotAvailable)
			}
			// The ordinary search cache may still hold a long-lived
			// non-empty result whose releases were all rejected; serving
			// it would never re-ask the indexer. Bypass it too.
			forceFresh = true
		} else if !s.cachedOnly() {
			// The entry may already have expired under its own short TTL
			// (health-failed or fresh-release writes) while the search
			// cache still holds its rejected results. Peek at the last
			// unavailable check instead: within the window the remembered
			// answer still serves (without refreshing its checked time),
			// past it this Resolve rechecks with a fresh search.
			if checked, ok := s.lastUnavailableChecked(ctx, int64(t.Key)); ok {
				if s.now().Sub(checked) <= unavailableRecheck {
					trace.Err = "cached unavailable (cooloff)"
					return nil, fmt.Errorf("%q: %w", t.Title, core.ErrNotAvailable)
				}
				forceFresh = true
			}
		}
	}

	searchStart := time.Now()
	results, cached, searchErr := s.searchWithNumbering(ctx, &sreq, forceFresh)
	if os.Getenv("ETHER_TRACE_PLAYBACK") != "" {
		s.log.Info("playback trace: resolver.searchCached",
			"key", t.Key,
			"title", t.Title,
			"cached", cached,
			"results", len(results),
			"search_ms", time.Since(searchStart).Milliseconds(),
			"err", searchErr,
		)
	}
	path := "live-complete"
	if cached {
		path = "cache"
	} else if early.partial.Load() {
		path = "live-early"
	}
	if searchErr == nil {
		trace.Discovery = append(trace.Discovery, DiscoveryTrace{Path: path, Count: len(results)})
	}
	trace.SearchDuration = time.Since(searchStart)
	if searchErr != nil {
		if pin == nil && len(discovered) == 0 && len(library) == 0 {
			trace.Err = searchErr.Error()
			return nil, searchErr
		}
		// A pin carries its full release, so it can still play while
		// the indexers are failing; the search error stands otherwise.
		results = nil
	}
	s.rememberReleases(t, results)
	if s.searchObserved != nil {
		observed := append([]core.Release(nil), results...)
		s.persistBookkeeping(ctx, func(ctx context.Context) { s.searchObserved(ctx, t, observed) })
	}
	for _, r := range library {
		results = withRelease(results, r)
	}
	for _, r := range discovered {
		results = withRelease(results, r)
	}
	if pin != nil {
		results = withRelease(results, pin.Release)
	}

	rcfg := s.resolverCfg()
	if channelAvailabilityCheck(ctx) {
		rcfg.HealthRace = 1
		rcfg.OptimisticStart = false
	}
	cands, clientRejected, err := s.rankCandidates(ctx, req, sreq, profile, results, &trace, pinID)
	if err != nil {
		trace.Err = err.Error()
		return nil, err
	}
	// Show-level episode preference (e.g. "4K DV" or "1080p Remux") applies in
	// the Auto slot ahead of owner pins and owner quality rules. If no matching
	// healthy release exists, fall back silently.
	if showVC != nil {
		showPool := acceptableVariantAudio(clearIdentityVariants(cands))
		if s.resolverCfg().OfferHigherVersions {
			if relaxed, ok := relaxedVariantProfile(profile, req.Client); ok {
				if ranked, _, e := s.rankCandidatesLocal(ctx, req, sreq, relaxed, results, &trace, "", false); e == nil {
					showPool = acceptableVariantAudio(clearIdentityVariants(ranked))
				}
			}
		}
		if matched := s.matchShowChoiceCandidates(showVC, showPool); len(matched) > 0 {
			if idx, payload, verr := s.validateCandidates(ctx, matched, &trace, 0); verr == nil && idx >= 0 {
				trace.Winner = matched[idx].rel.ID
				s.setAvailability(ctx, int64(t.Key), true, s.searchTTL())
				if autoVersion(req.Prefer) {
					selector, slotRelease = req.Prefer, matched[idx].rel.ID
				}
				return s.resolution(t.Key, matched, idx, payload, &trace), nil
			}
		}
		if pin != nil {
			if !pin.Fallback {
				return resolveOwnerPin()
			}
			if res, err, done := preferOwnerPin(); done {
				return res, err
			}
			cands = slices.DeleteFunc(cands, func(c candidate) bool { return c.rel.ID == failedPreference })
		}
	}
	// Logical version selectors are resolved only after normal discovery and
	// policy gates. An authorized non-Auto slot tries all matching candidates
	// in that slot with Auto's failover machinery, stepping down only to lower
	// tiers the client allows and never up to Auto/4K.
	if slot, ok := parseVersionSlot(req.Prefer); ok {
		plan := s.configuredSlots(profile, req.Client, core.MaxVariants)
		selected := -1
		for i, configured := range plan {
			if configured == slot {
				selected = i
				break
			}
		}
		if slot.Kind != "auto" {
			if selected < 0 {
				if req.PreferredOnly {
					return nil, core.ErrNotAvailable
				}
				req.Prefer = ""
			} else {
				return s.resolveVersionSlot(ctx, slotResolveParams{
					slot:         slot,
					origPrefer:   req.Prefer,
					selected:     selected,
					plan:         plan,
					req:          &req,
					sreq:         sreq,
					profile:      profile,
					results:      results,
					cands:        cands,
					pinID:        pinID,
					searchErr:    searchErr,
					hasExclude:   hasExclude,
					inconclusive: inconclusive,
					trace:        &trace,
					rcfg:         rcfg,
					early:        early,
					selector:     &selector,
					slotRelease:  &slotRelease,
				})
			}
		} else {
			cands = pinFirst(acceptableVariantAudio(clearIdentityVariants(cands)), pinID)
			pool := cands
			autoReq := req
			autoReq.Prefer = ""
			titleVC := s.viewerTitleChoiceForResolve(autoReq, nil)
			showVC := s.viewerShowChoiceForResolve(autoReq)
			auto, _ := s.slotAutoWithSource(autoReq, cands, pool, titleVC, showVC)
			picks := slotPicks(plan, pool, auto, req, titleVC)
			if selected >= 0 && selected < len(picks) && picks[selected].ID != "" {
				selector, slotRelease = req.Prefer, picks[selected].ID
				slotAutoFulfilled = !req.PreferredOnly
				found := false
				for _, c := range cands {
					if c.rel.ID == slotRelease {
						found = true
						break
					}
				}
				if !found {
					for _, c := range pool {
						if c.rel.ID == slotRelease {
							cands = append(cands, c)
							break
						}
					}
				}
				req.Prefer = slotRelease
			} else {
				if req.PreferredOnly {
					return nil, core.ErrNotAvailable
				}
				req.Prefer = ""
			}
		}
	}

	// Speculative work (warm-ups, detail discovery) never runs a rescue: it
	// is Play's second pass. What speculative work did not find proves
	// nothing about the title, so it is not remembered unavailable either.
	if req.PreferredOnly && len(cands) == 0 {
		return nil, core.ErrNotAvailable
	}
	speculative := core.SpeculativeIndexerWork(ctx)
	reqLang := profileRequiredLanguage(profile)
	requiredLangRejected := reqLang != "" && traceHasRequiredProfileLanguageRejection(&trace)
	rescueTried, rescueSkipped := false, false
	// When no candidate matched a Required profile audio language (and releases
	// for this title were already found and rejected for that language), fail
	// fast instead of running rescue searches for a language that cannot match.
	if len(cands) == 0 && searchErr == nil && ctx.Err() == nil && (speculative || requiredLangRejected) {
		rescueSkipped = true
	} else if len(cands) == 0 && searchErr == nil && ctx.Err() == nil {
		rescueTried = true
		var rejected bool
		cands, results, rejected, err = s.rescueCandidates(ctx, req, sreq, profile, results, &trace, pinID)
		clientRejected = clientRejected || rejected
		if err != nil {
			trace.Err = err.Error()
			return nil, err
		}
	}

	failed := func(fallback error) error {
		if searchErr != nil {
			trace.Err = searchErr.Error()
			return searchErr
		}
		return fallback
	}

	if len(cands) == 0 {
		if pin != nil {
			s.notePinForClient(ctx, t.Key, pinID, PinRejected, candidateReason(&trace, pinID), clientDisplayName(req.Client))
			trace.PinStatus = "rejected: " + candidateReason(&trace, pinID)
		}
		// Client/profile restrictions and temporary source skips do not prove
		// that the title is unavailable for everyone.
		if !clientRejected && !rescueSkipped && !hasExclude && searchErr == nil && !coverage.Incomplete() {
			s.setAvailability(ctx, int64(t.Key), false, s.cooloffFor(t))
		}
		if isFreshRelease(t, s.now()) {
			skey := searchCacheKey(sreq)
			s.mu.Lock()
			if pe, ok := s.searchCache[skey]; ok {
				if capExp := s.now().Add(freshReleaseCooloff); pe.expires.After(capExp) {
					pe.expires = capExp
					s.searchCache[skey] = pe
				}
			}
			s.mu.Unlock()
		}
		if videoOff, _ := s.transcodeSettingsFor(req.Client); videoOff && traceHasRejectionReason(&trace, ReasonNeedsVideoTranscode) {
			trace.Err = "no release can play without a video transcode, and transcoding is off"
			return nil, failed(fmt.Errorf("%q: %w", t.Title, core.ErrVideoTranscodingDisabled))
		}
		if reqLang != "" && traceHasRequiredProfileLanguageRejection(&trace) {
			langErr := &core.RequiredAudioLanguageError{Language: releaseparse.LanguageDisplayName(reqLang)}
			trace.Err = langErr.Error()
			return nil, failed(fmt.Errorf("%q: %w", t.Title, langErr))
		}
		trace.Err = "no acceptable candidates"
		return nil, failed(fmt.Errorf("%q: %w", t.Title, core.ErrNotAvailable))
	}

	// A quality change may prefer a known HD source that saves decoding work.
	// This is a soft preference: never promote unknown/SD, ambiguous, starved,
	// wrong-audio or below-score sources, and never discard a healthy fallback.
	if req.Prefer == "" && pin == nil && req.Client.PreferredSourceHeight >= 720 {
		suitable := func(c candidate) bool {
			return c.rel.Info.Height >= 720 && c.rel.Info.Height <= req.Client.PreferredSourceHeight && !c.identityWeak && !c.identityConflict && !c.audioUnverified && !c.starved && !c.belowScore && c.langOK
		}
		sort.SliceStable(cands, func(i, j int) bool { return suitable(cands[i]) && !suitable(cands[j]) })
	}
	if req.Prefer == "" && pin == nil && req.Client.MaxBitrateKbps > 0 && !req.Client.DefaultRemoteBitrate {
		cleanHD := func(c candidate) bool {
			return c.rel.Info.Height >= 720 && !c.identityWeak && !c.identityConflict && !c.audioUnverified && !c.starved && !c.belowScore && c.langOK
		}
		fitsCap := func(c candidate) bool { return cleanHD(c) && c.bandwidth > 0 }
		if slices.ContainsFunc(cands, fitsCap) {
			sort.SliceStable(cands, func(i, j int) bool {
				return fitsCap(cands[i]) && !fitsCap(cands[j]) && cands[j].bandwidth <= 0
			})
		} else if req.Client.MaxBitrateKbps <= 12000 && !slices.ContainsFunc(cands, func(c candidate) bool { return c.bandwidth > 0 }) {
			hd1080 := func(c candidate) bool { return cleanHD(c) && c.rel.Info.Height <= 1080 }
			sort.SliceStable(cands, func(i, j int) bool {
				return hd1080(cands[i]) && !hd1080(cands[j]) && cands[j].rel.Info.Height > 1080
			})
		}
	}

	// Preserve normal ranking within each codec family, after policy gates.
	if req.PreferVideoCodec != "" {
		sort.SliceStable(cands, func(i, j int) bool {
			return strings.EqualFold(cands[i].rel.Info.VideoCodec, req.PreferVideoCodec) && !strings.EqualFold(cands[j].rel.Info.VideoCodec, req.PreferVideoCodec)
		})
	}
	if req.Prefer == "" && pin == nil {
		cands = s.demoteBackingOffUsenet(cands, &trace)
	}
	prioritizeOwnerTracks(cands)
	// resolvePreferred drops the preferred release from cands in place.
	var preferredCand *candidate
	if prefer := cmp.Or(req.Prefer, pinID); prefer != "" {
		if i := slices.IndexFunc(cands, func(c candidate) bool { return c.rel.ID == prefer }); i >= 0 {
			c := cands[i]
			preferredCand = &c
		}
	}
	preferred, err := s.resolvePreferred(ctx, req, pin, cands, &trace, rcfg.HealthRace)
	if err != nil {
		if req.PreferredOnly {
			return nil, err
		}
		if res := s.lastResort(ctx, inconclusive, t.Key, preferred.candidates, &trace); res != nil {
			return res, nil
		}
		if len(preferred.candidates) == 0 {
			return nil, failed(err)
		}
		return nil, err
	}
	if preferred.resolution != nil {
		return preferred.resolution, nil
	}
	// lead is the preferred release when it was tried: it heads the last
	// resort when no health check verifies.
	var lead []candidate
	if preferred.attempted && preferredCand != nil {
		lead = append(lead, *preferredCand)
	}
	cands = preferred.candidates
	early.attach(cands)
	optimisticErr := preferred.transient
	preferTried := preferred.attempted

	prepareStart := time.Now()
	var checks candidateChecks
	defer checks.close()
	cands = s.prepareSources(ctx, cands, req.Title, &trace, &checks)
	cands = groupRepostCandidates(cands, pinID, req.Prefer)
	trace.SourcePreparationDuration = time.Since(prepareStart)
	if rcfg.OptimisticStart && !preferTried && vault.KeyOf(cands[0].rel) == "" {
		if cands[0].rel.OpensOnDemand() && !core.OnDemandAllowedAt(ctx, cands[0].rel, 0) {
			// Nothing ranked below the add-on stream may stand in for it.
			trace.Err = errOnDemandForeground.Error()
			return nil, errOnDemandForeground
		}
		members := append([]candidate{cands[0]}, cands[0].fallbacks...)
		for mi, m := range members {
			if mi > 0 && (s.isBadRelease(m.rel) || s.recentlyFailedRelease(m.rel)) {
				continue
			}
			at := time.Now()
			if !reserveSpeculativeCheck(ctx) {
				return nil, errHealthBudget
			}
			nzb, err := s.indexer.FetchNZB(ctx, m.rel)
			trace.Candidates[m.trace].Attempted = true
			trace.Candidates[m.trace].FetchDuration = time.Since(at)
			if err == nil {
				winner := m
				winner.rel, _ = s.learnReleaseContent(ctx, winner.rel, nzb)
				cands[0] = winner
				trace.Winner = winner.rel.ID
				trace.Candidates[winner.trace].Reason = "optimistic start: health check pending"
				s.optimisticCheck(winner.rel, nzb)
				return s.resolution(t.Key, cands, 0, nzb, &trace), nil
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			trace.Candidates[m.trace].Rejected = true
			trace.Candidates[m.trace].Reason = "nzb fetch: " + err.Error()
			trace.Candidates[m.trace].err = err
			if permanentGrabFailure(err) {
				s.recordFailedValidation(ctx, m.rel, validation{err: err})
			} else if !permanentFailure(err) {
				optimisticErr = err
			}
		}
		cands = cands[1:]
	}
	// A drastic step down (to SD or an unknown resolution when 1080p or
	// better was on offer, here or in a release this failover excludes)
	// waits until everything comparable, a rescue search included, has
	// failed. Then it plays, and the trace says why.
	reference := s.referenceHeight(cands, req.Exclude)
	primary, downgrades := splitDowngrades(cands, reference)
	var primaryErr error
	primaryUncached := false
	if len(primary) > 0 {
		index, nzb, err := s.validateCandidates(ctx, primary, &trace, rcfg.HealthRace)
		if err != nil {
			if ctx.Err() == nil && !errors.Is(err, errOnDemandForeground) && onlyUncachedOrDead(primary, &trace) {
				primaryErr = err
				primaryUncached = true
			} else {
				if res := s.lastResort(ctx, inconclusive, t.Key, concat(lead, primary, downgrades), &trace); res != nil {
					return res, nil
				}
				// The viewer's own pick (lead) is the one sent to download.
				tried := concat(lead, primary)
				if i, payload, uerr, ok := s.uncachedOutcome(ctx, tried, results, &trace); ok {
					if i >= 0 {
						return s.acceptResolution(ctx, t.Key, tried, i, payload, &trace), nil
					}
					return nil, uerr
				}
				trace.Err = err.Error()
				return nil, err
			}
		} else if index >= 0 {
			return s.acceptResolution(ctx, t.Key, primary, index, nzb, &trace), nil
		}
	}

	var rescuePrimary []candidate
	// Retry discovery once after the initially ranked releases failed health.
	if !rescueTried && searchErr == nil && ctx.Err() == nil && core.SpeculativeIndexerWork(ctx) {
		rescueSkipped = true
	} else if !rescueTried && searchErr == nil && ctx.Err() == nil {
		rescueCands, updatedResults, rejected, rerr := s.rescueCandidates(ctx, req, sreq, profile, results, &trace, pinID)
		results = updatedResults
		clientRejected = clientRejected || rejected
		if rerr == nil && len(rescueCands) > 0 {
			var rescueChecks candidateChecks
			defer rescueChecks.close()
			at := time.Now()
			rescueCands = s.prepareSources(ctx, rescueCands, req.Title, &trace, &rescueChecks)
			rescueCands = groupRepostCandidates(rescueCands, pinID, req.Prefer)
			trace.SourcePreparationDuration += time.Since(at)
			var rescueDowngrades []candidate
			rescuePrimary, rescueDowngrades = splitDowngrades(rescueCands, reference)
			if len(rescuePrimary) > 0 {
				index, nzb, verr := s.validateCandidates(ctx, rescuePrimary, &trace, rcfg.HealthRace)
				if verr == nil && index >= 0 {
					return s.acceptResolution(ctx, t.Key, rescuePrimary, index, nzb, &trace), nil
				}
			}
			downgrades = append(downgrades, rescueDowngrades...)
			cands = append(cands, rescueCands...)
		}
	}
	if len(downgrades) > 0 && ctx.Err() == nil {
		sort.SliceStable(downgrades, func(i, j int) bool {
			return compareCandidates(downgrades[i], downgrades[j])
		})
		downgrades = groupRepostCandidates(downgrades, pinID, req.Prefer)
		index, nzb, verr := s.validateCandidates(ctx, downgrades, &trace, rcfg.HealthRace)
		if verr == nil && index >= 0 {
			trace.Downgrade = downgradeNote(reference, downgrades[index].rel, len(primary) > 0)
			s.log.Info("resolver: playing a lower quality", "title", t.Title, "release", downgrades[index].rel.Name, "reason", trace.Downgrade)
			return s.acceptResolution(ctx, t.Key, downgrades, index, nzb, &trace), nil
		}
	}

	// The resolver's own order: comparable quality (rescue included)
	// before any drastic downgrade, which are already sorted by quality.
	if res := s.lastResort(ctx, inconclusive, t.Key, concat(lead, primary, rescuePrimary, downgrades), &trace); res != nil {
		if res.QualityFallback == "" && slices.ContainsFunc(downgrades, func(c candidate) bool { return c.rel.ID == res.Release.ID }) {
			trace.Downgrade = downgradeNote(reference, res.Release, len(primary) > 0)
		}
		return res, nil
	}
	// Provider pacing is temporary and must not become an uncached or
	// unavailable title. Other providers and rescue candidates can still win.
	all := concat(lead, primary, rescuePrimary, downgrades)
	for _, c := range all {
		if err := trace.Candidates[c.trace].err; errors.Is(err, core.ErrDebridThrottled) {
			trace.Err = err.Error()
			return nil, err
		}
	}
	if optimisticErr != nil {
		return nil, optimisticErr
	}
	if errors.Is(searchErr, core.ErrStreamLimit) {
		return nil, searchErr
	}
	if searchErr != nil {
		return nil, failed(nil)
	}
	// Every release checked was a torrent no debrid account holds (or a
	// broken Usenet release): say so, send one to download when the owner
	// allows it, and do not record the title unavailable (a download may
	// finish, or the provider may cache it, at any time).
	if i, payload, uerr, ok := s.uncachedOutcome(ctx, all, results, &trace); ok {
		if i >= 0 {
			return s.acceptResolution(ctx, t.Key, all, i, payload, &trace), nil
		}
		return nil, uerr
	}
	if primaryUncached {
		tried := concat(lead, primary)
		if i, payload, uerr, ok := s.uncachedOutcome(ctx, tried, results, &trace); ok {
			if i >= 0 {
				return s.acceptResolution(ctx, t.Key, tried, i, payload, &trace), nil
			}
			return nil, uerr
		}
		if primaryErr != nil {
			trace.Err = primaryErr.Error()
			return nil, primaryErr
		}
	}
	// An occupied provider is temporary; keep the title available and expose
	// the actionable busy outcome instead of remembering a failed title.
	for _, c := range all {
		ct := &trace.Candidates[c.trace]
		if c.rel.StreamProvider != "" && errors.Is(ct.err, core.ErrStreamLimit) {
			trace.Err = "provider busy"
			return nil, fmt.Errorf("provider busy: %w", ct.err)
		}
	}
	// A capped warm-up did not check every candidate, so it proves nothing.
	totalMembers := countGroupedMembers(cands)
	if !clientRejected && !rescueSkipped && !hasExclude && totalMembers <= maxHealthAttempts && !(core.IsSpeculative(ctx) && totalMembers > speculativeHealthAttempts) && !coverage.Incomplete() {
		s.setAvailability(ctx, int64(t.Key), false, min(s.cooloff(), failedHealthTTL))
	}
	trace.Err = "no healthy candidates"
	for _, c := range all {
		ct := &trace.Candidates[c.trace]
		if ct.Attempted && ct.Rejected {
			return nil, fmt.Errorf("%q: %w: %w", t.Title, core.ErrNotAvailable, core.ErrSourceUnavailable)
		}
	}
	// Nothing playable was found and some releases were only ruled out for
	// needing a video transcode: say so, rather than a bare "not available".
	// A source that was tried and failed stays a temporary failure above.
	if videoOff, _ := s.transcodeSettingsFor(req.Client); videoOff && traceHasRejectionReason(&trace, ReasonNeedsVideoTranscode) {
		return nil, fmt.Errorf("%q: %w: %w", t.Title, core.ErrNotAvailable, core.ErrVideoTranscodingDisabled)
	}
	if reqLang != "" && traceHasRequiredProfileLanguageRejection(&trace) {
		langErr := &core.RequiredAudioLanguageError{Language: releaseparse.LanguageDisplayName(reqLang)}
		trace.Err = langErr.Error()
		return nil, fmt.Errorf("%q: %w", t.Title, langErr)
	}
	return nil, fmt.Errorf("%q: %w", t.Title, core.ErrNotAvailable)
}

// sourceInventory is implemented by indexers that know whether anything is
// configured to search (debrid.Indexer, newznab.Manager).
type sourceInventory interface {
	MissingSources() error
}

// missingSources reports core.ErrNoSources (or core.ErrNoTorrentSources)
// when the indexer can tell there is nothing to search.
func (s *Service) missingSources() error {
	if inv, ok := s.indexer.(sourceInventory); ok {
		return inv.MissingSources()
	}
	return nil
}

// titleOriginalLanguage is the catalogue field Title.Language.
// An unknown title language stays empty. Custom-format scoring may
// assume English for an untagged release; this comparison must not.
func titleOriginalLanguage(t *core.Title) string {
	if t == nil {
		return ""
	}
	return strings.TrimSpace(t.Language)
}

// searchCacheKey names a title's search. It leaves out the enrichment
// (anime cour mapping, owner-chosen episode group) so that Play, the
// console, the detail refill, the version picker and Forget, which do
// not all know it, share one entry; the entry keeps its request
// (searchCacheEntry.req) so an enriched caller can tell whether it
// matches (searchCacheEntry.serves). The alias-only numbering search is a different search.
func searchCacheKey(req core.SearchRequest) string {
	key := fmt.Sprintf("%s|%v|%s|%d|%d|%d|only:%t", req.Type, req.IDs, strings.ToLower(req.Title), req.Year, req.Season, req.Episode, req.ReleaseAliasesOnly)
	if req.EpisodeSource == core.MetadataTVDB {
		key += "|tvdb:" + req.Released.UTC().Format(time.DateOnly)
	}
	return key
}

// numberingEnrichment and courEnrichment are what a request adds to its
// title's plain request (searchRequestFor): release numbering aliases
// (an owner-chosen episode group, XEM) and the anime cour mapping. ""
// when the request has none.
func numberingEnrichment(req core.SearchRequest) string {
	aliases := core.EpisodeAliases(req)
	if len(aliases) == 0 && len(req.ReleasePackTargets) == 0 && !req.ReleaseNumberingKnown {
		return ""
	}
	return fmt.Sprintf("aliases:%v|packs:%v|known:%t", aliases, req.ReleasePackTargets, req.ReleaseNumberingKnown)
}

func courEnrichment(req core.SearchRequest) string {
	if req.Cour <= 0 && req.CourTVDBSeason <= 0 {
		return ""
	}
	return fmt.Sprintf("cour:%d:%d|tvdb:%d:%d", req.Cour, req.CourEpisode, req.CourTVDBSeason, req.CourTVDBEpisode)
}

// withSearchEnrichment copies from's enrichment onto req.
func withSearchEnrichment(req, from core.SearchRequest) core.SearchRequest {
	req.AltSeason, req.AltEpisode = from.AltSeason, from.AltEpisode
	req.ReleaseAliases, req.ReleasePackTargets, req.ReleaseNumberingKnown = from.ReleaseAliases, from.ReleasePackTargets, from.ReleaseNumberingKnown
	req.Cour, req.CourEpisode, req.CourTVDBSeason, req.CourTVDBEpisode = from.Cour, from.CourEpisode, from.CourTVDBSeason, from.CourTVDBEpisode
	return req
}

// cachedSearchRequest is t's plain search request with the enrichment of
// its cached search, so readers that cannot afford the cour lookup or the
// episode-group numbering judge the cached results as Play did.
func (s *Service) cachedSearchRequest(t *core.Title) core.SearchRequest {
	req := searchRequestFor(t)
	s.mu.RLock()
	e, ok := s.searchCache[searchCacheKey(req)]
	s.mu.RUnlock()
	if ok && !e.forgotten && s.now().Before(e.expires) {
		req = withSearchEnrichment(req, e.req)
	}
	return req
}

type rescueSearcher interface {
	SearchRescue(ctx context.Context, req core.SearchRequest) ([]core.Release, error)
}

func (s *Service) searchRescue(ctx context.Context, req core.SearchRequest) ([]core.Release, string) {
	rs, ok := s.indexer.(rescueSearcher)
	if !ok {
		return nil, ""
	}
	key := searchCacheKey(req) + "|rescue"
	s.mu.RLock()
	e, ok := s.searchCache[key]
	s.mu.RUnlock()
	if ok && s.now().Before(e.expires) && e.serves(req) {
		return append([]core.Release(nil), e.results...), "cache"
	}
	rctx, coverage := core.WithSearchCoverage(ctx)
	results, err := rs.SearchRescue(rctx, req)
	if coverage.Incomplete() {
		// An indexer was paused or did not answer: whatever came back
		// neither proves absence nor stands as the fleet's answer.
		core.NoteSearchIncomplete(ctx)
	}
	if err != nil {
		return nil, ""
	}
	results = s.normalizeSearchResults(req, results)
	if coverage.Incomplete() {
		return results, "live-complete"
	}
	s.mu.Lock()
	now := s.now()
	boundMap(s.searchCache, maxSearchCache, func(e searchCacheEntry) bool { return !now.Before(e.expires) })
	s.searchCache[key] = searchCacheEntry{results: append([]core.Release(nil), results...), expires: s.now().Add(s.searchTTL()), refreshed: s.now(), req: req}
	s.mu.Unlock()
	// Merge rescued results into the primary search cache entry so Play
	// Version (Variants) and subsequent resolves see the full candidate set.
	primaryKey := searchCacheKey(req)
	s.mu.Lock()
	if pe, ok := s.searchCache[primaryKey]; ok {
		extra := novelReleases(pe.results, results)
		if len(extra) > 0 {
			pe.results = append(append([]core.Release(nil), pe.results...), extra...)
			s.searchCache[primaryKey] = pe
		}
	}
	s.mu.Unlock()
	s.InvalidateDetailVariants()
	return results, "live-complete"
}

func (s *Service) searchSeasonPacks(ctx context.Context, req core.SearchRequest, fresh bool) []core.Release {
	rs, ok := s.indexer.(rescueSearcher)
	if !ok {
		return nil
	}
	key := searchCacheKey(req) + "|rescue"
	if !fresh {
		s.mu.RLock()
		e, ok := s.searchCache[key]
		s.mu.RUnlock()
		if ok && s.now().Before(e.expires) && e.serves(req) {
			return append([]core.Release(nil), e.results...)
		}
	}
	rctx, coverage := core.WithSearchCoverage(ctx)
	results, err := rs.SearchRescue(rctx, req)
	if err != nil {
		return nil
	}
	results = expandReleaseFallbacks(results)
	for i := range results {
		if results[i].Info.Title == "" && results[i].Info.Height == 0 && results[i].Info.Source == core.SourceUnknown {
			results[i].Info = releaseparse.Parse(results[i].Name)
		}
		results[i] = withTarget(results[i], req)
	}
	if !coverage.Incomplete() {
		s.mu.Lock()
		now := s.now()
		boundMap(s.searchCache, maxSearchCache, func(e searchCacheEntry) bool { return !now.Before(e.expires) })
		s.searchCache[key] = searchCacheEntry{results: append([]core.Release(nil), results...), expires: s.now().Add(s.searchTTL()), refreshed: s.now(), req: req}
		s.mu.Unlock()
		s.InvalidateDetailVariants()
	}
	return results
}

func novelReleases(existing, incoming []core.Release) []core.Release {
	seen := make(map[string]bool, len(existing))
	for _, r := range existing {
		seen[r.ID] = true
		if r.InfoHash != "" {
			seen["h:"+r.InfoHash] = true
		}
		if r.ContentID != "" {
			seen["c:"+r.ScopedContentID(r.ContentID)] = true
		}
	}
	var out []core.Release
	for _, r := range incoming {
		if seen[r.ID] || (r.InfoHash != "" && seen["h:"+r.InfoHash]) || (r.ContentID != "" && seen["c:"+r.ScopedContentID(r.ContentID)]) {
			continue
		}
		seen[r.ID] = true
		if r.InfoHash != "" {
			seen["h:"+r.InfoHash] = true
		}
		if r.ContentID != "" {
			seen["c:"+r.ScopedContentID(r.ContentID)] = true
		}
		out = append(out, r)
	}
	return out
}

// searchCached answers from the search cache unless fresh is set; cached
// reports whether the answer came from the cache.
func (s *Service) searchCached(ctx context.Context, req core.SearchRequest, fresh bool) (results []core.Release, cached bool, err error) {
	key := searchCacheKey(req)
	ls, _ := ctx.Value(liveSearchContextKey{}).(*liveSearch)
	// A detail search starts before playback preparation. Join it rather
	// than spending a second set of indexer requests for the same title.
	if !fresh && ctx.Value(refillSearchContextKey{}) == nil {
		s.bgMu.Lock()
		refill := s.refills[key]
		mark := s.refillRetryAfter[key]
		s.bgMu.Unlock()
		if refill.done != nil {
			// Source preparation can use live candidates from the refill.
			// Keep the same bound on waiting before starting its own search.
			var bound <-chan time.Time
			var early *searchPreparation
			if !core.SpeculativeIndexerWork(ctx) {
				t := time.NewTimer(playStartDeadline)
				defer t.Stop()
				bound = t.C
				early, _ = ctx.Value(searchPreparationKey{}).(*searchPreparation)
			}
			results, complete, err := refill.wait(ctx, req, early, bound)
			if err != nil || len(results) > 0 {
				return results, false, err
			}
			if complete {
				if results, ok := s.peekSearchCacheFor(req); ok {
					return results, true, nil
				}
				// A failed detail discovery is not a reason for speculative
				// playback preparation to immediately spend the quota again.
				if core.SpeculativeIndexerWork(ctx) {
					return nil, false, core.ErrBudgetExhausted
				}
			}
		} else if core.SpeculativeIndexerWork(ctx) && mark.missed && s.now().Before(mark.until) {
			// Discovery for this title found nothing this cooloff: speculative
			// playback preparation does not spend the quota asking again.
			return nil, false, core.ErrBudgetExhausted
		}
	}
	s.mu.RLock()
	e, ok := s.searchCache[key]
	s.mu.RUnlock()
	if ok && !fresh && s.now().Before(e.expires) && e.serves(req) {
		cachedRes := append([]core.Release(nil), e.results...)
		if ls != nil {
			s.finishLiveSearch(ls, cachedRes)
		}
		return cachedRes, true, nil
	}
	if !fresh && s.speculativeBudgetPaused(ctx) {
		if ls != nil {
			s.finishLiveSearch(ls, nil)
		}
		return nil, false, core.ErrBudgetExhausted
	}
	coverage := core.SearchCoverageFrom(ctx)
	if coverage == nil {
		ctx, coverage = core.WithSearchCoverage(ctx)
	}
	if ls == nil {
		ls = s.beginLiveSearch(req)
	}
	liveOwnedByBg := false
	defer func() {
		if !liveOwnedByBg {
			s.finishLiveSearch(ls, nil)
		}
	}()
	partial := false
	if early, ok := ctx.Value(searchPreparationKey{}).(*searchPreparation); ok && early != nil {
		if streaming, ok := s.indexer.(core.StreamingIndexer); ok {
			liveOwnedByBg = !early.disabled
			results, partial, err = early.searchLive(ctx, streaming, req, ls)
		} else {
			results, err = s.indexer.Search(ctx, req)
		}
	} else if streaming, ok := s.indexer.(core.StreamingIndexer); ok {
		results, err = streaming.SearchWith(ctx, req, func(batch []core.Release) {
			ls.add(s, batch)
		})
	} else {
		results, err = s.indexer.Search(ctx, req)
	}
	if err != nil {
		if errors.Is(err, core.ErrBudgetExhausted) {
			s.noteSpeculativeBudgetExhausted(ctx)
		}
		return nil, false, err
	}
	results = s.normalizeSearchResults(req, results)
	if partial || coverage.Incomplete() {
		// Never cache an incomplete fleet answer: an early Play start,
		// or an indexer that was paused or did not answer.
		if !liveOwnedByBg {
			s.finishLiveSearch(ls, results)
		}
		return results, false, nil
	}
	s.storeSearchCache(req, results)
	if !liveOwnedByBg {
		s.finishLiveSearch(ls, results)
	}
	return results, false, nil
}

// rememberSearchResults normalizes a completed fleet answer and writes the
// search cache. Used when a progressive Play start left discovery running.
func (s *Service) rememberSearchResults(req core.SearchRequest, results []core.Release) {
	results = s.normalizeSearchResults(req, results)
	s.storeSearchCache(req, results)
}

func filterOutPlaceholders(in []core.Release) []core.Release {
	hasPlaceholder := false
	for _, r := range in {
		if releaseparse.IsPlaceholderResult(r.Name) {
			hasPlaceholder = true
			break
		}
	}
	if !hasPlaceholder {
		return in
	}
	out := make([]core.Release, 0, len(in)-1)
	for _, r := range in {
		if !releaseparse.IsPlaceholderResult(r.Name) {
			out = append(out, r)
		}
	}
	return out
}

func (s *Service) normalizeSearchResults(req core.SearchRequest, results []core.Release) []core.Release {
	results = expandReleaseFallbacks(results)
	results = filterOutPlaceholders(results)
	for i := range results {
		if results[i].Info.Title == "" && results[i].Info.Height == 0 && results[i].Info.Source == core.SourceUnknown {
			results[i].Info = releaseparse.Parse(results[i].Name)
		}
		// Target torrents at the wanted episode here, once, so every
		// consumer (ranking, pins, the console's release memory, variants)
		// sees the same per-episode release ID.
		results[i] = withTarget(results[i], req)
	}
	return results
}

func (s *Service) storeSearchCache(req core.SearchRequest, results []core.Release) {
	key := searchCacheKey(req)
	s.mu.Lock()
	now := s.now()
	boundMap(s.searchCache, maxSearchCache, func(e searchCacheEntry) bool { return !now.Before(e.expires) })
	ttl := s.searchTTL()
	if len(results) == 0 && ttl > freshReleaseCooloff {
		ttl = freshReleaseCooloff
	}
	s.searchCache[key] = searchCacheEntry{results: append([]core.Release(nil), results...), expires: now.Add(ttl), refreshed: now, req: req}
	s.mu.Unlock()
	s.notifyLiveWaiters(key)
	s.InvalidateDetailVariants()
}

// rankCandidates is scoreCandidates with the owner's pinned release id
// (pinID, "" for none): the pin is judged by pinRejectReason's hard gates
// instead of the profile and identity rules, and is otherwise scored and
// traced like every other result. Interactive search reads the same trace.
func (s *Service) rankCandidates(ctx context.Context, req core.ResolveRequest, sreq core.SearchRequest, profile core.QualityProfile, results []core.Release, trace *Trace, pinID string) ([]candidate, bool, error) {
	return s.rankCandidatesWith(ctx, req, sreq, profile, results, trace, pinID, releaseTitleLastResortAllowed(ctx, req), releaseTitleWarmAllowed(ctx, req))
}

// rankCandidatesLocal ranks without synchronous last-resort metadata waits
// (for pre-search fast paths, incremental search arrival, detail UI and batch
// views). When allowWarm is true, releases rejected only on title mismatch may
// still queue async background verification.
func (s *Service) rankCandidatesLocal(ctx context.Context, req core.ResolveRequest, sreq core.SearchRequest, profile core.QualityProfile, results []core.Release, trace *Trace, pinID string, allowWarm bool) ([]candidate, bool, error) {
	return s.rankCandidatesWith(ctx, req, sreq, profile, results, trace, pinID, false, allowWarm && releaseTitleWarmAllowed(ctx, req))
}

func (s *Service) rankCandidatesWith(ctx context.Context, req core.ResolveRequest, sreq core.SearchRequest, profile core.QualityProfile, results []core.Release, trace *Trace, pinID string, allowSync, allowWarm bool) ([]candidate, bool, error) {
	policy, err := s.compileRules(profile.ReleaseRules)
	if err != nil {
		return nil, true, err
	}
	local := core.LocalResponse(ctx)
	env := rankEnv{policy: policy, allowReleaseTitleSync: allowSync && !local, allowReleaseTitleWarm: allowWarm && !local}
	if !local && usesPluginRanking(profile) {
		env.hot = s.rankingSession(ctx, req.Client)
		if env.hot != nil {
			defer env.hot.Close()
		}
	}
	env.exclude = map[string]bool{}
	for _, id := range req.Exclude {
		env.exclude[id] = true
	}
	env.excludeReleases = req.ExcludeReleases
	env.skipPreference, _ = ctx.Value(skipPreferenceKey{}).(string)
	env.rcfg = s.resolverCfg()
	env.lang = s.catalogLanguage()
	env.audio(profile, req)
	env.seaDex = s.seaDexForRank(ctx, sreq, !local && seaDexFetchAllowed(ctx, req))
	return s.rankWith(ctx, env, req, sreq, profile, results, trace, pinID)
}

// rankEnv is what ranking reads besides the results: the compiled rules,
// the hosted ranking session, resolver settings and audio languages. The
// feed evidence pass prepares it once and reuses it for every release.
type rankEnv struct {
	policy                *releasepolicy.Policy
	hot                   RankingSession
	seaDex                *seaDexEntry
	exclude               map[string]bool
	excludeReleases       []core.Release
	skipPreference        string // a preferred release this resolve already tried
	feedRejections        map[core.RatingKey][]core.Release
	rcfg                  config.Resolver
	lang                  string
	wanted, accepted      []string
	wantedSubs            []string
	allowReleaseTitleSync bool
	allowReleaseTitleWarm bool
}

// audio fills the request's wanted and accepted audio languages.
func (env *rankEnv) audio(profile core.QualityProfile, req core.ResolveRequest) {
	env.wanted = wantedAudio(profile, req.User, env.lang)
	env.accepted = acceptedAudio(profile, req.User, env.lang, req.Title)
	env.wantedSubs = wantedSubtitles(req.User, env.lang)
}

// rankWith is rankCandidates with its environment already prepared.
func (s *Service) rankWith(ctx context.Context, env rankEnv, req core.ResolveRequest, sreq core.SearchRequest, profile core.QualityProfile, results []core.Release, trace *Trace, pinID string) ([]candidate, bool, error) {
	if !core.LocalResponse(ctx) {
		s.prepareSeriesYears(ctx, req.Title, sreq, results)
		s.prepareTitleIDVerifications(ctx, req.Title, sreq, env.lang, results)
	}
	traceBase, traceRouting := 0, ""
	if trace != nil {
		traceBase = len(trace.Candidates)
		traceRouting = trace.Routing
	}
	cands, clientRejected, titleRej, err := s.rankPass(ctx, env, req, sreq, profile, results, trace, pinID)
	if err != nil {
		return nil, clientRejected, err
	}
	// Last resort only: run bounded independent TMDB verification when normal
	// ranking found nothing playable and candidates were rejected for title
	// mismatch alone. Plays with any acceptable candidate pay zero TMDB cost.
	if len(cands) == 0 && len(titleRej.playable) > 0 && env.allowReleaseTitleSync {
		if s.prepareReleaseTitlesLastResort(ctx, req.Title, sreq, titleRej.playable) {
			if trace != nil {
				trace.Candidates = trace.Candidates[:traceBase]
				trace.Routing = traceRouting
			}
			cands, clientRejected, titleRej, err = s.rankPass(ctx, env, req, sreq, profile, results, trace, pinID)
			if err != nil {
				return nil, clientRejected, err
			}
		}
	}
	if len(titleRej.identity) > 0 && env.allowReleaseTitleWarm {
		s.queueReleaseTitleWarm(req.Title, sreq, titleRej.identity)
	}
	return cands, clientRejected, nil
}

func onlyUncachedOrDead(cands []candidate, tr *Trace) bool {
	sawUncached := false
	for _, c := range cands {
		if c.trace < 0 || c.trace >= len(tr.Candidates) {
			continue
		}
		ct := &tr.Candidates[c.trace]
		if !ct.Attempted || !ct.Rejected {
			continue
		}
		switch {
		case ct.err != nil && errors.Is(ct.err, core.ErrNotCached) && protocolOf(c.rel) == core.ProtocolTorrent:
			sawUncached = true
		case ct.err == nil || permanentFailure(ct.err):
		default:
			return false
		}
	}
	return sawUncached
}

func (s *Service) rankRejected(env rankEnv, key core.RatingKey, rel core.Release) bool {
	if env.feedRejections != nil {
		return slices.ContainsFunc(env.feedRejections[key], func(old core.Release) bool { return nearRelease(old, rel) })
	}
	return s.rejectedFor(key, rel)
}

func (s *Service) rankPass(ctx context.Context, env rankEnv, req core.ResolveRequest, sreq core.SearchRequest, profile core.QualityProfile, results []core.Release, trace *Trace, pinID string) ([]candidate, bool, releaseTitleRejections, error) {
	var titleRej releaseTitleRejections
	t := req.Title
	policy, hot, exclude, rcfg, wanted, accepted := env.policy, env.hot, env.exclude, env.rcfg, env.wanted, env.accepted
	upscales := presumedUpscales(results, t.IsAnime())
	ids := make([]string, len(results))
	targeted := make([]core.Release, len(results))
	for i, r := range results {
		ids[i] = r.ID
		targeted[i] = withTarget(r, sreq)
	}
	verifiedIDs := s.verifiedReleases(ctx, targeted)
	var subtitles map[string]bool
	if rcfg.HardSubtitles != "allow" {
		subtitleIDs := ids
		if sreq.Type == core.TypeEpisode {
			// The candidate loop narrows season packs to episode identities before
			// checking subtitles. Read verdicts for those same identities.
			subtitleIDs = make([]string, len(results))
			for n, rel := range results {
				subtitleIDs[n] = withTarget(rel, sreq).ID
			}
		}
		subtitles = s.subtitleVerdicts(ctx, subtitleIDs)
	}
	// One instant ages every candidate. Reading the clock per release made
	// identical releases differ by the nanoseconds between their scores, so
	// ties broke on scoring order and clock resolution (a single tick on
	// Windows) instead of on the ranking.
	now := s.now()
	rc := s.rankContext(profile, t, rcfg, now)
	original := titleOriginalLanguage(t)
	preferOriginal := original != "" && releasepolicy.PrefersOriginalAudio(profile, req.User)
	preferSubs := prefersViewerSubtitles(profile, req.User)
	var cands []candidate
	clientRejected := false
	// For a known catalogue namesake, a corroborated result makes name-only
	// results ambiguous. Other series keep their ordinary quality fallbacks.
	// Check the whole result set before profile/exclusion gates: a failed or
	// lower-quality correct release must not cause failover to a namesake.
	corroborated := false
	identity := s.localEpisodeIdentity(ctx, t, sreq, results)
	namesake := sreq.Type == core.TypeEpisode && (identity.shared(sreq) || s.hasSeriesNamesake(ctx, t, sreq))
	if namesake {
		for _, rel := range results {
			rel = s.applyVerifiedTitleIDs(rel, t, sreq)
			if identity.reject(rel, t, sreq) == "" && identity.evidence(rel, t, sreq) != "" && rejectIdentityYears(rel, t, sreq, env.lang, func(y int) bool { return s.seriesYearAccepted(t.ShowKey, y) }) == "" {
				corroborated = true
				break
			}
		}
	}
	// Unknown height is not evidence that a source fits an explicit client
	// ceiling excluding every measured release. Owner profiles still permit
	// conservative SD fallbacks when their preferred quality is unavailable.
	aboveCeiling, withinCeiling := false, false
	if req.Client.ExplicitMaxHeight && req.Client.MaxHeight > 0 && profile.MaxHeight == req.Client.MaxHeight {
		for _, rel := range results {
			if rel.Info.Height > profile.MaxHeight {
				aboveCeiling = true
			} else if rel.Info.Height > 0 {
				withinCeiling = true
			}
		}
	}
	langActive := viewerLanguageActiveFor(wanted, profile, t)
	prefLang := profilePreferredLanguage(profile)
	var movieLangs []string
	var movieOrig string
	if sreq.Type == core.TypeMovie {
		movieLangs, movieOrig = movieTargetLanguages(profile, wanted, t)
	}
	for ri, rel := range results {
		if env.skipPreference != "" && rel.ID == env.skipPreference {
			// The preference attempt already traced this release.
			continue
		}
		rel = s.applyVerifiedTitleIDs(rel, t, sreq)
		rel = withTarget(rel, sreq)
		if sreq.Type == core.TypeMovie && rel.Target != nil {
			rel.Target.WantedLanguages = movieLangs
			rel.Target.OriginalLanguage = movieOrig
		}
		ct := CandidateTrace{IndexerID: rel.IndexerID, ReleaseID: rel.ID, Name: rel.Name, Indexer: rel.Indexer, Size: rel.Size, Height: rel.Info.Height}
		if rel.Target != nil {
			ct.PackID = rel.Target.PackID
		}
		if !s.ChannelSourceAllowed(sizedForEpisode(rel), req.Client, t) {
			clientRejected = true
			ct.Rejected, ct.Reason = true, "source exceeds channel delivery budget"
			if trace != nil {
				trace.Candidates = append(trace.Candidates, ct)
			}
			continue
		}
		pinned := pinID != "" && rel.ID == pinID
		ct.Pinned = pinned
		// Catalogue language is applied here, after the search cache.
		// The cache stores indexer results only; it does not freeze a
		// previous title's original language or a custom-format guess.
		policyContext := releasepolicy.Context{OriginalLanguage: titleOriginalLanguage(t)}
		if hot != nil {
			policyContext.ExternalMatch = func(r core.Release, c core.ReleaseCondition) (bool, bool) {
				return hot.Match(r, c, verifiedIDs[r.ID])
			}
		}
		preferred := pinned || (req.Prefer != "" && rel.ID == req.Prefer)
		probe, probed := s.probedMediaFor(rel)
		probedLangs := releasepolicy.ProbedAudioLanguages(probe.Streams)
		langRel := withProbedAudioLanguages(rel, probedLangs)
		policyResult := policy.Evaluate(langRel, policyContext)
		var unverifiedLangs []string
		audioUnverified := false
		if !pinned {
			unverifiedLangs, audioUnverified = classifyAudioVerification(policy, policyContext, &policyResult, profile, t, langRel)
		}
		hardSubbed := subtitles[rel.ID] || releaseparse.HardSubbed(rel.Name)
		if rcfg.HardSubtitles != "allow" && hardSubbed {
			policyResult.Fallback += 100
		}
		if len(profile.ReleaseRules) > 0 {
			ct.Preferences = &policyResult
		}
		var reason string
		identityReason := identity.reject(rel, t, sreq)
		identityEv := identity.evidence(rel, t, sreq)
		if excludedByRelease(env.excludeReleases, rel) {
			reason = "excluded (already tried)"
		} else if rcfg.HardSubtitles == "exclude" && hardSubbed {
			reason = "blocked hardcoded subtitles"
		} else if s.rankRejected(env, t.Key, rel) {
			reason = "release rejected for this title"
		} else if nonVideoRelease(rel, t, sreq) {
			reason = ReasonNotAVideoFile
		} else if !pinned && identityReason != "" {
			reason = identityReason
		} else if !pinned && namesake && identityEv == "" && episodeNamesakeConflict(rel, t, sreq) {
			reason = identityAmbiguousReason("conflicting namesake audio/region", sreq, identity.animeIdentifiesTarget(t, sreq), true)
		} else if !pinned && corroborated && releaseparse.EpisodeSeriesYear(rel.Name, rel.Info.Year) == 0 && identityEv == "" {
			reason = identityAmbiguousReason("shared series title (name only)", sreq, identity.animeIdentifiesTarget(t, sreq), true)
		} else if pinned {
			reason = s.pinRejectReason(rel, sreq, req.Client, exclude)
			if reason == "" {
				if videoOff, remoteCap := s.transcodeSettingsFor(req.Client); videoOff && releaseNeedsVideoTranscode(rel, t, req.Client, remoteCap) {
					reason = ReasonNeedsVideoTranscode
				}
			}
		} else if rel.Info.Height <= 0 && aboveCeiling && !withinCeiling {
			clientRejected = true
			reason = "unknown source resolution under a resolution limit"
		} else if policyResult.Rejected {
			reason = audioAbsentPolicyReason(policy, policyContext, langRel, policyResult)
		} else {
			videoOff, _ := s.transcodeSettingsFor(req.Client)
			reason = s.rejectReasonFor(rel, t, sreq, profile, rcfg, req.Client, exclude, preferred, req.User, probedLangs, env)
			if !videoOff && reason == "video codec unsupported by client" && req.ExactHEVCTranscode && !req.Client.Download && req.Prefer != "" && rel.ID == req.Prefer && strings.EqualFold(rel.Info.VideoCodec, "hevc") {
				// Only relax the codec gate for the explicitly selected source;
				// every other policy and the final probed output decision still apply.
				caps := req.Client
				caps.VideoCodecs = append(slices.Clone(caps.VideoCodecs), rel.Info.VideoCodec)
				reason = s.rejectReasonFor(rel, t, sreq, profile, rcfg, caps, exclude, preferred, req.User, probedLangs, env)
			}
			if strings.HasPrefix(reason, "audio is not in the profile's language (") {
				clientRejected = true
				reason = audioAbsentProfileReason(reason, profile)
			}
		}
		if reason != "" {
			if strings.HasPrefix(reason, "title mismatch") && s.releaseTitleIDs != nil && t != nil && sreq.IDs != (core.IDs{}) && (sreq.Type == core.TypeEpisode || sreq.Type == core.TypeMovie) {
				if s.titleMismatchIdentityOnly(rel, t, sreq, env.lang) {
					titleRej.identity = append(titleRej.identity, rel)
					if s.titleMismatchPolicyOK(rel, t, profile, rcfg, req, preferred) {
						titleRej.playable = append(titleRej.playable, rel)
					}
				}
			}
			if strings.HasPrefix(reason, "release preference:") || strings.HasPrefix(reason, "audio language absent:") || strings.HasPrefix(reason, "audio language unverified:") {
				clientRejected = true
			}
			ct.Rejected = true
			switch reason {
			case ReasonAboveProfileMaxResolution, ReasonAboveClientMaxResolution, ReasonBelowProfileMinResolution, ReasonNeedsVideoTranscode, "dolby vision not allowed for client", "video codec unsupported by client", "quality excluded by imported profile", "source recently failed health check; retrying later",
				"dub audio does not match the original or preferred languages",
				// Size ranges are per profile: a remux too large for a
				// download or mobile profile still plays for others.
				"size too small for runtime", "size too large for runtime":
				clientRejected = true
			case "cam not allowed", "screener not allowed":
				// Another profile may allow pre-release copies. When none
				// do, the title is unavailable for everyone.
				if s.profileAllowsCam() {
					clientRejected = true
				}
			}
			ct.Reason = reason
			if reason == ReasonBelowProfileMinResolution && rel.Info.Height == 0 && floorHeight(rel.Info) > 0 {
				ct.CompatibilityReason = fmt.Sprintf("height unknown; conservative %dp floor assumed", floorHeight(rel.Info))
			}
			if trace != nil {
				trace.Candidates = append(trace.Candidates, ct)
			}
			continue
		}
		langOK := audioAcceptable(langRel.Info, rel.SourceLanguage, accepted, t)
		var langMatch viewerLanguageMatch
		if langOK {
			langMatch = matchViewerLanguageFor(langRel.Info, rel.SourceLanguage, wanted, profile, t)
		}
		langRank := 0
		if langMatch.Matched {
			langRank = langMatch.Rank + 1
		}
		hasSubs := releaseHasWantedSubtitles(rel, probe, probed, env.wantedSubs)
		trackPreference := 0
		if preferOriginal {
			trackPreference = originalAudioPreference(langRel, t, original)
		}
		preferredSubtitles := langOK && preferSubs && hasSubs
		// Allow subtitle preference to beat an audio-only conversion. When
		// the source video itself needs encoding, leave the tradeoff to the
		// profile's format scores. Explicit original-audio rules still apply.
		if preferredSubtitles && !releaseNeedsVideoTranscode(rel, t, req.Client, 0) {
			trackPreference += wantedSubtitlePreference
		}
		lowBitrate := bitrateScore(sizedForEpisode(rel), t) < 0
		starved := lowBitrate || upscales[ri]
		yearGuessed := seriesYearGuessed(rel, sreq)
		facts := formatFacts{starved: starved, lowBitrate: lowBitrate, viewerLanguage: langMatch, originalLanguage: titleOriginalLanguage(t), viewerSubs: hasSubs, yearDiffers: yearGuessed, pastRetention: s.pastRetention(rel, now)}
		sc, err := s.scoreRelease(rel, profile, req.Client, t, policyResult, facts)
		if errors.Is(err, errFormatTimeout) {
			// One (possibly hostile) release name must not fail the whole
			// resolution; it only loses its own place in the ranking.
			clientRejected = true
			ct.Rejected = true
			ct.Reason = err.Error()
			if trace != nil {
				trace.Candidates = append(trace.Candidates, ct)
			}
			continue
		}
		if err != nil {
			return nil, clientRejected, titleRej, err
		}
		ct.CustomFormats = sc.matches
		// Imported packs use -10000 (TRaSH) or -999999 (Dictionarry)
		// for forbidden formats. They remain hard vetoes even if another
		// matching format would offset the penalty or the owner pins it.
		blocked := false
		for _, match := range sc.matches {
			if core.CountsTowardMinimum(match.ID) && match.Score <= -10000 {
				ct.Rejected, ct.Reason = true, "blocked custom format: "+match.Name
				blocked = true
				break
			}
		}
		if blocked {
			if trace != nil {
				trace.Candidates = append(trace.Candidates, ct)
			}
			continue
		}
		addonOrdered := rc.addonOrder[rel.IndexerID]
		addonPos := 0
		if addonOrdered {
			addonPos = rel.SourceOrder
			if addonPos <= 0 {
				addonPos = ri + 1
			}
		}
		belowScore := !addonOrdered && profile.MinCustomFormatScore != nil && sc.counted < *profile.MinCustomFormatScore
		belowQuality := !addonOrdered && belowPreferredQuality(rel.Info, profile)
		if belowScore {
			ct.MinimumFormatScore = profile.MinCustomFormatScore
			ct.Reason = fmt.Sprintf("Best available — below this profile's minimum (score %d of %d)", sc.counted, *profile.MinCustomFormatScore)
		} else if belowQuality {
			ct.Reason = "Best available — below this profile's preferred quality"
		}
		ct.CompatibilityReason = sc.compat
		if addonOrdered {
			ct.CustomFormats = nil
			rel.Score = 0
			ct.Score = 0
			if rel.Indexer != "" {
				ct.Route = fmt.Sprintf("add-on order #%d from %s", addonPos, rel.Indexer)
			} else {
				ct.Route = fmt.Sprintf("add-on order #%d", addonPos)
			}
		} else {
			rel.Score = float64(sc.total)
			ct.Score = rel.Score
		}
		verified := verifiedIDs[rel.ID]
		if verified {
			ct.CompatibilityReason = strings.TrimSpace(sc.compat + "; source recently verified playable")
			ct.CompatibilityReason = strings.TrimPrefix(ct.CompatibilityReason, "; ")
		}
		tr := -1
		if trace != nil {
			trace.Candidates = append(trace.Candidates, ct)
			tr = len(trace.Candidates) - 1
		}
		sized := sizedForEpisode(rel)
		bandwidth, delivery, bandwidthReason := s.deliveryFitReason(sized, req.Client, t)
		baseBandwidth := bandwidth
		var deliveryDemotion core.AutoSelectionReason
		if req.Client.Protocol == "vela" && !req.Client.Download && !req.Client.NoPlaybackTelemetry {
			baseBandwidth = sourceBandwidthFit(sized, req.Client, t)
			if bandwidth < 0 && baseBandwidth >= 0 {
				deliveryDemotion = core.AutoSourceDelivery
			}
			fit := applyClientDeliveryFit(bandwidth, releaseBitsPerSecond(sized, t), req.ClientDeliveryBudgetKbps)
			if fit < bandwidth {
				deliveryDemotion = core.AutoConnectionEstimate
			}
			bandwidth = fit
		}
		if trace != nil {
			note := func(n string) {
				trace.Candidates[tr].CompatibilityReason = strings.TrimPrefix(trace.Candidates[tr].CompatibilityReason+"; "+n, "; ")
			}
			if !addonOrdered {
				switch {
				case upscales[ri]:
					note("presumed upscale: ranked one resolution lower")
				case lowBitrate:
					note("too little bitrate for its resolution: ranked one resolution lower")
				}
			}
			if bandwidth < 0 && bandwidthReason != "" {
				note(bandwidthReason)
			}
			if !addonOrdered && yearGuessed {
				note("series year differs but names no other show: ranked after correctly-labelled releases")
			}
			if prefLang != "" && !addonOrdered {
				switch {
				case langMatch.Matched && langMatch.Rank == 0:
					note(fmt.Sprintf("preferred language (%s)", releaseparse.LanguageDisplayName(prefLang)))
				case !langOK:
					note("other language (fallback)")
				case !langMatch.Matched:
					if isBareDual(langRel.Info, rel.SourceLanguage, t) {
						note("bare DUAL names no dub language: other language (fallback)")
					} else {
						note("other language (fallback): original language")
					}
				}
			} else if !langOK {
				note("ranked after releases in an accepted audio language")
			} else if !addonOrdered && !langMatch.Matched && isBareDual(langRel.Info, rel.SourceLanguage, t) && langActive {
				note("bare DUAL names no dub language: ranked as original-language fallback")
			}
			if identityEv == AnimeReleaseStyleIdentityReason {
				note(AnimeReleaseStyleIdentityReason)
			} else if !pinned && identity.ambiguous(rel, t, sreq) {
				note(identityAmbiguousReason("shared series title (ranked after corroborated releases)", sreq, identity.animeIdentifiesTarget(t, sreq), true))
			}
			if audioUnverified {
				note(audioUnverifiedCompatNote(unverifiedLangs))
			}
		}
		keys := keysFor(rel, starved, rc)
		if addonOrdered && keys.addonOrder <= 0 {
			keys.addonOrder = addonPos
		}
		cf := sc.total
		if addonOrdered {
			cf = 0
		}
		cands = append(cands, candidate{localFirst: localFirst(rc, rel, policyResult.Fallback, langOK), identityWeak: !pinned && identity.ambiguous(rel, t, sreq), identityConflict: namesake && identityEv == "" && episodeRegionConflict(rel, t), audioUnverified: audioUnverified, unverifiedLangs: unverifiedLangs, fallback: belowQuality, belowScore: belowScore, counted: sc.counted, policy: policyResult, rel: rel, trace: tr, verified: verified, langOK: langOK, preferredSubtitles: preferredSubtitles, trackPreference: trackPreference, langRank: langRank, starved: starved, bandwidth: bandwidth, preDeliveryBandwidth: baseBandwidth, deliveryDemotion: deliveryDemotion, delivery: delivery, yearGuessed: yearGuessed, cf: cf, keys: keys})
	}
	if env.seaDex != nil && len(cands) > 0 {
		s.addSeaDexScores(env.seaDex, cands, trace)
	}
	if hot != nil && len(cands) > 0 {
		s.addPluginScores(hot, profile, cands, trace)
	}

	if req.Client.Protocol == "vela" && pinID == "" && core.IsAutoVersionSelector(req.Prefer) {
		markAutoDeliveryReasons(cands, t)
	}
	ordered := s.orderCandidates(cands, req.Title, trace)
	capCtx := releasepolicy.Context{OriginalLanguage: titleOriginalLanguage(t)}
	if hot != nil {
		capCtx.ExternalMatch = func(r core.Release, c core.ReleaseCondition) (bool, bool) {
			return hot.Match(r, c, verifiedIDs[r.ID])
		}
	}
	ordered, capped := capUnverifiedAudioCandidates(ordered, results, sreq, exclude, policy, capCtx, profile, t, trace)
	if capped {
		clientRejected = true
	}
	return ordered, clientRejected, titleRej, nil
}

func (s *Service) orderCandidates(cands []candidate, title *core.Title, trace *Trace) []candidate {
	// Rank strict matches in isolation so additional closest matches cannot
	// affect their existing sort or source routing, and keep unverified-audio
	// candidates strictly behind candidates with confirmed required audio.
	confirmed := make([]candidate, 0, len(cands))
	unverified := make([]candidate, 0)
	for _, c := range cands {
		if c.audioUnverified {
			unverified = append(unverified, c)
		} else {
			confirmed = append(confirmed, c)
		}
	}
	orderGroup := func(group []candidate) []candidate {
		if len(group) == 0 {
			return nil
		}
		local := make([]candidate, 0)
		strict := make([]candidate, 0, len(group))
		soft := make([]candidate, 0)
		for _, c := range group {
			switch {
			case c.localFirst:
				// A file the owner has leads even below the profile's
				// preferred quality or minimum score.
				local = append(local, c)
			case c.belowScore || c.fallback:
				soft = append(soft, c)
			default:
				strict = append(strict, c)
			}
		}
		local = s.orderPreferenceCandidates(local, title, trace)
		strict = s.orderPreferenceCandidates(strict, title, trace)
		soft = s.orderPreferenceCandidates(soft, title, trace)
		return append(append(local, strict...), soft...)
	}
	ordered := append(orderGroup(confirmed), orderGroup(unverified)...)
	ordered = s.demoteBackingOffUsenet(ordered, trace)
	prioritizeOwnerTracks(ordered)
	return ordered
}

func (s *Service) orderPreferenceCandidates(cands []candidate, title *core.Title, trace *Trace) []candidate {
	sort.SliceStable(cands, func(i, j int) bool {
		return compareCandidates(cands[i], cands[j])
	})
	cands = s.routeSources(cands, title, trace)
	// Source preferences may reorder quality peers, but cannot promote an
	// ambiguous namesake above evidence identifying the requested show.
	sort.SliceStable(cands, func(i, j int) bool { return !cands[i].identityWeak && cands[j].identityWeak })
	return cands
}

// rankContext gathers what the comparer keys read for one ranking pass.
func (s *Service) rankContext(profile core.QualityProfile, t *core.Title, rcfg config.Resolver, now time.Time) rankContext {
	s.cfgMu.RLock()
	scorer, priority := s.scorer, s.indexerPriority
	addonOrder, sourceBand, defaultBand := s.indexerAddonOrder, s.indexerSourceBand, s.defaultSourceBand
	s.cfgMu.RUnlock()
	return rankContext{
		profile:           profile,
		title:             t,
		now:               now,
		preferSource:      preferredSource(rcfg.SourcePreference),
		preferRevision:    !scorer.scoresRevision(profile.CustomFormatScores),
		preferLocal:       rcfg.PreferLocalFiles,
		priority:          priority,
		addonOrder:        addonOrder,
		sourceBand:        sourceBand,
		defaultSourceBand: defaultBand,
		reliability: func(indexer string) float64 {
			s.mu.RLock()
			defer s.mu.RUnlock()
			return s.reps[indexer].score()
		},
	}
}

// pastRetention reports a Usenet post older than the provider's retention.
func (s *Service) pastRetention(rel core.Release, now time.Time) bool {
	if s.retentionDays <= 0 || rel.Protocol != core.ProtocolUsenet || rel.PubDate.IsZero() {
		return false
	}
	return now.Sub(rel.PubDate).Hours()/24 > float64(s.retentionDays)
}

// addPluginScores runs the profile's plugin ranking steps over the
// candidates and adds each release's points to its custom format score. A
// step that did not finish for every candidate adds nothing.
func (s *Service) addPluginScores(hot RankingSession, profile core.QualityProfile, cands []candidate, trace *Trace) {
	releases := make([]core.Release, len(cands))
	verified := make([]bool, len(cands))
	for i := range cands {
		releases[i], verified[i] = cands[i].rel, cands[i].verified
	}
	steps := make([]string, 0, len(profile.PluginScores))
	for step := range profile.PluginScores {
		if core.IsPluginRankingStep(step) {
			steps = append(steps, step)
		}
	}
	slices.Sort(steps)
	for _, step := range steps {
		keys := hot.Rank(step, profile.PluginScores[step], releases, verified)
		if len(keys) != len(cands) {
			continue
		}
		for i, key := range keys {
			if cands[i].keys.addonOrder > 0 {
				continue
			}
			points := int(max(min(key, maxPluginPoints), -maxPluginPoints))
			if cands[i].pluginKeys == nil {
				cands[i].pluginKeys = map[string]int64{}
			}
			cands[i].pluginKeys[step] = key
			cands[i].cf += points
			cands[i].rel.Score += float64(points)
			if trace != nil && cands[i].trace >= 0 && cands[i].trace < len(trace.Candidates) && points != 0 {
				ct := &trace.Candidates[cands[i].trace]
				ct.Score = cands[i].rel.Score
				ct.CustomFormats = append(ct.CustomFormats, FormatMatch{ID: step, Name: step, Score: points})
			}
		}
	}
}

// maxPluginPoints bounds what one plugin step adds to a release's score.
const maxPluginPoints = 100000

// releaseScore is one release's custom format score and its parts.
type releaseScore struct {
	total   int // every matching format: ranks the release
	counted int // the owner's and imported formats: held to the minimum score
	matches []FormatMatch
	compat  string
}

// scoreRelease scores a release for a profile and client: the owner's and
// imported custom formats, Ether's built-in formats and the Prefer rules
// that matched, each at the profile's score.
func (s *Service) scoreRelease(rel core.Release, profile core.QualityProfile, caps core.ClientCaps, t *core.Title, policy releasepolicy.Result, facts formatFacts) (releaseScore, error) {
	s.cfgMu.RLock()
	scorer, scorerErr := s.scorer, s.scorerErr
	s.cfgMu.RUnlock()
	if scorerErr != nil {
		return releaseScore{}, scorerErr
	}
	// A torrent season pack's total size is not the episode's
	// (sizedForEpisode), so size formats see it as unknown.
	formatRel := sizedForEpisode(rel)
	counted, matches, err := scorer.ScoreForProfile(formatRel, profile.CustomFormatScores, t)
	if err != nil {
		return releaseScore{}, err
	}
	total := counted
	revisionScored := false
	for _, m := range matches {
		revisionScored = revisionScored || (m.revision && m.Score != 0)
	}
	if facts.originalLanguage == "" && t != nil {
		facts.originalLanguage = titleOriginalLanguage(t)
	}
	for _, id := range builtinFormatMatches(formatRel, profile, caps, facts) {
		if id == core.FormatRepackProper && revisionScored {
			// The profile scores revisions with its own Repack/Proper
			// format; the built-in one would count them twice.
			continue
		}
		score, scored := profile.CustomFormatScores[id]
		if id == core.FormatViewerLanguage && facts.viewerLanguage.ProfilePreferred != "" && (!scored || score == 0) {
			score, scored = defaultPreferredLanguageScore, true
		}
		if !scored || score == 0 {
			continue // a built-in format the profile doesn't use
		}
		name := BuiltinFormatName(id)
		if id == core.FormatViewerLanguage {
			score = viewerLanguageScore(score, facts.viewerLanguage.Rank)
			name = viewerLanguageFormatName(facts.viewerLanguage)
		}
		total += score
		matches = append(matches, FormatMatch{ID: id, Name: name, Score: score})
	}
	for _, r := range profile.ReleaseRules {
		if r.Action != "prefer" || r.Disabled || policy.PreferRule(r.ID) <= 0 {
			continue
		}
		id := core.RuleFormatPrefix + r.ID
		score := profile.CustomFormatScores[id]
		total += score
		matches = append(matches, FormatMatch{ID: id, Name: r.Name, Score: score})
	}
	return releaseScore{total: total, counted: counted, matches: matches, compat: compatibilityNote(formatRel, caps, t)}, nil
}

// rejectReason returns "" to accept, or the reason to drop a release.
// preferred (the viewer's explicit Play Version pick) may go below the
// profile floor (a 720p pick on a 1080p minimum). Releases above the
// ceiling — height, HDR policy, imported qualities, maximum size — stay
// rejected unless the owner has opted in to offering higher versions.
// Hard client gates and the identity/absolute-size checks always apply.
func (s *Service) rejectReason(rel core.Release, t *core.Title, sreq core.SearchRequest,
	profile core.QualityProfile, rcfg config.Resolver, caps core.ClientCaps, exclude map[string]bool, preferred bool) string {
	return s.rejectReasonFor(rel, t, sreq, profile, rcfg, caps, exclude, preferred, nil, s.probedAudioLanguagesFor(rel), rankEnv{})
}

func (s *Service) rejectReasonFor(rel core.Release, t *core.Title, sreq core.SearchRequest,
	profile core.QualityProfile, rcfg config.Resolver, caps core.ClientCaps, exclude map[string]bool, preferred bool, user *core.User, probedLangs []string, env rankEnv) string {
	// Usenet passwords are supplied by NZB metadata and validated by the engine.
	if rel.Password && rel.Protocol == core.ProtocolTorrent {
		return "passworded"
	}
	if exclude[rel.ID] {
		return "excluded (already tried)"
	}
	if s.rankRejected(env, t.Key, rel) {
		return "release rejected for this title"
	}
	if s.isBadRelease(rel) {
		return "marked bad"
	}
	if s.recentlyFailedRelease(rel) {
		return "source recently failed health check; retrying later"
	}
	lowerName := strings.ToLower(rel.Name)
	for _, term := range rcfg.BlockedTerms {
		if releaseparse.KeywordMatch(rel.Name, term) {
			return "blocked term: " + term
		}
	}
	for _, g := range rcfg.BlockedGroups {
		if g != "" && strings.EqualFold(rel.Info.Group, g) {
			return "blocked group: " + g
		}
	}
	if nonVideoRelease(rel, t, sreq) {
		return ReasonNotAVideoFile
	}
	var viewerLangs []string
	if user != nil {
		viewerLangs = user.AudioLanguages
	}
	if reason := sourceSuitabilityWithLanguages(rel, t, profile, preferred, probedLangs, viewerLangs); reason != "" {
		return reason
	}
	// Samples ("...Sample.mkv", "-sample" parts).
	for _, tok := range strings.FieldsFunc(lowerName, func(r rune) bool {
		return r == '.' || r == '-' || r == '_' || r == ' '
	}) {
		if tok == "sample" {
			return "sample"
		}
	}
	acceptYear := func(y int) bool { return t != nil && s.seriesYearAccepted(t.ShowKey, y) }
	if reason := rejectIdentityMetadata(rel, t, sreq, s.catalogLanguage(), acceptYear, s.metadataTitleMatches(rel, sreq)); reason != "" {
		return reason
	}
	videoOff, remoteCap := s.transcodeSettingsFor(caps)
	return rejectPolicyWithTranscode(rel, t, profile, caps, preferred, preferred && s.resolverCfg().OfferHigherVersions, videoOff, remoteCap)
}

// rejectIdentity checks the result's reported identity without client policy.
// catalogLang is the language catalogue episode titles are in ("en-US").
func rejectIdentity(rel core.Release, t *core.Title, sreq core.SearchRequest, catalogLang string) string {
	return rejectIdentityYears(rel, t, sreq, catalogLang, nil)
}

// rejectIdentityYears is rejectIdentity where acceptYear may accept a series
// year that differs from the show's (one that names no other show).
func rejectIdentityYears(rel core.Release, t *core.Title, sreq core.SearchRequest, catalogLang string, acceptYear func(int) bool) string {
	return rejectIdentityMetadata(rel, t, sreq, catalogLang, acceptYear, false)
}

// metadataMatch is independent metadata evidence, separate from reported IDs.
func rejectIdentityMetadata(rel core.Release, t *core.Title, sreq core.SearchRequest, catalogLang string, acceptYear func(int) bool, metadataMatch bool) string {
	if releaseparse.IsPlaceholderResult(rel.Name) {
		return "indexer placeholder"
	}
	isEpisode := sreq.Type == core.TypeEpisode
	if isEpisode && sreq.Season == 0 && !releaseparse.SpecialMatches(rel.Name, sreq.EpisodeTitle, sreq.SpecialTitles, specialShowTitles(sreq)) {
		return "special identity mismatch"
	}
	idMatch, idConflict := releaseparse.CompareTitleIDs(rel.ReportedTitleIDs(), sreq.IDs)
	if idConflict {
		return "indexer title identifier mismatch"
	}
	// An explicit contradiction remains unsafe even when an indexer reports
	// the requested ID; reported IDs can themselves be wrong.
	if !isEpisode {
		for _, director := range t.Directors {
			if releaseparse.DisclaimsDirector(rel.Name, director.Name) {
				return "director disclaimer"
			}
		}
	}
	// Title / obfuscation.
	wantTitle := sreq.Title
	miniseriesPart := 0
	if isEpisode && sreq.Season == 1 && len(rel.Info.Episodes) == 0 && !rel.Info.FullSeason {
		if partTitle, partNum, ok := releaseparse.MiniseriesPart(rel.Name); ok {
			if releaseparse.TitlesMatch(partTitle, wantTitle) ||
				(sreq.OriginalTitle != "" && releaseparse.TitlesMatch(partTitle, sreq.OriginalTitle)) ||
				(t.ShowOriginalTitle != "" && releaseparse.TitlesMatch(partTitle, t.ShowOriginalTitle)) ||
				(sreq.EnglishTitle != "" && releaseparse.TitlesMatch(partTitle, sreq.EnglishTitle)) ||
				(t.ShowEnglishTitle != "" && releaseparse.TitlesMatch(partTitle, t.ShowEnglishTitle)) {
				miniseriesPart = partNum
			}
		}
	}
	canRecoverTitle := titleMismatchRecoverable(rel.Info.Title, t, sreq)
	titleOK := (metadataMatch && canRecoverTitle) || miniseriesPart > 0 ||
		releaseparse.TitleMatches(rel.Info, wantTitle) ||
		(sreq.OriginalTitle != "" && releaseparse.TitleMatches(rel.Info, sreq.OriginalTitle)) ||
		(!isEpisode && t.OriginalTitle != "" && releaseparse.TitleMatches(rel.Info, t.OriginalTitle)) ||
		(isEpisode && t.ShowOriginalTitle != "" && releaseparse.TitleMatches(rel.Info, t.ShowOriginalTitle)) ||
		(sreq.EnglishTitle != "" && releaseparse.TitleMatches(rel.Info, sreq.EnglishTitle)) ||
		(!isEpisode && t.EnglishTitle != "" && releaseparse.TitleMatches(rel.Info, t.EnglishTitle)) ||
		(isEpisode && t.ShowEnglishTitle != "" && releaseparse.TitleMatches(rel.Info, t.ShowEnglishTitle)) ||
		matchesAlternativeTitle(rel.Info, sreq.AlternativeTitles) ||
		(!isEpisode && matchesAlternativeTitle(rel.Info, t.AlternativeTitles)) ||
		(isEpisode && matchesAlternativeTitle(rel.Info, t.ShowAlternativeTitles))
	if !titleOK {
		if stripped, ok := releaseparse.StripTrailingLanguage(rel.Info.Title); ok {
			for _, expected := range expectedTitlesFor(t, sreq) {
				if releaseparse.TitlesMatch(stripped, expected) {
					titleOK = true
					break
				}
			}
		}
	}
	// Korean and Asian multi-season variety releases often append the season
	// number directly to the show title before a bare episode or air date
	// ("World.Dice.Tour.4.E01.261017", "지구마불 세계여행4.E01").
	if !titleOK && isEpisode && sreq.Season >= 2 && len(rel.Info.Seasons) == 0 && (len(rel.Info.Episodes) > 0 || rel.Info.AirDate != "") {
		if stripped, ok := stripTrailingSeasonNumber(rel.Info.Title, sreq.Season); ok {
			for _, expected := range expectedTitlesFor(t, sreq) {
				if releaseparse.TitlesMatch(stripped, expected) {
					titleOK = true
					break
				}
			}
		}
	}
	if isEpisode && sreq.Season == 0 {
		prefix := core.ReleaseInfo{Title: releaseparse.SpecialSeriesTitle(rel.Name)}
		titleOK = titleOK || releaseparse.TitleMatches(prefix, wantTitle) ||
			(sreq.OriginalTitle != "" && releaseparse.TitleMatches(prefix, sreq.OriginalTitle)) ||
			(t.ShowOriginalTitle != "" && releaseparse.TitleMatches(prefix, t.ShowOriginalTitle)) ||
			(sreq.EnglishTitle != "" && releaseparse.TitleMatches(prefix, sreq.EnglishTitle)) ||
			(t.ShowEnglishTitle != "" && releaseparse.TitleMatches(prefix, t.ShowEnglishTitle)) ||
			matchesAlternativeTitle(prefix, sreq.AlternativeTitles) ||
			matchesAlternativeTitle(prefix, t.ShowAlternativeTitles)
	}
	if titleOK && !idMatch && regionTagConflicts(rel.Info.Title, wantTitle, t.Countries) {
		titleOK = false
	}
	if !titleOK && !(idMatch && canRecoverTitle) {
		obf := releaseparse.IsObfuscated(rel.Info.Title)
		if obf && !releaseparse.HasUsableInfo(rel.Info) {
			return "obfuscated, no usable info"
		}
		// Querying by ID does not prove that the indexer honored it.
		// Independent metadata or reported identifiers are needed to
		// establish an otherwise alternate title's identity.
		return formatTitleMismatch(rel.Info.Title, expectedTitlesFor(t, sreq))
	}
	if isEpisode {
		releaseYear := releaseparse.EpisodeSeriesYear(rel.Name, rel.Info.Year)
		if !releaseparse.SeriesYearMatches(releaseYear, sreq.Year) && !longRunningEpisodeYear(rel, t, sreq, releaseYear) && (acceptYear == nil || !acceptYear(releaseYear)) {
			return "series year mismatch"
		}
		if sreq.Season != 0 && releaseYear == 0 && !idMatch && !metadataMatch && !localizedEpisodeTitle(rel.Info, t, catalogLang) && !episodeTitleMatchesRelease(rel, sreq.EpisodeTitle) && !releaseparse.EpisodeTitleEvidence(rel.Name, sreq.EpisodeTitle) {
			return "episode title mismatch"
		}
		if rel.Info.FullSeason {
			// A pack streams the wanted episode's file (Release.Target):
			// through debrid for a torrent, from the episode's own NZB
			// files for Usenet. Other protocols cannot pick a file.
			if rel.Protocol != core.ProtocolTorrent && rel.Protocol != core.ProtocolUsenet {
				return "season pack (skipped in v1)"
			}
			_, packEpisode := releaseNumbers(rel.Info, sreq)
			if rel.Info.Cour > 0 && rel.Info.Cour == sreq.Cour && sreq.CourEpisode > 0 {
				packEpisode = sreq.CourEpisode
			}
			if !packCoversSeasonCour(rel.Info, sreq) ||
				(len(rel.Info.Episodes) > 0 && !slices.Contains(rel.Info.Episodes, packEpisode)) {
				return "season/episode mismatch"
			}
		} else if miniseriesPart > 0 {
			if miniseriesPart != sreq.Episode {
				return "season/episode mismatch"
			}
		} else if !(sreq.Season == 0 && len(rel.Info.Seasons) == 0 && len(rel.Info.Episodes) == 0 && len(rel.Info.AbsoluteEpisodes) == 0 && releaseparse.SpecialTitleEvidence(rel.Name, sreq.EpisodeTitle, specialShowTitles(sreq)...)) && !episodeMatchesRequest(rel.Info, sreq) {
			return "season/episode mismatch"
		}
	} else {
		if len(rel.Info.Episodes) > 0 || rel.Info.FullSeason {
			return "episode release for a movie request"
		}
		if !movieYearMatches(rel.Info.Year, sreq.Year, idMatch && titleOK) {
			return "year mismatch"
		}
	}
	return ""
}

func expectedTitlesFor(t *core.Title, sreq core.SearchRequest) []string {
	var out []string
	add := func(v string) {
		v = strings.TrimSpace(v)
		if v == "" {
			return
		}
		for _, existing := range out {
			if strings.EqualFold(existing, v) {
				return
			}
		}
		out = append(out, v)
	}
	add(sreq.Title)
	add(sreq.OriginalTitle)
	if t != nil {
		if sreq.Type == core.TypeEpisode {
			add(t.ShowOriginalTitle)
		} else {
			add(t.OriginalTitle)
		}
	}
	add(sreq.EnglishTitle)
	if t != nil {
		if sreq.Type == core.TypeEpisode {
			add(t.ShowEnglishTitle)
		} else {
			add(t.EnglishTitle)
		}
	}
	for _, alt := range sreq.AlternativeTitles {
		add(alt)
	}
	if t != nil {
		if sreq.Type == core.TypeEpisode {
			for _, alt := range t.ShowAlternativeTitles {
				add(alt)
			}
		} else {
			for _, alt := range t.AlternativeTitles {
				add(alt)
			}
		}
	}
	return out
}

func formatTitleMismatch(parsed string, expected []string) string {
	parsed = strings.TrimSpace(parsed)
	if parsed == "" && len(expected) == 0 {
		return "title mismatch"
	}
	quoted := make([]string, 0, min(len(expected), 3))
	for i, e := range expected {
		if i == 3 {
			break
		}
		quoted = append(quoted, fmt.Sprintf("%q", e))
	}
	switch {
	case parsed != "" && len(quoted) == 1:
		return fmt.Sprintf("title mismatch (parsed %q; expected %s)", parsed, quoted[0])
	case parsed != "" && len(quoted) > 1:
		return fmt.Sprintf("title mismatch (parsed %q; expected %s)", parsed, strings.Join(quoted[:len(quoted)-1], ", ")+" or "+quoted[len(quoted)-1])
	case parsed != "":
		return fmt.Sprintf("title mismatch (parsed %q)", parsed)
	default:
		return fmt.Sprintf("title mismatch (expected %s)", strings.Join(quoted, " or "))
	}
}

var reCourEpisodeMarker = regexp.MustCompile(`(?i)\b(?:S\d{1,2}|(?:Season|Saison|Stagione|Temporada)\s*\d{1,2}|[1-9]\d?(?:st|nd|rd|th)[\s._-]+Season)[\s._-]+(?:Part|Cour)[\s._-]*\d{1,2}(?:[\s._-]+-[\s._-]+|[\s._-]+E?)\d{1,3}(?:v\d)?\b`)

func episodeTitleMatchesRelease(rel core.Release, want string) bool {
	if rel.Info.Cour > 0 {
		if rel.Info.FullSeason {
			return true
		}
		name := reCourEpisodeMarker.ReplaceAllString(rel.Name, "S01E01")
		return releaseparse.EpisodeTitleMatches(name, want)
	}
	return releaseparse.EpisodeTitleMatches(rel.Name, want)
}

func courTVDBEpisodeMatch(info core.ReleaseInfo, sreq core.SearchRequest) bool {
	if sreq.CourTVDBSeason <= 0 || sreq.CourTVDBEpisode <= 0 || info.Cour > 0 || len(info.Seasons) == 0 {
		return false
	}
	if len(info.AbsoluteEpisodes) > 0 {
		if abs := absoluteEpisode(sreq); abs > 0 && !releaseparse.EpisodeMatchesAbsolute(info, abs) {
			return false
		}
	}
	return releaseparse.EpisodeMatches(info, sreq.CourTVDBSeason, sreq.CourTVDBEpisode)
}

func episodeMatchesRequest(info core.ReleaseInfo, sreq core.SearchRequest) bool {
	return releaseparse.EpisodeMatchesRequest(info, sreq)
}

func packCoversSeasonCour(info core.ReleaseInfo, sreq core.SearchRequest) bool {
	if _, _, ok := packNumbers(info, sreq); !ok {
		return false
	}
	if info.Cour > 1 && (sreq.Cour != info.Cour || sreq.CourEpisode <= 0) &&
		!releaseparse.SeasonNumberingUnderCour(info.Cour, info.CourRelative, sreq.Cour, sreq.Anime) {
		return false
	}
	if info.Cour == 1 && sreq.Cour > 1 {
		return false
	}
	return true
}

func preferenceReason(result releasepolicy.Result) string {
	for _, e := range result.Explanations {
		// A prefer rule can be unknown without rejecting. Only a
		// require or avoid that excluded the release, or could not
		// be judged, names the rejection.
		rejecting := e.Outcome == "exclude" || (e.Outcome == "unknown" && (e.Action == "require" || e.Action == "avoid"))
		if rejecting {
			return "release preference: " + e.Name + " (" + e.Outcome + ")"
		}
	}
	return "release preference: rejected"
}

// transcodeSettingsFor is transcodeSettings for one client: video transcoding
// is also off when the owner switched it off for this viewer.
func (s *Service) transcodeSettingsFor(caps core.ClientCaps) (videoOff bool, remoteCap int) {
	videoOff, remoteCap = s.transcodeSettings()
	return videoOff || caps.NoVideoTranscode, remoteCap
}

func (s *Service) transcodeSettings() (videoOff bool, remoteCap int) {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.videoTranscodingOff, s.remoteMaxBitrateKbps
}

func traceHasRejectionReason(trace *Trace, reason string) bool {
	if trace == nil {
		return false
	}
	for _, c := range trace.Candidates {
		if c.Rejected && c.Reason == reason {
			return true
		}
	}
	return false
}

// releaseNeedsVideoTranscode reports whether rel's parsed video characteristics
// require re-encoding video for caps. Container and audio mismatches are
// excluded because direct stream / remux and audio conversion remain allowed.
func releaseNeedsVideoTranscode(rel core.Release, t *core.Title, caps core.ClientCaps, remoteCap int) bool {
	hasClient := len(caps.VideoCodecs) > 0 || len(caps.Containers) > 0 || caps.MaxHeight > 0 ||
		caps.MaxBitrateKbps > 0 || len(caps.VideoMaxHeights) > 0 || caps.Product != "" ||
		caps.Platform != "" || caps.Protocol != "" || caps.ExplicitNoHDR
	if !hasClient {
		return false
	}
	info := rel.Info
	if info.VideoCodec != "" && len(caps.VideoCodecs) > 0 && !caps.SupportsVideo(info.VideoCodec) {
		return true
	}
	if caps.MaxHeight > 0 && info.Height > caps.MaxHeight {
		return true
	}
	if info.VideoCodec != "" && caps.VideoMaxHeights[info.VideoCodec] > 0 && info.Height > caps.VideoMaxHeights[info.VideoCodec] {
		return true
	}
	if info.DolbyVision && !caps.DolbyVision && !(info.HDR10 && caps.HDR10) {
		return true
	}
	if info.HDR10 && !info.DolbyVision && !caps.HDR10 {
		return true
	}
	capKbps := caps.MaxBitrateKbps
	if caps.Remote && remoteCap > 0 && (capKbps == 0 || remoteCap < capKbps) {
		capKbps = remoteCap
	}
	if capKbps > 0 && !info.FullSeason && t != nil && t.Runtime > 0 {
		if size := sizedForEpisode(rel).Size; size > 0 {
			estKbps := float64(size) * 8 / t.Runtime.Seconds() / 1000
			audioHeadroom := 0.0
			switch info.AudioCodec {
			case "truehd", "dtshd", "flac":
				audioHeadroom = 4000
			}
			if estKbps-audioHeadroom > float64(capKbps) {
				return true
			}
		}
	}
	return false
}

func rejectPolicyWithTranscode(rel core.Release, t *core.Title, profile core.QualityProfile, caps core.ClientCaps, preferred, ceilingBypass, videoOff bool, remoteCap int) string {
	if !ceilingBypass && profile.MaxHeight > 0 && rel.Info.Height > profile.MaxHeight {
		if configured := configuredMaxHeight(profile); caps.MaxHeight > 0 &&
			(configured == configuredUnlimited || caps.MaxHeight < configured) &&
			rel.Info.Height > caps.MaxHeight {
			if videoOff {
				return ReasonNeedsVideoTranscode
			}
			return ReasonAboveClientMaxResolution
		}
		return ReasonAboveProfileMaxResolution
	}
	if videoOff && releaseNeedsVideoTranscode(rel, t, caps, remoteCap) {
		return ReasonNeedsVideoTranscode
	}
	return rejectPolicy(rel, t, profile, caps, preferred, ceilingBypass)
}

// rejectPolicy applies client support and quality/size policy in rejection order.
// Release rules are judged once in rankCandidates; this does not compile them again.
// preferred lets an explicit pick go below the profile floor. ceilingBypass,
// set only when the owner opted in to higher versions, is what lets that
// pick exceed the ceiling. Codec support is never bypassed.
func rejectPolicy(rel core.Release, t *core.Title, profile core.QualityProfile, caps core.ClientCaps, preferred, ceilingBypass bool) string {
	if !ceilingBypass && profile.MaxHeight > 0 && rel.Info.Height > profile.MaxHeight {
		// The effective ceiling is min(configured profile, client caps):
		// name the strictly lower one as the limiter. An unlimited
		// configured profile leaves the client as the only limiter. A
		// tie keeps the profile wording, as before.
		if configured := configuredMaxHeight(profile); caps.MaxHeight > 0 &&
			(configured == configuredUnlimited || caps.MaxHeight < configured) &&
			rel.Info.Height > caps.MaxHeight {
			return ReasonAboveClientMaxResolution
		}
		return ReasonAboveProfileMaxResolution
	}
	if !ceilingBypass && rel.Info.DolbyVision && !profile.AllowDolbyVision && !(rel.Info.HDR10 && profile.AllowHDR) {
		return "dolby vision not allowed for client"
	}
	if rel.Info.VideoCodec != "" && len(caps.VideoCodecs) > 0 && !caps.SupportsVideo(rel.Info.VideoCodec) {
		if caps.Download || !isLegacyTranscodableCodec(rel.Info.VideoCodec) {
			return "video codec unsupported by client"
		}
	}
	// Size sanity against runtime. An explicit pick may be smaller than
	// the profile floor. The maximum is a ceiling: it holds unless the
	// owner opted in to higher versions. The absolute implausibility
	// floor always applies. A season pack's size is the whole season's,
	// so it is not checked.
	size := sizedForEpisode(rel).Size
	if size > 0 && size < 5<<20 {
		return "implausibly small"
	}
	if size > 0 && t.Runtime > 0 {
		mbPerMin := float64(size) / (1 << 20) / t.Runtime.Minutes()
		cfgMin, cfgMax := profile.SizeBoundsFor(t)
		minMBPerMin := effectiveMinSizeMBPerMin(cfgMin, rel.Info)
		if !preferred && minMBPerMin > 0 && mbPerMin < minMBPerMin {
			return "size too small for runtime"
		}
		if !ceilingBypass {
			if maxMBPerMin := effectiveMaxSizeMBPerMin(cfgMax, rel.Info); maxMBPerMin > 0 && mbPerMin > maxMBPerMin {
				return "size too large for runtime"
			}
		}
	}
	return ""
}

// movieYearMatches allows a ±2 year festival/theatrical gap when both the
// title and an explicit indexer-reported identifier (IMDb/TMDB) agree.
func movieYearMatches(got, want int, verifiedIdentity bool) bool {
	if releaseparse.YearMatches(got, want) {
		return true
	}
	if verifiedIdentity && got > 0 && want > 0 {
		d := got - want
		return d >= -2 && d <= 2
	}
	return false
}

// isLegacyTranscodableCodec reports codecs used by older DVD/TV/HD-DVD
// releases ("xvid", "mpeg2", "vc1") that modern client direct-play lists
// omit even though live playback transcodes them seamlessly.
func isLegacyTranscodableCodec(codec string) bool {
	switch codec {
	case "xvid", "mpeg2", "vc1":
		return true
	default:
		return false
	}
}

// floorHeight is the height a release is held to against a profile's
// minimum. Scene WEB releases name no resolution only when they are SD
// ("Title.2026.WEB-DL.H.264-GRP"), so an unlabelled WEB-DL or WEBRip counts
// as 480 and cannot pass a 4K floor while the 4K release is unplayable for
// the client. Other unlabelled releases (legacy HDTV and DVD rips, which may
// be all an older title has, and obfuscated names) stay exempt.
func floorHeight(info core.ReleaseInfo) int {
	if info.Height > 0 {
		return info.Height
	}
	if info.Source == core.SourceWEBDL || info.Source == core.SourceWEBRip {
		return 480
	}
	if info.Source == core.SourceUnknown && (info.VideoCodec == "xvid" || info.VideoCodec == "mpeg2") {
		return 480
	}
	return 0
}

// effectiveMinSizeMBPerMin scales a profile's size floor down for lower-resolution
// or high-efficiency legacy encodes so a 4K/1080p profile does not reject valid
// 480p/576p/720p DVDRip/PDTV/HEVC releases of older titles, while preserving the
// full floor for 1080p H.264 and 2160p releases.
func effectiveMinSizeMBPerMin(profileMin float64, info core.ReleaseInfo) float64 {
	if profileMin <= 0 {
		return 0
	}
	switch {
	case info.Height > 0 && info.Height <= 576:
		return min(profileMin, 1.2)
	case info.Source == core.SourceDVD || info.VideoCodec == "xvid" || info.VideoCodec == "mpeg2":
		return min(profileMin, 1.2)
	case info.Height == 720:
		return min(profileMin, 2.5)
	case info.Height == 1080 && (info.VideoCodec == "hevc" || info.VideoCodec == "av1"):
		return min(profileMin, 2.5)
	case info.Height == 1080:
		return min(profileMin, 5.0)
	case info.Height == 0:
		return min(profileMin, 1.5)
	default:
		return profileMin
	}
}

// regionTagConflicts reports whether a release's trailing regional tag
// ("The.Office.UK") conflicts with the catalogue title's known origin countries.
func regionTagConflicts(releaseTitle, wantTitle string, countries []string) bool {
	relRegion := releaseparse.TrailingRegionTag(releaseTitle)
	if relRegion == "" || releaseparse.TrailingRegionTag(wantTitle) == relRegion {
		return false
	}
	if wantRegion := releaseparse.TrailingRegionTag(wantTitle); wantRegion != "" && wantRegion != relRegion {
		return true
	}
	var known []string
	for _, c := range countries {
		switch strings.ToLower(strings.TrimSpace(c)) {
		case "us", "usa", "united states", "united states of america":
			known = append(known, "us")
		case "gb", "uk", "united kingdom", "great britain":
			known = append(known, "uk")
		case "au", "aus", "australia":
			known = append(known, "au")
		case "ca", "can", "canada":
			known = append(known, "ca")
		case "nz", "new zealand":
			known = append(known, "nz")
		}
	}
	if len(known) == 0 {
		return false
	}
	for _, k := range known {
		if k == relRegion {
			return false
		}
	}
	return true
}

// releaseNumbers keeps canonical single-episode numbers when they fit. Packs
// covering the chosen order use that order, including complete-series packs.
func releaseNumbers(info core.ReleaseInfo, req core.SearchRequest) (int, int) {
	// Without aliases, even a rejected pack retains its canonical target and
	// per-episode release ID. Coverage validation belongs to the ranking gate.
	if !req.ReleaseNumberingKnown && len(core.EpisodeAliases(req)) == 0 && (req.CourTVDBSeason <= 0 || req.CourTVDBEpisode <= 0) {
		return req.Season, req.Episode
	}
	if info.FullSeason {
		season, episode, ok := packNumbers(info, req)
		if !ok && !req.ReleaseNumberingKnown && len(core.EpisodeAliases(req)) == 0 {
			return req.Season, req.Episode
		}
		return season, episode
	}
	if !releaseparse.EpisodeMatchesNumbering(info, req.Season, req.Episode, absoluteEpisode(req)) && info.Cour == 0 {
		if courTVDBEpisodeMatch(info, req) {
			return req.CourTVDBSeason, req.CourTVDBEpisode
		}
		for _, n := range core.EpisodeAliases(req) {
			if releaseparse.EpisodeMatches(info, n.Season, n.Episode) {
				return n.Season, n.Episode
			}
		}
	}
	return req.Season, req.Episode
}

// A pack cannot select different files under two fitting numbering systems.
func packNumbers(info core.ReleaseInfo, req core.SearchRequest) (int, int, bool) {
	if info.Cour > 0 {
		if packCoversSeason(info, req.Season) {
			return req.Season, req.Episode, true
		}
		if req.CourTVDBSeason > 0 && req.CourTVDBEpisode > 0 && len(info.Seasons) > 0 && packCoversSeason(info, req.CourTVDBSeason) {
			return req.CourTVDBSeason, req.CourTVDBEpisode, true
		}
		return req.Season, req.Episode, false
	}
	if req.ReleaseNumberingKnown {
		chosen := core.EpisodeNumber{}
		for _, n := range req.ReleasePackTargets {
			if !packCoversSeason(info, n.Season) || (len(info.Episodes) > 0 && !slices.Contains(info.Episodes, n.Episode)) {
				continue
			}
			if chosen.Episode > 0 && chosen != n {
				return 0, 0, false
			}
			chosen = n
		}
		if chosen.Episode > 0 {
			return chosen.Season, chosen.Episode, true
		}
		if req.CourTVDBSeason > 0 && req.CourTVDBEpisode > 0 && len(info.Seasons) > 0 &&
			slices.Contains(info.Seasons, req.CourTVDBSeason) &&
			(len(info.Episodes) == 0 || slices.Contains(info.Episodes, req.CourTVDBEpisode)) {
			return req.CourTVDBSeason, req.CourTVDBEpisode, true
		}
		return 0, 0, false
	}
	for _, n := range core.EpisodeAliases(req) {
		if packCoversSeason(info, n.Season) && (len(info.Episodes) == 0 || slices.Contains(info.Episodes, n.Episode)) {
			return n.Season, n.Episode, true
		}
	}
	if req.CourTVDBSeason > 0 && req.CourTVDBEpisode > 0 &&
		(req.CourTVDBSeason != req.Season || req.CourTVDBEpisode != req.Episode) &&
		len(info.Seasons) > 0 && slices.Contains(info.Seasons, req.CourTVDBSeason) &&
		(len(info.Episodes) == 0 || slices.Contains(info.Episodes, req.CourTVDBEpisode)) {
		return req.CourTVDBSeason, req.CourTVDBEpisode, true
	}
	if packCoversSeason(info, req.Season) {
		return req.Season, req.Episode, true
	}
	return 0, 0, false
}

// withTarget marks a torrent candidate for an episode request with the
// wanted episode, so debrid streams that file of a multi-file torrent. A
// season pack gets a per-episode release ID: health, bad marks, pins and
// cached bytes then apply to the episode file, not the whole pack.
func withTarget(rel core.Release, sreq core.SearchRequest) core.Release {
	if sreq.Type == core.TypeMovie {
		target := core.ReleaseTarget{}
		if rel.Target != nil {
			target = *rel.Target
		}
		titles := []string{sreq.Title, sreq.OriginalTitle}
		if sreq.EnglishTitle != "" {
			titles = append(titles, sreq.EnglishTitle)
		}
		target.MovieTitles = append(titles, sreq.AlternativeTitles...)
		target.MovieYear = sreq.Year
		rel.Target = &target
		return rel
	}
	if sreq.Type != core.TypeEpisode || sreq.Episode <= 0 {
		return rel
	}
	targetSeason, targetEpisode := releaseNumbers(rel.Info, sreq)
	if rel.Info.FullSeason && targetEpisode <= 0 {
		return rel
	}
	aliases := core.EpisodeAliases(sreq)
	alternate := targetSeason != sreq.Season || targetEpisode != sreq.Episode
	// A Usenet season pack is narrowed to the episode's files when its NZB
	// is fetched; other non-torrent releases need no target unless aliases
	// apply at open-time file verification.
	if !alternate && len(aliases) == 0 && rel.Target == nil && rel.Protocol != core.ProtocolTorrent && !(rel.Protocol == core.ProtocolUsenet && rel.Info.FullSeason) {
		return rel
	}
	if rel.Target != nil {
		target := *rel.Target
		target.Aliases = aliases
		if alternate || len(aliases) > 0 {
			target.CanonicalSeason, target.CanonicalEpisode = sreq.Season, sreq.Episode
		}
		if alternate {
			target.Season, target.Episode = targetSeason, targetEpisode
			if target.PackID != "" {
				rel.ID = core.TargetReleaseID(target.PackID, target)
			}
		}
		target.ShowYear, target.EpisodeTitle = sreq.Year, sreq.EpisodeTitle
		if !sreq.Released.IsZero() {
			target.AirDate = sreq.Released.UTC().Format("2006-01-02")
		}
		target.FileName = rel.FileName
		if sreq.Season == 0 {
			target.ShowTitles, target.SpecialTitles = specialShowTitles(sreq), sreq.SpecialTitles
		}
		target.AbsoluteEpisode = releaseparse.AnimeSearchEpisode(sreq)
		target.Cour, target.CourEpisode, target.Anime = sreq.Cour, sreq.CourEpisode, sreq.Anime
		if alternate {
			target.Cour, target.CourEpisode = 0, 0
		}
		target.ExpectedIDs, target.ReportedIDs = sreq.IDs, rel.ReportedTitleIDs()
		rel.Target = &target
		return rel // enrich cached identities without deriving their ID again
	}
	var airDate string
	if !sreq.Released.IsZero() {
		airDate = sreq.Released.UTC().Format("2006-01-02")
	}
	target := core.ReleaseTarget{AbsoluteEpisode: releaseparse.AnimeSearchEpisode(sreq), Aliases: aliases, Cour: sreq.Cour, CourEpisode: sreq.CourEpisode, Anime: sreq.Anime, Season: targetSeason, Episode: targetEpisode, AirDate: airDate, ShowYear: sreq.Year, EpisodeTitle: sreq.EpisodeTitle, ExpectedIDs: sreq.IDs, ReportedIDs: rel.ReportedTitleIDs(), FileName: rel.FileName}
	if alternate || len(aliases) > 0 {
		target.CanonicalSeason, target.CanonicalEpisode = sreq.Season, sreq.Episode
	}
	if alternate {
		target.Cour, target.CourEpisode = 0, 0
	}
	if sreq.Season == 0 {
		target.ShowTitles, target.SpecialTitles = specialShowTitles(sreq), sreq.SpecialTitles
	}
	if rel.Info.FullSeason {
		target.PackID = rel.ID
		rel.ID = core.TargetReleaseID(rel.ID, target)
	}
	rel.Target = &target
	return rel
}

// retargetPack targets a season pack release at another episode in the season,
// deriving that episode's stable TargetReleaseID from the original pack ID.
func retargetPack(rel core.Release, sreq core.SearchRequest) core.Release {
	if !rel.Info.FullSeason {
		return rel
	}
	if rel.Target != nil && rel.Target.PackID != "" {
		rel.ID = rel.Target.PackID
	}
	// A matched file belongs to the original episode query, not every
	// episode covered by this pack. Retarget using the file selector.
	rel.FileName = ""
	rel.Target = nil
	return withTarget(rel, sreq)
}

// packIdentity returns a stable key grouping the same season pack across episodes.
func packIdentity(rel core.Release) string {
	if !rel.Info.FullSeason {
		return ""
	}
	if rel.Target != nil && rel.Target.PackID != "" {
		return rel.Target.PackID
	}
	if rel.InfoHash != "" {
		return "h:" + rel.InfoHash
	}
	if rel.GUID != "" {
		return rel.Indexer + "|" + rel.GUID
	}
	return rel.ID
}

// sizedForEpisode hides a season pack's total size from per-runtime size
// checks and bitrate estimates. A combined multi-episode file ("S01E01-E02")
// holds every episode it names, so its per-episode size is the total divided
// by that count, matching the combined runtime those checks compare against.
func sizedForEpisode(rel core.Release) core.Release {
	if rel.Info.FullSeason {
		rel.Size = 0
	} else if n := len(rel.Info.Episodes); n > 1 && rel.Size > 0 {
		rel.Size /= int64(n)
	}
	return rel
}

func packCoversSeason(info core.ReleaseInfo, season int) bool {
	if len(info.Seasons) == 0 {
		return true // "Complete series": the file chooser verifies the episode
	}
	for _, s := range info.Seasons {
		if s == season {
			return true
		}
	}
	return false
}

func fallbackHeight(info core.ReleaseInfo) int {
	if info.Height == 0 && info.Source == core.SourceDVD {
		return 480
	}
	return floorHeight(info)
}

// SourcesChanged invalidates remembered results when a database-backed source changes.
func (s *Service) SourcesChanged(ctx context.Context) { s.forgetSourceResults(ctx) }

// episodeIdentityEvidence distinguishes corroboration from absence of a
// contradiction. Query IDs and a generic/missing episode subtitle prove nothing.
func episodeIdentityEvidence(rel core.Release, req core.SearchRequest) bool {
	matched, conflict := releaseparse.CompareTitleIDs(rel.ReportedTitleIDs(), req.IDs)
	if conflict {
		return false
	}
	year := releaseparse.EpisodeSeriesYear(rel.Name, rel.Info.Year)
	return matched || (year > 0 && req.Year > 0 && releaseparse.SeriesYearMatches(year, req.Year)) ||
		releaseparse.EpisodeTitleEvidence(rel.Name, req.EpisodeTitle) ||
		releaseparse.EpisodeMatchesAirDate(rel.Info, req.Released)
}

func stripTrailingSeasonNumber(title string, season int) (string, bool) {
	if season < 2 {
		return "", false
	}
	suffix := strconv.Itoa(season)
	trimmed := strings.TrimSpace(title)
	if !strings.HasSuffix(trimmed, suffix) {
		return "", false
	}
	base := trimmed[:len(trimmed)-len(suffix)]
	if base == "" {
		return "", false
	}
	lastRune, _ := utf8.DecodeLastRuneInString(base)
	if lastRune == ' ' || lastRune == '-' {
		base = strings.TrimSpace(strings.TrimRight(base, "-"))
		return base, base != ""
	}
	if lastRune >= 128 && unicode.IsLetter(lastRune) {
		return strings.TrimSpace(base), true
	}
	return "", false
}

// languageConflict reports that a release names its audio languages and
// none of them is the title's original language (a dub or a different show).
// It uses the shared release-language parser, so no platform or language is
// special-cased. Multi-language releases never conflict.
func languageConflict(rel core.Release, t *core.Title) bool {
	if t == nil || t.Language == "" {
		return false
	}
	info := rel.Info
	if info.Title == "" {
		info = releaseparse.Parse(rel.Name)
	}
	named := false
	for _, l := range info.Languages {
		if l == releaseparse.LanguageMulti || l == releaseparse.LanguageDual {
			return false
		}
		named = true
		if releaseparse.LanguageMatch(l, t.Language) != releaseparse.LanguageNoMatch {
			return false
		}
	}
	return named
}

// episodeRegionConflict is language evidence used to break otherwise equal
// rankings for known namesakes.
func episodeRegionConflict(rel core.Release, t *core.Title) bool { return languageConflict(rel, t) }

// belowPreferredQuality is ranking evidence, never permission to exceed a ceiling.
func belowPreferredQuality(info core.ReleaseInfo, profile core.QualityProfile) bool {
	if h := floorHeight(info); profile.MinHeight > 0 && h > 0 && h < profile.MinHeight {
		return true
	}
	if len(profile.AllowedQualities) == 0 {
		return false
	}
	for _, quality := range profile.AllowedQualities {
		if qualityGateMatch(quality, info) {
			return false
		}
	}
	return true
}
