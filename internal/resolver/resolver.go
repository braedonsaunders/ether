package resolver

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/clusterbus"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/releaseparse"
	"github.com/braedonsaunders/ether/internal/releasepolicy"
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

// AnimeCour maps a multi-cour TMDB anime season episode to its 1-based cour
// (part) number and 1-based episode number within that cour.
type AnimeCour interface {
	LookupCour(ctx context.Context, req core.SearchRequest) (cour int, episode int, ok bool, err error)
}

// LibraryProfiles answers which library a title belongs to and the quality
// profile set on it (spec §419: a quality profile per library). A filter
// library wins over the built-in Movies/TV Shows library; the lowest
// library id breaks a tie between filter libraries. An empty name means the
// library set no profile, so today's home/away defaults apply.
type LibraryProfiles interface {
	QualityProfileForTitle(ctx context.Context, t *core.Title) string
}

// Options tunes a Service. Zero values get sensible defaults.
type Options struct {
	SearchObserved func(context.Context, *core.Title, []core.Release)
	Discovered     func(context.Context, *core.Title) []core.Release
	Observed       func(context.Context, *core.Title, core.Release)
	Invalidated    func(context.Context, string)
	// SeriesYearConflict reports whether the catalogue holds a different
	// show titled title that first aired in year (see series_year.go).
	SeriesYearConflict func(ctx context.Context, show core.RatingKey, title string, year int) (bool, error)
	// AnimeCour maps multi-cour TMDB anime season episodes to their cour
	// and cour-relative episode number; nil disables cour mapping.
	AnimeCour AnimeCour
	// LibraryProfiles resolves a title's per-library quality profile; nil
	// leaves selection to the existing home/away defaults.
	LibraryProfiles LibraryProfiles
	Logger          *slog.Logger
	Now             func() time.Time
	MaxTraces       int
	RetentionDays   int // posts older than this are penalised; 0 = no extra penalty
	// HealthTTL is how long a passed health check is trusted before the
	// release is sampled again (default 6h; negative disables the cache).
	HealthTTL time.Duration
	// HistoryMax bounds the persisted resolution history (default 5000
	// rows); HistoryMaxAge drops older rows (default 60 days).
	HistoryMax    int
	HistoryMaxAge time.Duration
}

// CandidateTrace explains one candidate's fate in a resolution.
type CandidateTrace struct {
	Preferences         *releasepolicy.Result `json:"preferences,omitempty"`
	ReleaseID           string                `json:"releaseId"`
	Name                string                `json:"name"`
	Indexer             string                `json:"indexer"`
	Score               float64               `json:"score"`
	CustomFormats       []FormatMatch         `json:"customFormats,omitempty"`
	CompatibilityReason string                `json:"compatibilityReason,omitempty"`
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
	Discovery                 []DiscoveryTrace `json:"discovery,omitempty"`
	Limits                    *TraceLimits     `json:"limits,omitempty"`
	TitleKey                  core.RatingKey   `json:"titleKey"`
	TitleName                 string           `json:"titleName"`
	Profile                   string           `json:"profile"`
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
	results []core.Release
	expires time.Time
}

type inflight struct {
	done      chan struct{}
	res       *core.Resolution
	err       error
	abandoned bool // the leader's own context ended the resolution
}

// Service implements core.Resolver.
type Service struct {
	years                     seriesYears
	seriesYearConflict        func(ctx context.Context, show core.RatingKey, title string, year int) (bool, error)
	animeCour                 AnimeCour
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
	indexer        core.Indexer
	health         core.HealthChecker
	db             *sql.DB
	log            *slog.Logger
	now            func() time.Time

	cfgMu            sync.RWMutex
	rcfg             config.Resolver
	quality          config.Quality
	catalogLang      string // catalogue metadata language ("en-US"): the server's language
	debridCachedOnly bool
	scorer           *FormatScorer
	indexerPriority  map[string]int // indexer id → Priority (lower ranks first)
	scorerErr        error
	sources          string // sourcesKey of the applied config

	mu            sync.RWMutex
	bad           map[string]badMark // release id -> blocklist mark
	reps          map[string]reputation
	avail         map[int64]availEntry
	searchCache   map[string]searchCacheEntry
	retentionDays int
	verified      map[string]time.Time // release id -> last passed health check
	verifiedMiss  map[string]time.Time // release id -> when resolver_verified last had no fresh check
	failedHealth  map[string]time.Time // confirmed source failures, briefly skipped on subsequent resolves
	healthTTL     time.Duration

	tracesMu  sync.Mutex
	traces    []Trace
	maxTraces int

	bgCtx            context.Context
	bgCancel         context.CancelFunc
	bgMu             sync.Mutex
	bgClosed         bool
	bgWG             sync.WaitGroup
	refills          map[string]chan struct{} // episode search key -> completion; guarded by bgMu
	refillRetryAfter map[string]refillMark    // next detail discovery allowed; guarded by bgMu

	flightMu sync.Mutex
	flights  map[string]*inflight

	delivery deliveryState

	route routingState // source routing: startup measurements and signals

	con consoleState // pins, release memory, health results, history (console.go)

	extend extendState // descriptor registry for ranking/rules (extend_registry.go)

	invalMu sync.Mutex
	inval   *clusterbus.Invalidator // nil on a standalone server
}

var _ core.Resolver = (*Service)(nil)

