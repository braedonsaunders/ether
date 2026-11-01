package resolver

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
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
	defaultMaxTraces      = 50
	maxHealthAttempts     = 32
	defaultSearchCacheTTL = 6 * time.Hour
	defaultUnavailableTTL = 24 * time.Hour
	defaultMaxCandidates  = 3
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

// Options tunes a Service. Zero values get sensible defaults.
type Options struct {
	SearchObserved func(context.Context, *core.Title, []core.Release)
	Discovered     func(context.Context, *core.Title) []core.Release
	Observed       func(context.Context, *core.Title, core.Release)
	Invalidated    func(context.Context, string)
	Logger         *slog.Logger
	Now            func() time.Time
	MaxTraces      int
	RetentionDays  int // posts older than this are penalised; 0 = no extra penalty
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
	scorerErr        error
	sources          string // sourcesKey of the applied config

	mu            sync.RWMutex
	bad           map[string]badMark // release id -> blocklist mark
	reps          map[string]reputation
	avail         map[int64]availEntry
	searchCache   map[string]searchCacheEntry
	retentionDays int
	verified      map[string]time.Time // release id -> last passed health check
	failedHealth  map[string]time.Time // confirmed source failures, briefly skipped on subsequent resolves
	healthTTL     time.Duration

	tracesMu  sync.Mutex
	traces    []Trace
	maxTraces int

	bgCtx    context.Context
	bgCancel context.CancelFunc
	bgMu     sync.Mutex
	bgClosed bool
	bgWG     sync.WaitGroup
	refills  map[string]chan struct{} // episode search key -> completion; guarded by bgMu

	flightMu sync.Mutex
	flights  map[string]*inflight

	delivery deliveryState

	route routingState // source routing: startup measurements and signals

	con consoleState // pins, release memory, health results, history (console.go)

	invalMu sync.Mutex
	inval   *clusterbus.Invalidator // nil on a standalone server
}

var _ core.Resolver = (*Service)(nil)