// New builds a resolver Service. indexer is the aggregate indexer
// (typically *newznab.Manager); health may be nil (health checks are
// then skipped); db may be nil (state then lives in memory).
func New(indexer core.Indexer, health core.HealthChecker, db *sql.DB, cfg config.Config, opts Options) *Service {
	s := &Service{seriesYearConflict: opts.SeriesYearConflict, animeCour: opts.AnimeCour, libProfiles: opts.LibraryProfiles, searchObserved: opts.SearchObserved, discovered: opts.Discovered, observed: opts.Observed, invalidated: opts.Invalidated,
		indexer: indexer, health: health, db: db,
		bad: map[string]badMark{}, reps: map[string]reputation{},
		avail: map[int64]availEntry{}, searchCache: map[string]searchCacheEntry{},
		flights:       map[string]*inflight{},
		retentionDays: opts.RetentionDays,
		maxTraces:     opts.MaxTraces,
		verified:      map[string]time.Time{},
		healthTTL:     opts.HealthTTL,
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
	s.loadPersisted(context.Background())
	return s
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
	s.scorer, s.scorerErr = NewFormatScorer(cfg.Resolver.CustomFormats)
	s.indexerPriority = make(map[string]int, len(cfg.Indexers))
	for _, ix := range cfg.Indexers {
		s.indexerPriority[ix.ID] = ix.Priority
	}
	changed := s.sources != "" && s.sources != key
	s.sources = key
	s.cfgMu.Unlock()
	if changed {
		s.forgetSourceResults(context.Background())
	}
}

// sourcesKey fingerprints the settings that decide which releases a search
// can find and which of them can play.
func sourcesKey(cfg config.Config) string {
	var b strings.Builder
	for _, ix := range cfg.Indexers {
		fmt.Fprintf(&b, "i|%s|%t|%s|%s|%s\n", ix.ID, ix.Enabled, ix.URL, ix.Kind, ix.Protocol)
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
	s.mu.Unlock()
	if s.db != nil {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM resolver_availability WHERE available = 0`); err != nil {
			s.log.Warn("forget unavailable titles", "err", err)
		}
	}
	s.log.Info("sources changed: cleared cached searches and unavailable titles")
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
	s.mu.Lock()
	m := badMark{reason: reason, at: s.now()}
	if prev, ok := s.bad[releaseID]; ok && prev.manual {
		// An automatic mark never downgrades an owner block (persistBad
		// keeps the owner's reason too); it only refreshes the clock.
		m.reason, m.manual = prev.reason, true
	}
	s.bad[releaseID] = m
	s.mu.Unlock()
	s.forgetVerified(ctx, releaseID)
	s.notePinsBlocked(ctx, releaseID, reason)
	return s.persistBad(ctx, s.blockEntryFor(releaseID, reason, false))
}

func (s *Service) isBad(id string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.bad[id]
	return ok && m.live(s.now())
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
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.bad[id]
	if !ok || !m.live(s.now()) {
		return "", false
	}
	return m.reason, true
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
	season, episode := t.Season, t.Episode
	if t.Key.Kind() == core.KindEpisode {
		if season == 0 {
			season = t.Key.Season()
		}
		if episode == 0 {
			episode = t.Key.Episode()
		}
	}
	if t.Type == core.TypeEpisode || t.Key.Kind() == core.KindEpisode {
		ids := t.ShowIDs
		// Episode external IDs describe the episode, not its series.
		// The rating key provides a canonical series TMDB ID even when
		// the cached ShowIDs only contain an IMDb or TVDB identifier.
		if ids.TMDB == 0 && t.Key.Kind() == core.KindEpisode {
			ids.TMDB = t.Key.TMDB()
		}
		title := t.ShowTitle
		if title == "" {
			title = t.Title
		}
		return core.SearchRequest{Type: core.TypeEpisode, IDs: ids, Title: title, OriginalTitle: t.ShowOriginalTitle, Year: t.ShowYear, Season: season, Episode: episode, EpisodeTitle: t.Title,
			AbsoluteEpisode: t.AbsoluteEpisode, Anime: isAnime(t), AlternativeTitles: t.ShowAlternativeTitles, Released: t.Released}
	}
	return core.SearchRequest{Type: core.TypeMovie, IDs: t.IDs, Title: t.Title, OriginalTitle: t.OriginalTitle, Year: t.Year, Anime: isAnime(t), AlternativeTitles: t.AlternativeTitles, Released: t.Released}
}

func (s *Service) searchRequest(ctx context.Context, t *core.Title) core.SearchRequest {
	req := searchRequestFor(t)
	if s != nil && s.animeCour != nil && req.Anime && req.Type == core.TypeEpisode && req.Season > 0 && req.Episode > 1 && req.IDs.TMDB > 0 {
		if cour, ep, ok, err := s.animeCour.LookupCour(ctx, req); err == nil && ok {
			req.Cour, req.CourEpisode = cour, ep
		}
	}
	return req
}

// isAnime reports a title whose releases follow anime conventions
// (absolute episode numbers, fansub groups): TMDB's "anime" keyword, or
// Japanese-language animation.
func isAnime(t *core.Title) bool {
	for _, k := range t.Keywords {
		if strings.EqualFold(k, "anime") {
			return true
		}
	}
	if !strings.EqualFold(t.Language, "ja") {
		return false
	}
	for _, g := range t.Genres {
		if strings.EqualFold(g, "Animation") {
			return true
		}
	}
	return false
}

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

func flightKey(req core.ResolveRequest, profile core.QualityProfile) string {
	ex := append([]string(nil), req.Exclude...)
	sort.Strings(ex)
	capsJSON, _ := json.Marshal(req.Client)
	profileJSON, _ := json.Marshal(profile)
	// Without profile languages the viewer's own audio languages decide
	// which releases are acceptable (acceptedAudio), so they key the flight.
	var audio []string
	if len(profile.PreferredLanguages) == 0 && req.User != nil {
		audio = req.User.AudioLanguages
	}
	return fmt.Sprintf("%d|%s|%s|%s|%s|%s|%t|%s", req.Title.Key, profile.Name, strings.Join(ex, ","), capsJSON, profileJSON, req.Prefer, req.ExactHEVCTranscode, strings.Join(audio, ","))
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
	media := titleMedia(req.Title)
	q := s.qualityCfg()
	libProfile := s.libProfileForTitle(ctx, req.Title)
	req.Client, _ = applyClientRules(q, req.Client, req.User, media, libProfile)
	profile := SelectProfileForTitle(q, req.Client, req.User, req.Title, libProfile)
	key := flightKey(req, profile)

	for {
		s.flightMu.Lock()
		if f, ok := s.flights[key]; ok {
			s.flightMu.Unlock()
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
		s.flights[key] = f
		s.flightMu.Unlock()

		ctx = context.WithValue(ctx, observationTitleKey{}, req.Title)
		res, err := s.resolve(ctx, req, profile)
		f.res, f.err, f.abandoned = res, err, err != nil && ctx.Err() != nil
		s.flightMu.Lock()
		delete(s.flights, key)
		s.flightMu.Unlock()
		close(f.done)
		return res, err
	}
}

func (s *Service) resolve(ctx context.Context, req core.ResolveRequest, profile core.QualityProfile) (*core.Resolution, error) {
	started := time.Now()
	// An indexer that was paused or did not answer leaves the search
	// incomplete; nothing found then does not prove the title unavailable.
	ctx, coverage := core.WithSearchCoverage(ctx)
	t := req.Title
	trace := Trace{TitleKey: t.Key, TitleName: consoleTitleName(t), Profile: profile.Name, At: s.now(), Failover: len(req.Exclude) > 0}
	trace.Limits = &TraceLimits{ProfileMinHeight: profile.MinHeight, ProfileMaxHeight: profile.MaxHeight, ProfileConfiguredMaxHeight: traceConfiguredMaxHeight(profile), ClientMaxHeight: req.Client.MaxHeight, ClientMaxBitrateKbps: req.Client.MaxBitrateKbps, AllowHDR: profile.AllowHDR, AllowDolbyVision: profile.AllowDolbyVision}
	defer func() {
		trace.TotalDuration = time.Since(started)
		trace.Ceiling = s.explainCeiling(&trace, profile, req)
		noteChooser(&trace, req)
		if trace.Ceiling != "" {
			s.log.Info("resolver: below 4K", "title", trace.TitleName, "profile", profile.Name, "why", trace.Ceiling)
		}
		s.addTrace(trace)
		s.recordHistory(ctx, trace)
	}()

	// The owner's pin for this title, when it applies to this request
	// (pins.go). It is tried first, like a viewer's Play Version pick.
	pin, pinNote := s.pinForResolve(req)
	trace.PinStatus = pinNote
	pinID := ""
	if pin != nil {
		pinID = pin.Release.ID
		trace.Pinned = pinID
	}

	ctx, inconclusive := withUnverifiedPool(ctx)
	sreq := s.searchRequest(ctx, t)
	early := newSearchPreparation(ctx, s, req, sreq, profile, pinID)
	defer early.close()
	ctx = context.WithValue(ctx, searchPreparationKey{}, early)
	var discovered []core.Release
	if s.discovered != nil && pin == nil && req.Prefer == "" && len(req.Exclude) == 0 {
		discovered = filterOutPlaceholders(s.discovered(ctx, t))
		if len(discovered) > 0 {
			trace.Discovery = append(trace.Discovery, DiscoveryTrace{Path: "feed", Count: len(discovered)})
		}
		early.arrived(discovered)
		early.startFeed()

		// A locally verified feed answer can avoid a search. A slow feed
		// gets only 100ms head start, then keeps checking alongside search.
		if len(discovered) > 0 && !early.disabled {
			deadline := time.NewTimer(100 * time.Millisecond)
			tick := time.NewTicker(5 * time.Millisecond)
		feedWait:
			for {
				if ready := early.readyResultsAfter(0); len(ready) > 0 {
					candidates, _, rankErr := s.rankCandidates(ctx, req, sreq, profile, ready, &trace, "")
					if rankErr == nil && len(candidates) > 0 && !candidates[0].fallback {
						early.attach(candidates)
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
	if pin == nil && len(discovered) == 0 {
		if err := s.missingSources(); err != nil {
			trace.Err = err.Error()
			return nil, err
		}
	}

	// Fast path: cached unavailable answer, unless this is a failover
	// request that already knows about specific releases, the viewer
	// picked a specific version, or the owner pinned a release.
	forceFresh := false
	if len(req.Exclude) == 0 && req.Prefer == "" && pin == nil && len(discovered) == 0 {
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
	results, cached, searchErr := s.searchCached(ctx, sreq, forceFresh)
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
		if pin == nil && len(discovered) == 0 {
			trace.Err = searchErr.Error()
			return nil, searchErr
		}
		// A pin carries its full release, so it can still play while
		// the indexers are failing; the search error stands otherwise.
		results = nil
	}
	s.rememberReleases(t, results)
	if s.searchObserved != nil {
		s.searchObserved(ctx, t, results)
	}
	for _, r := range discovered {
		results = withRelease(results, r)
	}
	if pin != nil {
		results = withRelease(results, pin.Release)
	}

	rcfg := s.resolverCfg()
	cands, clientRejected, err := s.rankCandidates(ctx, req, sreq, profile, results, &trace, pinID)
	if err != nil {
		trace.Err = err.Error()
		return nil, err
	}
	// Speculative work (warm-ups, detail discovery) never runs a rescue: it
	// is Play's second pass. What speculative work did not find proves
	// nothing about the title, so it is not remembered unavailable either.
	speculative := core.SpeculativeIndexerWork(ctx)
	rescueTried, rescueSkipped := false, false
	if len(cands) == 0 && searchErr == nil && ctx.Err() == nil && speculative {
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
			s.notePin(ctx, t.Key, pinID, PinRejected, candidateReason(&trace, pinID))
			trace.PinStatus = "rejected: " + candidateReason(&trace, pinID)
		}
		// Client/profile restrictions and temporary source skips do not prove
		// that the title is unavailable for everyone.
		if !clientRejected && !rescueSkipped && len(req.Exclude) == 0 && searchErr == nil && !coverage.Incomplete() {
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
		trace.Err = "no acceptable candidates"
		return nil, failed(fmt.Errorf("%q: %w", t.Title, core.ErrNotAvailable))
	}

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
	trace.SourcePreparationDuration = time.Since(prepareStart)
	if rcfg.OptimisticStart && !preferTried {
		if cands[0].rel.OpensOnDemand() && !core.OnDemandAllowedAt(ctx, cands[0].rel, 0) {
			// Nothing ranked below the add-on stream may stand in for it.
			trace.Err = errOnDemandForeground.Error()
			return nil, errOnDemandForeground
		}
		at := time.Now()
		nzb, err := s.indexer.FetchNZB(ctx, cands[0].rel)
		trace.Candidates[cands[0].trace].Attempted = true
		trace.Candidates[cands[0].trace].FetchDuration = time.Since(at)
		if err == nil {
			winner := cands[0]
			trace.Winner = winner.rel.ID
			trace.Candidates[winner.trace].Reason = "optimistic start: health check pending"
			s.optimisticCheck(winner.rel, nzb)
			return s.resolution(t.Key, cands, 0, nzb, &trace), nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		trace.Candidates[cands[0].trace].Rejected = true
		trace.Candidates[cands[0].trace].Reason = "nzb fetch: " + err.Error()
		trace.Candidates[cands[0].trace].err = err
		if !permanentFailure(err) {
			optimisticErr = err
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
		index, nzb, verr := s.validateCandidates(ctx, downgrades, &trace, rcfg.HealthRace)
		if verr == nil && index >= 0 {
			trace.Downgrade = downgradeNote(reference, downgrades[index].rel, len(primary) > 0)
			s.log.Info("resolver: playing a lower quality", "title", t.Title, "release", downgrades[index].rel.Name, "reason", trace.Downgrade)
			return s.acceptResolution(ctx, t.Key, downgrades, index, nzb, &trace), nil
		}
	}
	var fallbackCands []candidate
	if profile.FallbackBelowMin && profile.MinHeight > 0 && ctx.Err() == nil && !hasFallbackCandidates(cands) {
		fbCands, fbRejected, ferr := s.rankBelowMinFallback(ctx, req, sreq, profile, results, &trace)
		clientRejected = clientRejected || fbRejected
		if ferr == nil && len(fbCands) > 0 {
			var fbChecks candidateChecks
			defer fbChecks.close()
			at := time.Now()
			fbCands = s.prepareSources(ctx, fbCands, req.Title, &trace, &fbChecks)
			trace.SourcePreparationDuration += time.Since(at)
			fallbackCands = fbCands
			cands = append(cands, fbCands...)
			index, nzb, verr := s.validateCandidates(ctx, fallbackCands, &trace, rcfg.HealthRace)
			if verr == nil && index >= 0 {
				return s.acceptResolution(ctx, t.Key, fallbackCands, index, nzb, &trace), nil
			}
		}
	}

	// The resolver's own order: comparable quality (rescue included)
	// before any drastic downgrade, which are already sorted by quality.
	if res := s.lastResort(ctx, inconclusive, t.Key, concat(lead, primary, rescuePrimary, downgrades, fallbackCands), &trace); res != nil {
		if slices.ContainsFunc(downgrades, func(c candidate) bool { return c.rel.ID == res.Release.ID }) {
			trace.Downgrade = downgradeNote(reference, res.Release, len(primary) > 0)
		}
		return res, nil
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
	all := concat(lead, primary, rescuePrimary, downgrades, fallbackCands)
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
	if !clientRejected && !rescueSkipped && len(req.Exclude) == 0 && len(cands) <= maxHealthAttempts && !(core.IsSpeculative(ctx) && len(cands) > speculativeHealthAttempts) && !coverage.Incomplete() {
		s.setAvailability(ctx, int64(t.Key), false, min(s.cooloff(), failedHealthTTL))
	}
	trace.Err = "no healthy candidates"
	for _, c := range all {
		ct := &trace.Candidates[c.trace]
		if ct.Attempted && ct.Rejected {
			return nil, fmt.Errorf("%q: %w: %w", t.Title, core.ErrNotAvailable, core.ErrSourceUnavailable)
		}
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

func searchCacheKey(req core.SearchRequest) string {
	return fmt.Sprintf("%s|%v|%s|%d|%d|%d", req.Type, req.IDs, strings.ToLower(req.Title), req.Year, req.Season, req.Episode)
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
	if ok && s.now().Before(e.expires) {
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
	s.searchCache[key] = searchCacheEntry{results: append([]core.Release(nil), results...), expires: s.now().Add(s.searchTTL())}
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
		if ok && s.now().Before(e.expires) {
			return append([]core.Release(nil), e.results...)
		}
	}
	rctx, coverage := core.WithSearchCoverage(ctx)
	results, err := rs.SearchRescue(rctx, req)
	if err != nil {
		return nil
	}
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
		s.searchCache[key] = searchCacheEntry{results: append([]core.Release(nil), results...), expires: s.now().Add(s.searchTTL())}
		s.mu.Unlock()
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
	}
	var out []core.Release
	for _, r := range incoming {
		if seen[r.ID] || (r.InfoHash != "" && seen["h:"+r.InfoHash]) {
			continue
		}
		seen[r.ID] = true
		if r.InfoHash != "" {
			seen["h:"+r.InfoHash] = true
		}
		out = append(out, r)
	}
	return out
}

// searchCached answers from the search cache unless fresh is set; cached
// reports whether the answer came from the cache.
func (s *Service) searchCached(ctx context.Context, req core.SearchRequest, fresh bool) (results []core.Release, cached bool, err error) {
	key := searchCacheKey(req)
	// A detail search starts before playback preparation. Join it rather
	// than spending a second set of indexer requests for the same title.
	if !fresh && ctx.Value(refillSearchContextKey{}) == nil {
		s.bgMu.Lock()
		done := s.refills[key]
		mark := s.refillRetryAfter[key]
		s.bgMu.Unlock()
		if done != nil {
			// A viewer's Play joins the detail search only briefly: that search
			// waits for every indexer, so after playStartDeadline Play runs its
			// own progressive search instead of a slow indexer's timeout.
			var bound <-chan time.Time
			if !core.SpeculativeIndexerWork(ctx) {
				t := time.NewTimer(playStartDeadline)
				defer t.Stop()
				bound = t.C
			}
			select {
			case <-bound:
			case <-done:
				if results, ok := s.peekSearchCache(key); ok {
					return results, true, nil
				}
				// A failed detail discovery is not a reason for speculative
				// playback preparation to immediately spend the quota again.
				if core.SpeculativeIndexerWork(ctx) {
					return nil, false, core.ErrBudgetExhausted
				}
			case <-ctx.Done():
				return nil, false, ctx.Err()
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
	if ok && !fresh && s.now().Before(e.expires) {
		return append([]core.Release(nil), e.results...), true, nil
	}
	coverage := core.SearchCoverageFrom(ctx)
	if coverage == nil {
		ctx, coverage = core.WithSearchCoverage(ctx)
	}
	partial := false
	if early, ok := ctx.Value(searchPreparationKey{}).(*searchPreparation); ok {
		if streaming, ok := s.indexer.(core.StreamingIndexer); ok {
			results, partial, err = early.searchLive(ctx, streaming, req)
		} else {
			results, err = s.indexer.Search(ctx, req)
		}
	} else {
		results, err = s.indexer.Search(ctx, req)
	}
	if err != nil {
		return nil, false, err
	}
	results = s.normalizeSearchResults(req, results)
	if partial || coverage.Incomplete() {
		// Never cache an incomplete fleet answer: an early Play start,
		// or an indexer that was paused or did not answer.
		return results, false, nil
	}
	s.storeSearchCache(req, results)
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
	s.searchCache[key] = searchCacheEntry{results: append([]core.Release(nil), results...), expires: now.Add(ttl)}
	s.mu.Unlock()
}

// rankCandidates is scoreCandidates with the owner's pinned release id
// (pinID, "" for none): the pin is judged by pinRejectReason's hard gates
// instead of the profile and identity rules, and is otherwise scored and
// traced like every other result. Interactive search reads the same trace.
func (s *Service) rankCandidates(ctx context.Context, req core.ResolveRequest, sreq core.SearchRequest, profile core.QualityProfile, results []core.Release, trace *Trace, pinID string) ([]candidate, bool, error) {
	policy, err := s.compileRules(profile.ReleaseRules)
	if err != nil {
		return nil, true, err
	}
	env := rankEnv{policy: policy}
	if usesPluginRanking(profile) {
		env.hot = s.rankingSession(ctx, req.Client)
		if env.hot != nil {
			defer env.hot.Close()
		}
	}
	env.exclude = map[string]bool{}
	for _, id := range req.Exclude {
		env.exclude[id] = true
	}
	env.rcfg = s.resolverCfg()
	env.lang = s.catalogLanguage()
	env.audio(profile, req)
	return s.rankWith(ctx, env, req, sreq, profile, results, trace, pinID)
}

// rankEnv is what ranking reads besides the results: the compiled rules,
// the hosted ranking session, resolver settings and audio languages. The
// feed evidence pass prepares it once and reuses it for every release.
type rankEnv struct {
	policy           *releasepolicy.Policy
	hot              RankingSession
	exclude          map[string]bool
	rcfg             config.Resolver
	lang             string
	wanted, accepted []string
}

// audio fills the request's wanted and accepted audio languages.
func (env *rankEnv) audio(profile core.QualityProfile, req core.ResolveRequest) {
	env.wanted = wantedAudio(profile, req.User, env.lang)
	env.accepted = acceptedAudio(profile, req.User, env.lang, req.Title)
}

// rankWith is rankCandidates with its environment already prepared.
func (s *Service) rankWith(ctx context.Context, env rankEnv, req core.ResolveRequest, sreq core.SearchRequest, profile core.QualityProfile, results []core.Release, trace *Trace, pinID string) ([]candidate, bool, error) {
	if s.db != nil {
		s.loadRejectionsFor(ctx, req.Title.Key)
	}
	s.prepareSeriesYears(ctx, req.Title, sreq, results)
	start := 0
	if trace != nil {
		start = len(trace.Candidates)
	}
	cands, rejected, err := s.rankPass(ctx, env, req, sreq, profile, results, trace, pinID, false)
	if err != nil || len(cands) > 0 || !profile.FallbackBelowMin || profile.MinHeight <= 0 {
		return cands, rejected, err
	}
	if trace != nil {
		trace.Candidates = trace.Candidates[:start]
	}
	// Re-evaluate the same search results, relaxing only the quality floor
	// and imported allowlist. All other gates are evaluated again unchanged.
	fallback, fallbackRejected, err := s.rankPass(ctx, env, req, sreq, profile, results, trace, pinID, true)
	return fallback, rejected || fallbackRejected, err
}

func hasFallbackCandidates(cands []candidate) bool {
	for _, c := range cands {
		if c.fallback {
			return true
		}
	}
	return false
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

// rankBelowMinFallback evaluates below-minimum releases after every strict
// candidate has failed health or debrid-cache checks, updating their existing
// trace entries in place so no duplicate candidate rows are recorded.
func (s *Service) rankBelowMinFallback(ctx context.Context, req core.ResolveRequest, sreq core.SearchRequest, profile core.QualityProfile, results []core.Release, trace *Trace) ([]candidate, bool, error) {
	policy, err := s.compileRules(profile.ReleaseRules)
	if err != nil {
		return nil, true, err
	}
	env := rankEnv{policy: policy}
	if usesPluginRanking(profile) {
		env.hot = s.rankingSession(ctx, req.Client)
		if env.hot != nil {
			defer env.hot.Close()
		}
	}
	env.exclude = map[string]bool{}
	for _, id := range req.Exclude {
		env.exclude[id] = true
	}
	env.rcfg = s.resolverCfg()
	env.lang = s.catalogLanguage()
	env.audio(profile, req)
	if s.db != nil {
		s.loadRejectionsFor(ctx, req.Title.Key)
	}
	s.prepareSeriesYears(ctx, req.Title, sreq, results)
	var fbTrace Trace
	fbCands, rejected, err := s.rankPass(ctx, env, req, sreq, profile, results, &fbTrace, "", true)
	if err != nil || len(fbCands) == 0 {
		return nil, rejected, err
	}
	if trace != nil {
		for i := range fbCands {
			fbCT := fbTrace.Candidates[fbCands[i].trace]
			placed := false
			for j := range trace.Candidates {
				if trace.Candidates[j].ReleaseID == fbCands[i].rel.ID && !trace.Candidates[j].Attempted {
					trace.Candidates[j] = fbCT
					fbCands[i].trace = j
					placed = true
					break
				}
			}
			if !placed {
				trace.Candidates = append(trace.Candidates, fbCT)
				fbCands[i].trace = len(trace.Candidates) - 1
			}
		}
	}
	return fbCands, rejected, nil
}

func (s *Service) rankPass(ctx context.Context, env rankEnv, req core.ResolveRequest, sreq core.SearchRequest, profile core.QualityProfile, results []core.Release, trace *Trace, pinID string, fallback bool) ([]candidate, bool, error) {
	t := req.Title
	policy, hot, exclude, rcfg, wanted, accepted := env.policy, env.hot, env.exclude, env.rcfg, env.wanted, env.accepted
	upscales := presumedUpscales(results, isAnime(t))
	ids := make([]string, len(results))
	for i, r := range results {
		ids[i] = r.ID
	}
	verifiedIDs := s.verifiedAmong(ctx, ids)
	// One instant ages every candidate. Reading the clock per release made
	// identical releases differ by the nanoseconds between their scores, so
	// ties broke on scoring order and clock resolution (a single tick on
	// Windows) instead of on the ranking.
	now := s.now()
	rc := s.rankContext(profile, t, rcfg, now)
	var cands []candidate
	clientRejected := false
	for ri, rel := range results {
		rel = withTarget(rel, sreq)
		ct := CandidateTrace{ReleaseID: rel.ID, Name: rel.Name, Indexer: rel.Indexer, Size: rel.Size, Height: rel.Info.Height}
		if fallback {
			h := fallbackHeight(rel.Info)
			if h == 0 || h >= profile.MinHeight {
				ct.Rejected = true
				ct.Reason = "not below profile minimum"
				if trace != nil {
					trace.Candidates = append(trace.Candidates, ct)
				}
				continue
			}
		}
		pinned := !fallback && pinID != "" && rel.ID == pinID
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
		policyResult := policy.Evaluate(rel, policyContext)
		if env.rcfg.HardSubtitles != "allow" && s.hardSubbed(ctx, rel) {
			policyResult.Fallback += 100
		}
		if len(profile.ReleaseRules) > 0 {
			ct.Preferences = &policyResult
		}
		preferred := !fallback && (pinned || (req.Prefer != "" && rel.ID == req.Prefer))
		var reason string
		if s.HardSubtitleBlocked(ctx, rel) {
			reason = "blocked hardcoded subtitles"
		} else if s.rejectedFor(t.Key, rel) {
			reason = "release rejected for this title"
		} else if pinned {
			reason = s.pinRejectReason(rel, sreq, req.Client, exclude)
		} else if !preferred && policyResult.Rejected {
			reason = preferenceReason(policyResult)
		} else {
			gateProfile := profile
			if fallback {
				gateProfile.MinHeight = 0
				gateProfile.AllowedQualities = nil
				gateProfile.AllowCam = false
				preferred = false
			}
			reason = s.rejectReason(rel, t, sreq, gateProfile, rcfg, req.Client, exclude, preferred)
			if !fallback && reason == "video codec unsupported by client" && req.ExactHEVCTranscode && !req.Client.Download && req.Prefer != "" && rel.ID == req.Prefer && strings.EqualFold(rel.Info.VideoCodec, "hevc") {
				// Only relax the codec gate for the explicitly selected source;
				// every other policy and the final probed output decision still apply.
				caps := req.Client
				caps.VideoCodecs = append(slices.Clone(caps.VideoCodecs), rel.Info.VideoCodec)
				reason = s.rejectReason(rel, t, sreq, gateProfile, rcfg, caps, exclude, preferred)
			}
		}
		if reason != "" {
			if strings.HasPrefix(reason, "release preference:") {
				clientRejected = true
			}
			ct.Rejected = true
			switch reason {
			case ReasonAboveProfileMaxResolution, ReasonAboveClientMaxResolution, ReasonBelowProfileMinResolution, "dolby vision not allowed for client", "video codec unsupported by client", "quality excluded by imported profile", "source recently failed health check; retrying later",
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
		langOK := audioAcceptable(rel.Info, accepted)
		langWanted := langOK && foreignTitleWantedAudio(rel.Info, wanted, t)
		lowBitrate := bitrateScore(sizedForEpisode(rel), t) < 0
		starved := lowBitrate || upscales[ri]
		yearGuessed := seriesYearGuessed(rel, sreq)
		facts := formatFacts{starved: starved, lowBitrate: lowBitrate, viewerLanguage: langWanted, yearDiffers: yearGuessed, pastRetention: s.pastRetention(rel, now)}
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
			return nil, clientRejected, err
		}
		ct.CustomFormats = sc.matches
		if minimum := profile.MinCustomFormatScore; minimum != nil && sc.counted < *minimum && !preferred {
			clientRejected = true
			ct.Rejected = true
			ct.Reason = "below minimum custom format score"
			if trace != nil {
				trace.Candidates = append(trace.Candidates, ct)
			}
			continue
		}
		ct.CompatibilityReason = sc.compat
		rel.Score = float64(sc.total)
		ct.Score = rel.Score
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
		bandwidth := s.deliveryFit(sizedForEpisode(rel), req.Client, t)
		if trace != nil {
			note := func(n string) {
				trace.Candidates[tr].CompatibilityReason = strings.TrimPrefix(trace.Candidates[tr].CompatibilityReason+"; "+n, "; ")
			}
			switch {
			case upscales[ri]:
				note("presumed upscale: ranked one resolution lower")
			case lowBitrate:
				note("too little bitrate for its resolution: ranked one resolution lower")
			}
			if bandwidth < 0 {
				note("larger than the client's connection carries")
			}
			if yearGuessed {
				note("series year differs but names no other show: ranked after correctly-labelled releases")
			}
			if !langOK {
				note("ranked after releases in an accepted audio language")
			}
		}
		cands = append(cands, candidate{fallback: fallback, policy: policyResult, rel: rel, trace: tr, verified: verified, langOK: langOK, langWanted: langWanted, starved: starved, bandwidth: bandwidth, yearGuessed: yearGuessed, cf: sc.total, keys: keysFor(rel, starved, rc)})
	}
	if hot != nil && len(cands) > 0 {
		s.addPluginScores(hot, profile, cands, trace)
	}
	if fallback {
		order := sourceOrder(profile.QualityOrder)
		sort.SliceStable(cands, func(i, j int) bool {
			a, b := fallbackHeight(cands[i].rel.Info), fallbackHeight(cands[j].rel.Info)
			if a != b {
				return a > b
			}
			if len(profile.AllowedQualities) > 0 {
				a, b := cands[i].rel.Info.Source, cands[j].rel.Info.Source
				if a != b {
					if order := compareFallbackSource(a, b, order); order != 0 {
						return order > 0
					}
				}
			}
			return compareCandidates(cands[i], cands[j])
		})
		return cands, clientRejected, nil
	}
	return s.orderCandidates(cands, req.Title, trace), clientRejected, nil
}

func (s *Service) orderCandidates(cands []candidate, title *core.Title, trace *Trace) []candidate {
	sort.SliceStable(cands, func(i, j int) bool {
		return compareCandidates(cands[i], cands[j])
	})
	return s.routeSources(cands, title, trace)
}

// rankContext gathers what the comparer keys read for one ranking pass.
func (s *Service) rankContext(profile core.QualityProfile, t *core.Title, rcfg config.Resolver, now time.Time) rankContext {
	s.cfgMu.RLock()
	scorer, priority := s.scorer, s.indexerPriority
	s.cfgMu.RUnlock()
	return rankContext{
		profile:        profile,
		title:          t,
		now:            now,
		preferSource:   preferredSource(rcfg.SourcePreference),
		preferRevision: !scorer.scoresRevision(profile.CustomFormatScores),
		priority:       priority,
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
	for _, id := range builtinFormatMatches(formatRel, profile, caps, facts) {
		if id == core.FormatRepackProper && revisionScored {
			// The profile scores revisions with its own Repack/Proper
			// format; the built-in one would count them twice.
			continue
		}
		score, scored := profile.CustomFormatScores[id]
		if !scored || score == 0 {
			continue // a built-in format the profile doesn't use
		}
		total += score
		matches = append(matches, FormatMatch{ID: id, Name: BuiltinFormatName(id), Score: score})
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
	// Usenet passwords are supplied by NZB metadata and validated by the engine.
	if rel.Password && rel.Protocol == core.ProtocolTorrent {
		return "passworded"
	}
	if exclude[rel.ID] {
		return "excluded (already tried)"
	}
	if s.rejectedFor(t.Key, rel) {
		return "release rejected for this title"
	}
	if s.isBad(rel.ID) {
		return "marked bad"
	}
	if s.recentlyFailedHealth(rel.ID) {
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
	if reason := sourceSuitability(rel, t, profile, preferred); reason != "" {
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
	if reason := rejectIdentityYears(rel, t, sreq, s.catalogLanguage(), acceptYear); reason != "" {
		return reason
	}
	return rejectPolicy(rel, t, profile, caps, preferred, preferred && s.resolverCfg().OfferHigherVersions)
}

// rejectIdentity checks the result's reported identity without client policy.
// catalogLang is the language catalogue episode titles are in ("en-US").
func rejectIdentity(rel core.Release, t *core.Title, sreq core.SearchRequest, catalogLang string) string {
	return rejectIdentityYears(rel, t, sreq, catalogLang, nil)
}

// rejectIdentityYears is rejectIdentity where acceptYear may accept a series
// year that differs from the show's (one that names no other show).
func rejectIdentityYears(rel core.Release, t *core.Title, sreq core.SearchRequest, catalogLang string, acceptYear func(int) bool) string {
	if releaseparse.IsPlaceholderResult(rel.Name) {
		return "indexer placeholder"
	}
	isEpisode := sreq.Type == core.TypeEpisode
	idMatch, idConflict := releaseparse.CompareTitleIDs(rel.TitleIDs, sreq.IDs)
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
				(t.ShowOriginalTitle != "" && releaseparse.TitlesMatch(partTitle, t.ShowOriginalTitle)) {
				miniseriesPart = partNum
			}
		}
	}
	titleOK := miniseriesPart > 0 ||
		releaseparse.TitleMatches(rel.Info, wantTitle) ||
		(!isEpisode && t.OriginalTitle != "" && releaseparse.TitleMatches(rel.Info, t.OriginalTitle)) ||
		(isEpisode && t.ShowOriginalTitle != "" && releaseparse.TitleMatches(rel.Info, t.ShowOriginalTitle)) ||
		matchesAlternativeTitle(rel.Info, sreq.AlternativeTitles) ||
		(!isEpisode && matchesAlternativeTitle(rel.Info, t.AlternativeTitles)) ||
		(isEpisode && matchesAlternativeTitle(rel.Info, t.ShowAlternativeTitles))
	if titleOK && !idMatch && regionTagConflicts(rel.Info.Title, wantTitle, t.Countries) {
		titleOK = false
	}
	if !titleOK && !idMatch {
		obf := releaseparse.IsObfuscated(rel.Info.Title)
		if obf && !releaseparse.HasUsableInfo(rel.Info) {
			return "obfuscated, no usable info"
		}
		// Querying by ID does not prove that the indexer honored it.
		// Only identifiers actually reported on this result can establish
		// an otherwise unreadable or alternate title's identity.
		return formatTitleMismatch(rel.Info.Title, expectedTitlesFor(t, sreq))
	}
	if isEpisode {
		releaseYear := releaseparse.EpisodeSeriesYear(rel.Name, rel.Info.Year)
		if !releaseparse.SeriesYearMatches(releaseYear, sreq.Year) && (acceptYear == nil || !acceptYear(releaseYear)) {
			return "series year mismatch"
		}
		if releaseYear == 0 && !idMatch && !localizedEpisodeTitle(rel.Info, t, catalogLang) && !episodeTitleMatchesRelease(rel, sreq.EpisodeTitle) {
			return "episode title mismatch"
		}
		if rel.Info.FullSeason {
			// A pack streams the wanted episode's file (Release.Target):
			// through debrid for a torrent, from the episode's own NZB
			// files for Usenet. Other protocols cannot pick a file.
			if rel.Protocol != core.ProtocolTorrent && rel.Protocol != core.ProtocolUsenet {
				return "season pack (skipped in v1)"
			}
			packEpisode := sreq.Episode
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
		} else if !episodeMatchesRequest(rel.Info, sreq) {
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
	if t != nil {
		if sreq.Type == core.TypeEpisode {
			add(t.ShowOriginalTitle)
		} else {
			add(t.OriginalTitle)
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

var reCourEpisodeMarker = regexp.MustCompile(`(?i)\b(?:S\d{1,2}|(?:Season|Saison|Stagione|Temporada)\s*\d{1,2})[\s._-]+(?:Part|Cour)[\s._-]*\d{1,2}(?:[\s._-]+-[\s._-]+|[\s._-]+E?)\d{1,3}(?:v\d)?\b`)

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

func episodeMatchesRequest(info core.ReleaseInfo, sreq core.SearchRequest) bool {
	if info.Cour == 1 && sreq.Cour > 1 {
		return false
	}
	if info.Cour > 1 {
		if sreq.Cour == info.Cour && sreq.CourEpisode > 0 && releaseparse.EpisodeMatches(info, sreq.Season, sreq.CourEpisode) {
			return true
		}
		return false
	}
	return releaseparse.EpisodeMatches(info, sreq.Season, sreq.Episode) ||
		releaseparse.EpisodeMatchesAbsolute(info, absoluteEpisode(sreq))
}

func packCoversSeasonCour(info core.ReleaseInfo, sreq core.SearchRequest) bool {
	if !packCoversSeason(info, sreq.Season) {
		return false
	}
	if info.Cour > 1 && sreq.Cour > 0 && info.Cour != sreq.Cour {
		return false
	}
	if info.Cour > 1 && (sreq.Cour == 0 || sreq.CourEpisode <= 0) {
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

// rejectPolicy applies client support and quality/size policy in rejection order.
// Release rules are judged once in rankCandidates; this does not compile them again.
// preferred lets an explicit pick go below the profile floor. ceilingBypass,
// set only when the owner opted in to higher versions, is what lets that
// pick exceed the ceiling. Codec support is never bypassed.
func rejectPolicy(rel core.Release, t *core.Title, profile core.QualityProfile, caps core.ClientCaps, preferred, ceilingBypass bool) string {
	if !ceilingBypass && len(profile.AllowedQualities) > 0 {
		allowed := false
		for _, q := range profile.AllowedQualities {
			if qualityGateMatch(q, rel.Info) {
				allowed = true
				break
			}
		}
		if !allowed {
			return "quality excluded by imported profile"
		}
	}
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
	if h := floorHeight(rel.Info); !preferred && profile.MinHeight > 0 && h > 0 && h < profile.MinHeight {
		return ReasonBelowProfileMinResolution
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

// withTarget marks a torrent candidate for an episode request with the
// wanted episode, so debrid streams that file of a multi-file torrent. A
// season pack gets a per-episode release ID: health, bad marks, pins and
// cached bytes then apply to the episode file, not the whole pack.
func withTarget(rel core.Release, sreq core.SearchRequest) core.Release {
	if sreq.Type != core.TypeEpisode || sreq.Episode <= 0 {
		return rel
	}
	// A Usenet season pack is narrowed to the episode's files when its NZB
	// is fetched; other Usenet releases need no target.
	if rel.Protocol != core.ProtocolTorrent && !(rel.Protocol == core.ProtocolUsenet && rel.Info.FullSeason) {
		return rel
	}
	if rel.Target != nil {
		target := *rel.Target
		target.ShowYear, target.EpisodeTitle = sreq.Year, sreq.EpisodeTitle
		target.AbsoluteEpisode = releaseparse.AnimeSearchEpisode(sreq)
		target.Cour, target.CourEpisode = sreq.Cour, sreq.CourEpisode
		target.ExpectedIDs, target.ReportedIDs = sreq.IDs, rel.TitleIDs
		rel.Target = &target
		return rel // enrich cached identities without deriving their ID again
	}
	target := core.ReleaseTarget{AbsoluteEpisode: releaseparse.AnimeSearchEpisode(sreq), Cour: sreq.Cour, CourEpisode: sreq.CourEpisode, Season: sreq.Season, Episode: sreq.Episode, ShowYear: sreq.Year, EpisodeTitle: sreq.EpisodeTitle, ExpectedIDs: sreq.IDs, ReportedIDs: rel.TitleIDs}
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
// checks and bitrate estimates.
func sizedForEpisode(rel core.Release) core.Release {
	if rel.Info.FullSeason {
		rel.Size = 0
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

// sourceOrder lists the release types a quality order names, in the order
// they first appear.
func sourceOrder(order []core.QualityItem) []core.Source {
	var out []core.Source
	for _, item := range order {
		for _, q := range item.Qualities {
			src, _, _ := strings.Cut(q, "-")
			s := core.Source(src)
			if src == "unknown" {
				s = core.SourceUnknown
			}
			if !slices.Contains(out, s) {
				out = append(out, s)
			}
		}
	}
	return out
}

// compareFallbackSource preserves an imported profile's order for sources it
// names (order, from sourceOrder), and uses the usual source hierarchy for
// the remaining source types.
func compareFallbackSource(a, b core.Source, order []core.Source) int {
	level := func(s core.Source) int {
		switch s {
		case core.SourceRemux:
			return 6
		case core.SourceBluRay:
			return 5
		case core.SourceWEBDL, core.SourceWEBRip:
			return 4
		case core.SourceHDTV:
			return 3
		case core.SourceDVD:
			return 2
		default:
			return 1
		}
	}
	rank := func(s core.Source) int {
		best := -10000
		for i, allowed := range order {
			d := level(s) - level(allowed)
			if d < 0 {
				d = -d
			}
			score := 1000 - i*100 - d*10
			if s == allowed {
				score++
			}
			best = max(best, score)
		}
		return best*10 + level(s)
	}
	return rank(a) - rank(b)
}

func fallbackHeight(info core.ReleaseInfo) int {
	if info.Height == 0 && info.Source == core.SourceDVD {
		return 480
	}
	return floorHeight(info)
}

// SourcesChanged invalidates remembered results when a database-backed source changes.
func (s *Service) SourcesChanged(ctx context.Context) { s.forgetSourceResults(ctx) }