// New builds a resolver Service. indexer is the aggregate indexer
// (typically *newznab.Manager); health may be nil (health checks are
// then skipped); db may be nil (state then lives in memory).
func New(indexer core.Indexer, health core.HealthChecker, db *sql.DB, cfg config.Config, opts Options) *Service {
	s := &Service{searchObserved: opts.SearchObserved, discovered: opts.Discovered, observed: opts.Observed, invalidated: opts.Invalidated,
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
	s.scorer, s.scorerErr = NewFormatScorer(cfg.Resolver.CustomFormats, cfg.Resolver.CustomFormatScores)
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

// profileAllowsCam reports whether any quality profile plays cinema recordings.
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
			AbsoluteEpisode: t.AbsoluteEpisode, Anime: isAnime(t), AlternativeTitles: t.ShowAlternativeTitles}
	}
	return core.SearchRequest{Type: core.TypeMovie, IDs: t.IDs, Title: t.Title, OriginalTitle: t.OriginalTitle, Year: t.Year, Anime: isAnime(t), AlternativeTitles: t.AlternativeTitles}
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
	req.Client = ApplyClientRules(s.qualityCfg(), req.Client, req.User)
	profile := s.ProfileFor(req.Client, req.User)
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
	sreq := searchRequestFor(t)
	early := newSearchPreparation(ctx, s, req, sreq, profile, pinID)
	defer early.close()
	ctx = context.WithValue(ctx, searchPreparationKey{}, early)
	var discovered []core.Release
	if s.discovered != nil && pin == nil && req.Prefer == "" && len(req.Exclude) == 0 {
		discovered = s.discovered(ctx, t)
		if len(discovered) > 0 {
			trace.Discovery = append(trace.Discovery, DiscoveryTrace{Path: "feed", Count: len(discovered)})
		}
		early.arrived(discovered)
		// A locally verified feed answer can avoid a search. A slow feed
		// gets only 100ms head start, then keeps checking alongside search.
		if len(discovered) > 0 && !early.disabled {
			deadline := time.NewTimer(100 * time.Millisecond)
			tick := time.NewTicker(5 * time.Millisecond)
		feedWait:
			for {
				if ready := early.readyResultsAfter(0); len(ready) > 0 {
					candidates, _, rankErr := s.rankCandidates(ctx, req, sreq, profile, ready, &trace, "")
					if rankErr == nil && len(candidates) > 0 {
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
	rescueTried := false
	if len(cands) == 0 && searchErr == nil && ctx.Err() == nil {
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
		if !clientRejected && len(req.Exclude) == 0 && searchErr == nil {
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
	if len(primary) > 0 {
		index, nzb, err := s.validateCandidates(ctx, primary, &trace, rcfg.HealthRace)
		if err != nil {
			if res := s.lastResort(ctx, inconclusive, t.Key, concat(lead, primary, downgrades), &trace); res != nil {
				return res, nil
			}
			trace.Err = err.Error()
			return nil, err
		}
		if index >= 0 {
			return s.acceptResolution(ctx, t.Key, primary, index, nzb, &trace), nil
		}
	}

	var rescuePrimary []candidate
	// Retry discovery once after the initially ranked releases failed health.
	if !rescueTried && searchErr == nil && ctx.Err() == nil {
		rescueCands, _, rejected, rerr := s.rescueCandidates(ctx, req, sreq, profile, results, &trace, pinID)
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
			return compareCandidates(downgrades[i], downgrades[j], profile)
		})
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
		if slices.ContainsFunc(downgrades, func(c candidate) bool { return c.rel.ID == res.Release.ID }) {
			trace.Downgrade = downgradeNote(reference, res.Release, len(primary) > 0)
		}
		return res, nil
	}
	if optimisticErr != nil {
		return nil, optimisticErr
	}
	if searchErr != nil {
		return nil, failed(nil)
	}
	if !clientRejected && len(req.Exclude) == 0 && len(cands) <= maxHealthAttempts {
		s.setAvailability(ctx, int64(t.Key), false, min(s.cooloff(), failedHealthTTL))
	}
	trace.Err = "no healthy candidates"
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

func (s *Service) search(ctx context.Context, req core.SearchRequest) ([]core.Release, error) {
	results, _, err := s.searchCached(ctx, req, false)
	return results, err
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
	results, err := rs.SearchRescue(ctx, req)
	if err != nil {
		return nil, ""
	}
	for i := range results {
		if results[i].Info.Title == "" && results[i].Info.Height == 0 && results[i].Info.Source == core.SourceUnknown {
			results[i].Info = releaseparse.Parse(results[i].Name)
		}
		results[i] = withTarget(results[i], req)
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
	s.mu.RLock()
	e, ok := s.searchCache[key]
	s.mu.RUnlock()
	if ok && !fresh && s.now().Before(e.expires) {
		return append([]core.Release(nil), e.results...), true, nil
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
	s.normalizeSearchResults(req, results)
	if partial {
		return results, false, nil
	} // Never cache an incomplete fleet answer.
	s.storeSearchCache(req, results)
	return results, false, nil
}

// rememberSearchResults normalizes a completed fleet answer and writes the
// search cache. Used when a progressive Play start left discovery running.
func (s *Service) rememberSearchResults(req core.SearchRequest, results []core.Release) {
	s.normalizeSearchResults(req, results)
	s.storeSearchCache(req, results)
}

func (s *Service) normalizeSearchResults(req core.SearchRequest, results []core.Release) {
	for i := range results {
		if results[i].Info.Title == "" && results[i].Info.Height == 0 && results[i].Info.Source == core.SourceUnknown {
			results[i].Info = releaseparse.Parse(results[i].Name)
		}
		// Target torrents at the wanted episode here, once, so every
		// consumer (ranking, pins, the console's release memory, variants)
		// sees the same per-episode release ID.
		results[i] = withTarget(results[i], req)
	}
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

// scoreCandidates filters and ranks search results for a request and
// profile. trace may be nil (the variants path); candidate.trace is then
// -1 and no trace rows are written.
func (s *Service) scoreCandidates(ctx context.Context, req core.ResolveRequest, sreq core.SearchRequest, profile core.QualityProfile, results []core.Release, trace *Trace) ([]candidate, bool, error) {
	return s.rankCandidates(ctx, req, sreq, profile, results, trace, "")
}

// rankCandidates is scoreCandidates with the owner's pinned release id
// (pinID, "" for none): the pin is judged by pinRejectReason's hard gates
// instead of the profile and identity rules, and is otherwise scored and
// traced like every other result. Interactive search reads the same trace.
func (s *Service) rankCandidates(ctx context.Context, req core.ResolveRequest, sreq core.SearchRequest, profile core.QualityProfile, results []core.Release, trace *Trace, pinID string) ([]candidate, bool, error) {
	t := req.Title
	policy, err := releasepolicy.Compile(profile.ReleaseRules)
	if err != nil {
		return nil, true, err
	}
	exclude := map[string]bool{}
	for _, id := range req.Exclude {
		exclude[id] = true
	}
	rcfg := s.resolverCfg()
	accepted := acceptedAudio(profile, req.User, s.catalogLanguage(), t)
	upscales := presumedUpscales(results, isAnime(t))
	var cands []candidate
	clientRejected := false
	for ri, rel := range results {
		rel = withTarget(rel, sreq)
		ct := CandidateTrace{ReleaseID: rel.ID, Name: rel.Name, Indexer: rel.Indexer, Size: rel.Size, Height: rel.Info.Height}
		pinned := pinID != "" && rel.ID == pinID
		ct.Pinned = pinned
		// Catalogue language is applied here, after the search cache.
		// The cache stores indexer results only; it does not freeze a
		// previous title's original language or a custom-format guess.
		policyResult := policy.Evaluate(rel, releasepolicy.Context{OriginalLanguage: titleOriginalLanguage(t)})
		if len(profile.ReleaseRules) > 0 {
			ct.Preferences = &policyResult
		}
		preferred := pinned || (req.Prefer != "" && rel.ID == req.Prefer)
		var reason string
		if pinned {
			reason = s.pinRejectReason(rel, sreq, req.Client, exclude)
		} else if !preferred && policyResult.Rejected {
			reason = preferenceReason(policyResult)
		} else {
			reason = s.rejectReason(rel, t, sreq, profile, rcfg, req.Client, exclude, preferred)
			if reason == "video codec unsupported by client" && req.ExactHEVCTranscode && !req.Client.Download && req.Prefer != "" && rel.ID == req.Prefer && strings.EqualFold(rel.Info.VideoCodec, "hevc") {
				// Only relax the codec gate for the explicitly selected source;
				// every other policy and the final probed output decision still apply.
				caps := req.Client
				caps.VideoCodecs = append(slices.Clone(caps.VideoCodecs), rel.Info.VideoCodec)
				reason = s.rejectReason(rel, t, sreq, profile, rcfg, caps, exclude, preferred)
			}
		}
		if reason != "" {
			if strings.HasPrefix(reason, "release preference:") {
				clientRejected = true
			}
			ct.Rejected = true
			switch reason {
			case "above profile max height", "above client max height", "below profile min height", "dolby vision not allowed for client", "video codec unsupported by client", "quality excluded by imported profile", "source recently failed health check; retrying later",
				// Size ranges are per profile: a remux too large for a
				// download or mobile profile still plays for others.
				"size too small for runtime", "size too large for runtime":
				clientRejected = true
			case "cam not allowed":
				// Another profile may allow cinema recordings. When none
				// do, the title is unavailable for everyone.
				if s.profileAllowsCam() {
					clientRejected = true
				}
			}
			ct.Reason = reason
			if trace != nil {
				trace.Candidates = append(trace.Candidates, ct)
			}
			continue
		}
		sc, err := s.scoreRelease(rel, profile, req.Client, t)
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
		if minimum := minFormatScore(rcfg, profile); minimum != nil && sc.format < *minimum && !preferred {
			clientRejected = true
			ct.Rejected = true
			ct.Reason = "below minimum custom format score"
			if trace != nil {
				trace.Candidates = append(trace.Candidates, ct)
			}
			continue
		}
		ct.CompatibilityReason = sc.compat
		rel.Score = sc.total
		ct.Score = rel.Score
		verified := s.recentlyVerified(ctx, rel.ID)
		if verified {
			ct.CompatibilityReason = strings.TrimSpace(sc.compat + "; source recently verified playable")
			ct.CompatibilityReason = strings.TrimPrefix(ct.CompatibilityReason, "; ")
		}
		tr := -1
		if trace != nil {
			trace.Candidates = append(trace.Candidates, ct)
			tr = len(trace.Candidates) - 1
		}
		langOK := audioAcceptable(rel.Info, accepted)
		lowBitrate := bitrateScore(sizedForEpisode(rel), t) < 0
		starved := lowBitrate || upscales[ri]
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
		}
		if !langOK && trace != nil {
			trace.Candidates[tr].CompatibilityReason = strings.TrimPrefix(trace.Candidates[tr].CompatibilityReason+"; ranked after releases in an accepted audio language", "; ")
		}
		cands = append(cands, candidate{policy: policyResult, rel: rel, trace: tr, verified: verified, langOK: langOK, starved: starved, quality: rankQuality(rel, profile, starved), bandwidth: bandwidth})
	}
	return s.orderCandidates(cands, profile, req.Title, trace), clientRejected, nil
}

func (s *Service) orderCandidates(cands []candidate, profile core.QualityProfile, title *core.Title, trace *Trace) []candidate {
	sort.SliceStable(cands, func(i, j int) bool {
		return compareCandidates(cands[i], cands[j], profile)
	})
	return s.routeSources(cands, title, trace)
}

// releaseScore is one release's total ranking score and its parts.
type releaseScore struct {
	total   float64
	format  int
	matches []FormatMatch
	compat  string
}

// scoreRelease computes the ranking score of a release for a profile and
// client: the profile score plus custom formats plus client compatibility.
func (s *Service) scoreRelease(rel core.Release, profile core.QualityProfile, caps core.ClientCaps, t *core.Title) (releaseScore, error) {
	s.cfgMu.RLock()
	scorer, scorerErr := s.scorer, s.scorerErr
	s.cfgMu.RUnlock()
	if scorerErr != nil {
		return releaseScore{}, scorerErr
	}
	// An untagged release is in the title's original language (as the
	// Starr apps assume), so "Language: Not English" does not match every
	// ordinary English release. A torrent season pack's total size is not
	// the episode's (sizedForEpisode), so size formats see it as unknown.
	formatRel := sizedForEpisode(rel)
	if len(formatRel.Info.Languages) == 0 {
		formatRel.Info.Languages = []string{assumedLanguage(t)}
	}
	formatScore, matches, err := scorer.ScoreForTitle(formatRel, profile.CustomFormatScores, originalLanguage(t))
	if err != nil {
		return releaseScore{}, err
	}
	compatibility, note := compatibilityScore(sizedForEpisode(rel), caps, t)
	return releaseScore{
		total:   s.score(sizedForEpisode(rel), profile, t) + float64(formatScore) + compatibility,
		format:  formatScore,
		matches: matches,
		compat:  note,
	}, nil
}

// minFormatScore is the profile's minimum custom format score, else the
// resolver-wide one; nil when neither is set.
func minFormatScore(rcfg config.Resolver, profile core.QualityProfile) *int {
	if profile.MinCustomFormatScore != nil {
		return profile.MinCustomFormatScore
	}
	return rcfg.MinCustomFormatScore
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
	if s.isBad(rel.ID) {
		return "marked bad"
	}
	if s.recentlyFailedHealth(rel.ID) {
		return "source recently failed health check; retrying later"
	}
	lowerName := strings.ToLower(rel.Name)
	for _, term := range rcfg.BlockedTerms {
		if term != "" && strings.Contains(lowerName, strings.ToLower(term)) {
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
	if reason := rejectIdentity(rel, t, sreq, s.catalogLanguage()); reason != "" {
		return reason
	}
	return rejectPolicy(rel, t, profile, caps, preferred, preferred && s.resolverCfg().OfferHigherVersions)
}

// rejectIdentity checks the result's reported identity without client policy.
// catalogLang is the language catalogue episode titles are in ("en-US").
func rejectIdentity(rel core.Release, t *core.Title, sreq core.SearchRequest, catalogLang string) string {
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
		matchesAlternativeTitle(rel.Info, sreq.AlternativeTitles)
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
		return "title mismatch"
	}
	if isEpisode {
		releaseYear := releaseparse.EpisodeSeriesYear(rel.Name, rel.Info.Year)
		if !releaseparse.SeriesYearMatches(releaseYear, sreq.Year) {
			return "series year mismatch"
		}
		if releaseYear == 0 && !idMatch && !localizedEpisodeTitle(rel.Info, t, catalogLang) && !releaseparse.EpisodeTitleMatches(rel.Name, sreq.EpisodeTitle) {
			return "episode title mismatch"
		}
		if rel.Info.FullSeason {
			// Usenet season packs are skipped in v1; a torrent pack streams
			// the wanted episode's file through debrid (Release.Target).
			if rel.Protocol != core.ProtocolTorrent {
				return "season pack (skipped in v1)"
			}
			if !packCoversSeason(rel.Info, sreq.Season) {
				return "season/episode mismatch"
			}
		} else if miniseriesPart > 0 {
			if miniseriesPart != sreq.Episode {
				return "season/episode mismatch"
			}
		} else if !releaseparse.EpisodeMatches(rel.Info, sreq.Season, sreq.Episode) &&
			!releaseparse.EpisodeMatchesAbsolute(rel.Info, absoluteEpisode(sreq)) {
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
			if q == releaseQuality(rel.Info) {
				allowed = true
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
			return "above client max height"
		}
		return "above profile max height"
	}
	if h := floorHeight(rel.Info); !preferred && profile.MinHeight > 0 && h > 0 && h < profile.MinHeight {
		return "below profile min height"
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
	if sreq.Type != core.TypeEpisode || rel.Protocol != core.ProtocolTorrent || sreq.Episode <= 0 {
		return rel
	}
	if rel.Target != nil {
		target := *rel.Target
		target.ShowYear, target.EpisodeTitle = sreq.Year, sreq.EpisodeTitle
		target.ExpectedIDs, target.ReportedIDs = sreq.IDs, rel.TitleIDs
		rel.Target = &target
		return rel // enrich cached identities without deriving their ID again
	}
	target := core.ReleaseTarget{Season: sreq.Season, Episode: sreq.Episode, ShowYear: sreq.Year, EpisodeTitle: sreq.EpisodeTitle, ExpectedIDs: sreq.IDs, ReportedIDs: rel.TitleIDs}
	rel.Target = &target
	if rel.Info.FullSeason {
		rel.ID = core.TargetReleaseID(rel.ID, target)
	}
	return rel
}

// sizedForEpisode hides a season pack's total size from per-runtime size
// checks and bitrate estimates.
func sizedForEpisode(rel core.Release) core.Release {
	if rel.Info.FullSeason && rel.Protocol == core.ProtocolTorrent {
		rel.Size = 0
	}
	return rel
}

// languageScore applies a profile's language preferences, which are
// opt-in (an unset profile is language-neutral) and may be ISO codes
// ("fr", "es-419") or release language names. Audio decides: a release
// declaring only other audio languages is pushed down, the preferred
// language or MULTI is not, and another regional variant of a preferred
// language (Castilian for a Latino preference) counts for less than the
// exact one. Subtitle-only declarations (VOSTFR) never stand in for audio;
// they add a small bonus of their own.
func languageScore(info core.ReleaseInfo, prefs []string) float64 {
	if len(prefs) == 0 {
		return 0
	}
	sc := 0.0
	if len(info.Languages) > 0 {
		ok := false
		for _, l := range info.Languages {
			if l == releaseparse.LanguageMulti {
				ok = true
				continue
			}
			best := releaseparse.LanguageNoMatch
			for _, pref := range prefs {
				best = max(best, releaseparse.LanguageMatch(l, pref))
			}
			switch best {
			case releaseparse.LanguageExactMatch:
				ok = true
				sc += 5
			case releaseparse.LanguageBaseMatch:
				ok = true
				sc += 2
			}
		}
		if !ok {
			sc -= 25
		}
	}
	for _, l := range info.SubtitleLanguages {
		matched := false
		for _, pref := range prefs {
			matched = matched || releaseparse.LanguageMatch(l, pref) == releaseparse.LanguageExactMatch
		}
		if matched {
			sc += 2
			break
		}
	}
	return sc
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

func indexScore[T comparable](list []T, v T, per float64) float64 {
	for i, x := range list {
		if x == v {
			return float64(len(list)-i) * per
		}
	}
	return 0
}

// score ranks an accepted release against a profile. Higher is better.
func (s *Service) score(rel core.Release, profile core.QualityProfile, t *core.Title) float64 {
	info := rel.Info
	var sc float64
	// Resolution fit, 0-50 points. An uncapped profile scales against 4K
	// so height stays one preference among the others, not the only one.
	maxH := float64(profile.MaxHeight)
	if maxH <= 0 {
		maxH = 2160
	}
	switch {
	case info.Height == 0:
	case profile.MaxHeight > 0 && info.Height == profile.MaxHeight:
		sc += 50
	case info.Height >= profile.MinHeight && (profile.MaxHeight == 0 || info.Height <= profile.MaxHeight):
		sc += 30 + 20*min(float64(info.Height)/maxH, 1)
	default: // below floor
		sc += 10 * float64(info.Height) / maxH
	}
	// Source / codec / audio preference order.
	if v := indexScore(profile.PreferredSources, info.Source, 10); v > 0 {
		sc += v
	} else if info.Source != core.SourceUnknown && info.Source != core.SourceCam {
		sc += 3
	}
	sc += indexScore(profile.PreferredCodecs, info.VideoCodec, 6)
	sc += indexScore(profile.PreferredAudio, info.AudioCodec, 4)
	if info.Atmos {
		sc += 3
	}
	// HDR/DV.
	if info.HDR10 {
		if profile.AllowHDR {
			sc += 8
		} else {
			sc -= 15
		}
	}
	if info.DolbyVision && profile.AllowDolbyVision {
		sc += 10
	}
	// Size sanity: in-range sizes get a small bonus, unknown size none.
	cfgMin, cfgMax := profile.SizeBoundsFor(t)
	if rel.Size > 0 && t.Runtime > 0 {
		mbPerMin := float64(rel.Size) / (1 << 20) / t.Runtime.Minutes()
		maxMBPerMin := effectiveMaxSizeMBPerMin(cfgMax, info)
		if (cfgMin == 0 || mbPerMin >= cfgMin) &&
			(maxMBPerMin == 0 || mbPerMin <= maxMBPerMin) {
			sc += 5
		}
	}
	if profile.PreferLargest && rel.Size > 0 {
		if (profile.MaxHeight > 0 && info.Height >= profile.MaxHeight) || (profile.MaxHeight == 0 && info.Height >= 2160) {
			sc += 100
		}
		if t != nil && t.Runtime > 0 {
			mbPerMin := float64(rel.Size) / (1 << 20) / t.Runtime.Minutes()
			ref := cfgMax
			if ref <= 0 {
				if maxH >= 2160 {
					ref = 800
				} else if maxH >= 1080 {
					ref = 200
				} else {
					ref = 80
				}
			}
			sc += 100 * min(mbPerMin/ref, 1)
		} else {
			gb := float64(rel.Size) / (1 << 30)
			sc += 100 * min(gb/80, 1)
		}
	}
	sc += bitrateScore(rel, t)
	// Age penalty.
	if !rel.PubDate.IsZero() {
		ageDays := s.now().Sub(rel.PubDate).Hours() / 24
		if ageDays > 0 {
			sc -= math.Min(ageDays/365*2, 15)
			if s.retentionDays > 0 && ageDays > float64(s.retentionDays) {
				sc -= 20
			}
		}
	}
	// Grabs (log scale).
	if rel.Grabs > 0 {
		sc += math.Min(math.Log10(float64(rel.Grabs)+1)*3, 9)
	}
	if info.Proper || info.Repack {
		sc += 6
	}
	sc += languageScore(info, profile.PreferredLanguages)
	// Indexer reputation.
	s.mu.RLock()
	rep := s.reps[rel.Indexer]
	s.mu.RUnlock()
	sc += rep.score()
	return sc
}
