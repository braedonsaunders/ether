package debrid

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/clusterbus"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/events"
	"github.com/braedonsaunders/ether/internal/nzb"
	"github.com/braedonsaunders/ether/internal/safefetch"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/vault"
)

func init() {
	// Portable SQL (no INSERT OR REPLACE; 0/1 booleans).
	store.Register("debrid", 1, `
CREATE TABLE debrid_payloads (
  release_key TEXT PRIMARY KEY,
  payload     TEXT NOT NULL,
  updated_at  INTEGER NOT NULL
);
CREATE TABLE debrid_torrents (
  provider_id TEXT NOT NULL,
  info_hash   TEXT NOT NULL,
  torrent_id  TEXT NOT NULL DEFAULT '',
  cached      INTEGER NOT NULL DEFAULT 0,
  checked_at  INTEGER NOT NULL,
  PRIMARY KEY (provider_id, info_hash)
);`)
}

const (
	defaultPayloadTTL  = 7 * 24 * time.Hour
	defaultCachedTTL   = 12 * time.Hour
	defaultUncachedTTL = 30 * time.Minute
	providerOpTimeout  = 45 * time.Second
	providerParallel   = 4
	authBackoff        = 15 * time.Minute
	maxBackoff         = 5 * time.Minute
	// linkMargin is how long before expiry a link counts as stale.
	linkMargin = 2 * time.Minute
)

// Options configure a Service.
type Options struct {
	AcquireStreamProvider func(context.Context, string) (func(), error)
	DB                    *sql.DB // nil keeps state in memory
	Logger                *slog.Logger
	Now                   func() time.Time
	// Provider options (tests point them at httptest servers).
	ProviderOptions ProviderOptions
	// NewProvider overrides provider construction (tests).
	NewProvider func(config.DebridProvider, ProviderOptions) (Provider, error)
	PayloadTTL  time.Duration // stored payload reuse, 0 = 7 days
	CachedTTL   time.Duration // known-cached memory, 0 = 12h
	UncachedTTL time.Duration // known-uncached memory, 0 = 30m
	// DirectGuard guards direct HTTP stream requests (tests map fake public
	// hosts onto local servers); nil uses a zero safefetch.Guard.
	DirectGuard   *safefetch.Guard
	IsIndexerHost func(string) bool
	// OnChange is notified when provider configuration or breaker state changes.
	OnChange func()
	// Events receives non-blocking provider.* domain events when set.
	Events *events.Bus
}

// Service holds the configured debrid providers and applies priority,
// backoff, bounded concurrency and caching to every provider call.
type Service struct {
	vault          vault.Store
	analysisTotals analysisCounters
	analysisMu     sync.Mutex
	analysis       map[string]*analysisAccount
	opts           Options
	log            *slog.Logger
	db             *sql.DB
	now            func() time.Time

	mu      sync.RWMutex
	cfg     config.Debrid
	entries []*entry          // configured providers in priority order
	plugins map[string]*entry // native plugin providers, not serialized into config

	// hinter holds the plugins answering which torrents a provider holds
	// (cache_hints.go); read without s.mu, before the hint deadline starts.
	hinter atomic.Pointer[CacheHinter]

	cdnEdges knownCDNEdges

	knownMu sync.Mutex
	known   map[string]knownState // providerID|hash

	payloadMetaMu sync.RWMutex
	payloadMeta   map[string]CachedFileMeta // payloadKey or bt:<hash> -> already-fetched file metadata

	queuedMu     sync.Mutex
	queued       map[string]queuedDownload // queueKey -> the download sent for that title
	queuedByHash map[string]string         // lowercase info hash -> queueKey (O(1) sibling lookup)

	direct directClients // direct HTTP streams (direct.go)

	mediaPausedMu sync.RWMutex
	mediaPaused   func(provider, rawURL string) error

	// Cluster (cluster.go); all nil/zero on a standalone server.
	clusterMu sync.RWMutex
	node      clusterbus.Node
	inval     *clusterbus.Invalidator
	origin    string
	unsubs    []func()
	share     func(configured int) int // concurrency share; nil = configured
}

type knownState struct {
	cached bool
	// refused marks a hash the provider refused (Real-Debrid's
	// infringing_file, a file it will not serve): it is not offered there
	// again, not even as a download, until the memory expires.
	refused bool
	// checkUncertain marks a negative verdict produced only by a zip-blind
	// CheckCached call (such as TorBox, whose checkcached reports 100+-file
	// server-side zips as not cached). Ordinary torrents still honour it for
	// UncachedTTL, while likely zip-only torrents ignore it and try the
	// cached-only add.
	checkUncertain bool
	// hint marks a state ApplyHints wrote from a source's marker, not the
	// provider's own answer; any answer recorded later replaces it.
	hint  bool
	until time.Time
}

// entry is one configured provider with its breaker and counters.
type entry struct {
	cfg config.DebridProvider
	p   Provider
	api *accountPacer
	err error // construction error (missing key, unknown kind)
	sem *limiter

	mu              sync.Mutex
	failures        int
	until           time.Time
	outageAt        time.Time
	state           string // "", "backoff", "auth_failed"
	lastError       string
	lastSpeedSample playbackSpeedSample
	stats           ProviderStats
	// Add refusals (ErrAddRefused): consecutive count, the latest, and the
	// pause on new torrents they caused.
	addRefusals int
	lastRefusal time.Time
	addsUntil   time.Time
}

const (
	// addRefusedTTL is how long a refused add skips that hash: short, since
	// a refusal may be the account's add throttle rather than the torrent.
	addRefusedTTL = 5 * time.Minute
	addPauseMin   = 2 * time.Minute
	addPauseMax   = 15 * time.Minute
)

// addRefused records a refused add. A second refusal in a row pauses new
// torrents on the provider (existing torrents and links keep working),
// doubling while refusals continue, so Ether does not keep provoking an
// account-level throttle.
func (e *entry) addRefused(now time.Time) time.Duration {
	e.mu.Lock()
	defer e.mu.Unlock()
	if now.Sub(e.lastRefusal) > addPauseMax {
		e.addRefusals = 0
	}
	e.addRefusals++
	e.lastRefusal = now
	if e.addRefusals >= 2 {
		duration := min(addPauseMin<<min(e.addRefusals-2, 3), addPauseMax)
		e.addsUntil = now.Add(duration)
		return duration
	}
	return 0
}

// addAccepted ends a run of refusals.
func (e *entry) addAccepted() {
	e.mu.Lock()
	e.addRefusals = 0
	e.mu.Unlock()
}

// addsPaused reports a pause on new torrents as not cached for now: the
// release is skipped on this provider without marking it.
func (e *entry) addsPaused(now time.Time) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if now.Before(e.addsUntil) {
		return fmt.Errorf("debrid provider %q is not adding torrents until %s: %w", displayName(e.cfg), e.addsUntil.Format(time.Kitchen), errAddsPaused)
	}
	return nil
}

// errAddsPaused is a release skipped while a provider's adds are paused.
var errAddsPaused = fmt.Errorf("debrid: adds paused after refusals: %w", core.ErrDownloadPaused)

// ProviderStats are one provider's counters and latency EWMAs.
type ProviderStats struct {
	ID               string        `json:"id"`
	Name             string        `json:"name"`
	Kind             string        `json:"kind"`
	Enabled          bool          `json:"enabled"`
	Priority         int           `json:"priority"`
	State            string        `json:"state"` // ok, backoff, auth_failed, disabled
	LastError        string        `json:"lastError,omitempty"`
	BackoffUntil     *time.Time    `json:"backoffUntil,omitempty"`
	RateLimitedUntil *time.Time    `json:"rateLimitedUntil,omitempty"`
	API              *APICallStats `json:"api,omitempty"`
	Requests         int64         `json:"requests"`
	Failures         int64         `json:"failures"`
	Cached           int64         `json:"cached"`
	NotCached        int64         `json:"notCached"`
	Blocked          int64         `json:"blocked"` // cached files the provider refused to serve
	LinksRefreshed   int64         `json:"linksRefreshed"`
	Reused           int64         `json:"reused"`
	// Slots is how many API calls this node makes to the provider at once:
	// AccountSlots on a standalone server, this node's share of it in a
	// cluster.
	Slots           int        `json:"slots"`
	AccountSlots    int        `json:"accountSlots"`
	QueuedDownloads int        `json:"queuedDownloads,omitempty"`
	QueuedDetail    string     `json:"queuedDetail,omitempty"`
	LastSuccess     *time.Time `json:"lastSuccess,omitempty"`
	Latency         Latency    `json:"latency"`
}

// Latency holds exponentially weighted averages of a provider's timings:
// API resolution (add/select/poll), link generation, CDN connect (TCP+TLS)
// and time to first byte of a range request, plus read throughput.
type Latency struct {
	ResolveMs     float64   `json:"resolveMs"`
	LinkMs        float64   `json:"linkMs"`
	ConnectMs     float64   `json:"connectMs"`
	TTFBMs        float64   `json:"ttfbMs"`
	ThroughputBps float64   `json:"throughputBps"`
	ThroughputAt  time.Time `json:"-"`
	Samples       int64     `json:"samples"`
}

const ewmaAlpha = 0.3

func ewma(cur, x float64, first bool) float64 {
	if first || cur == 0 {
		return x
	}
	return ewmaAlpha*x + (1-ewmaAlpha)*cur
}

// Status is the admin "debrid" status.
type Status struct {
	Enabled    bool            `json:"enabled"`
	CachedOnly bool            `json:"cachedOnly"`
	Providers  []ProviderStats `json:"providers"`
	Engine     *EngineStats    `json:"engine,omitempty"`
}

// NewService builds a Service from the debrid config section.
func NewService(cfg config.Debrid, opts Options) *Service {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.NewProvider == nil {
		opts.NewProvider = NewProvider
	}
	if opts.PayloadTTL <= 0 {
		opts.PayloadTTL = defaultPayloadTTL
	}
	if opts.CachedTTL <= 0 {
		opts.CachedTTL = defaultCachedTTL
	}
	if opts.UncachedTTL <= 0 {
		opts.UncachedTTL = defaultUncachedTTL
	}
	if opts.ProviderOptions.Now == nil {
		opts.ProviderOptions.Now = opts.Now
	}
	s := &Service{opts: opts, log: opts.Logger, db: opts.DB, now: opts.Now, known: map[string]knownState{}, payloadMeta: map[string]CachedFileMeta{}}
	s.opts.ProviderOptions.paces = &accountPaces{onPause: s.publishAccountPause, share: s.slotsFor, answers: &answerStore{db: opts.DB}}
	s.direct.guard = opts.DirectGuard
	s.UpdateConfig(cfg)
	s.preloadPayloadMeta()
	return s
}

// UpdateConfig hot-applies an edited debrid section. Providers whose
// identity and key are unchanged keep their breaker state and counters.
func (s *Service) notifyChange() {
	if s != nil && s.opts.OnChange != nil {
		s.opts.OnChange()
	}
}

func (s *Service) UpdateConfig(cfg config.Debrid) {
	defer s.notifyChange()
	s.mu.Lock()
	defer s.mu.Unlock()
	old := map[string]*entry{}
	for _, e := range s.entries {
		old[e.cfg.ID+"|"+e.cfg.Kind+"|"+e.cfg.APIKey] = e
	}
	next := make([]*entry, 0, len(cfg.Providers))
	for i, pc := range cfg.Providers {
		if pc.ID == "" {
			pc.ID = fmt.Sprintf("%s-%d", pc.Kind, i)
		}
		if e, ok := old[pc.ID+"|"+pc.Kind+"|"+pc.APIKey]; ok && (!pc.Enabled || e.p != nil) {
			e.mu.Lock()
			e.cfg = pc
			e.mu.Unlock()
			next = append(next, e)
			continue
		}
		e := &entry{cfg: pc, sem: newLimiter(s.slotsFor(providerParallel))}
		s.wireLimiterEvents(e)
		if pc.Enabled {
			e.p, e.err = s.opts.NewProvider(pc, s.opts.ProviderOptions)
			switch p := e.p.(type) {
			case pacedProvider:
				e.api = p.accountPace()
			case interface{ apiAccount() *accountPacer }:
				e.api = p.apiAccount()
			}
		}
		next = append(next, e)
	}
	pluginIDs := make([]string, 0, len(s.plugins))
	for id := range s.plugins {
		pluginIDs = append(pluginIDs, id)
	}
	sort.Strings(pluginIDs)
	for _, id := range pluginIDs {
		next = append(next, s.plugins[id])
	}
	sort.SliceStable(next, func(i, j int) bool { return next[i].cfg.Priority < next[j].cfg.Priority })
	s.cfg = cfg
	s.entries = next
}

// Config returns the current debrid section.
func (s *Service) Config() config.Debrid {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// active returns enabled, constructible providers in priority order.
func (s *Service) active() []*entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*entry
	for _, e := range s.entries {
		if e.cfg.Enabled && e.p != nil && e.err == nil {
			out = append(out, e)
		}
	}
	return out
}

func (s *Service) entryByID(id string) *entry {
	for _, e := range s.active() {
		if e.cfg.ID == id {
			return e
		}
	}
	return nil
}

// Enabled reports whether any provider can serve torrents.
func (s *Service) Enabled() bool { return len(s.active()) > 0 }

// ---------------------------------------------------------------------------
// Breaker and counters.

func (e *entry) blocked(now time.Time) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if now.Before(e.until) {
		if e.state == "auth_failed" {
			// Keep the cause classifiable: playback tells the viewer the
			// key was rejected rather than that nothing was found.
			return fmt.Errorf("debrid provider %q paused until %s (%s): %w", displayName(e.cfg), e.until.Format(time.Kitchen), e.lastError, ErrAuth)
		}
		return fmt.Errorf("debrid provider %q paused until %s (%s)", displayName(e.cfg), e.until.Format(time.Kitchen), e.lastError)
	}
	return nil
}

func (e *entry) acquire(ctx context.Context) error {
	adapter, paced := e.p.(pacedProvider)
	if paced && lowPriority(ctx) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !e.sem.tryAcquire(1) {
			return errBackgroundUndecided
		}
		return nil
	}
	if paced {
		pace := adapter.accountPace()
		if err := pace.cooldownFor(ctx); err != nil {
			return err
		}
	}
	if err := e.sem.acquire(ctx); err != nil {
		if paced && errors.Is(err, context.DeadlineExceeded) {
			if throttled := adapter.accountPace().cooldownFor(ctx); throttled != nil {
				return throttled
			}
		}
		return err
	}
	return nil
}

func (e *entry) release() { e.sem.release() }

// observe updates the breaker after a provider call. It returns the new
// pause when the call put the provider into back-off, a clear event when a
// success ended an earlier pause, and nil otherwise (cluster.go broadcasts
// the events).
func (e *entry) observe(now time.Time, err error) *backoffEvent {
	ev, _, _ := e.observeTransition(now, err)
	return ev
}

func (e *entry) observeTransition(now time.Time, err error) (ev *backoffEvent, prevState string, outageDur time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	prevState = e.state
	e.stats.Requests++
	if err == nil || errors.Is(err, core.ErrNotCached) || errors.Is(err, core.ErrWrongContent) || errors.Is(err, ErrGone) || errors.Is(err, core.ErrReleaseRejected) || errors.Is(err, core.ErrSourceUnavailable) {
		// The provider answered; release-level outcomes and explicitly
		// transient ones (a link still being generated, a torrent's
		// server under maintenance) are not outages.
		was := e.state
		e.failures = 0
		if !now.Before(e.until) {
			// A call that was already in flight when a pause began does not
			// end it (nor blank its reported state) early.
			e.state = ""
		}
		if err == nil {
			t := now
			e.stats.LastSuccess = &t
		}
		if was != "" && !now.Before(e.until) {
			if !e.outageAt.IsZero() {
				outageDur = max(time.Duration(0), now.Sub(e.outageAt))
				e.outageAt = time.Time{}
			}
			return &backoffEvent{Provider: e.cfg.ID}, prevState, outageDur
		}
		return nil, prevState, 0
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, core.ErrForegroundOnly) || errors.Is(err, core.ErrBudgetExhausted) || errors.Is(err, core.ErrDebridThrottled) {
		return nil, prevState, 0
	}
	e.stats.Failures++
	e.lastError = cleanDiagnostic(err.Error(), e.cfg.APIKey)
	var rl *rateLimitError
	switch {
	case errors.Is(err, ErrAuth):
		e.state, e.until = "auth_failed", now.Add(authBackoff)
	case errors.As(err, &rl) || errors.Is(err, ErrRateLimited):
		d := 30 * time.Second
		if rl != nil && rl.retryAfter > 0 {
			d = min(rl.retryAfter, maxBackoff)
		}
		e.state, e.until = "backoff", now.Add(d)
	default:
		e.failures++
		if e.failures < 3 {
			return nil, prevState, 0
		}
		d := min(maxBackoff, 30*time.Second<<min(e.failures-3, 4))
		e.state, e.until = "backoff", now.Add(d)
	}
	if e.outageAt.IsZero() {
		e.outageAt = now
	}
	return &backoffEvent{Provider: e.cfg.ID, State: e.state, Until: e.until.UnixMilli(), Error: shortError(e.lastError)}, prevState, 0
}

func (e *entry) count(fn func(*ProviderStats)) {
	e.mu.Lock()
	fn(&e.stats)
	e.mu.Unlock()
}

// Stats reports every configured provider.
func (s *Service) Stats() []ProviderStats {
	s.mu.RLock()
	entries := append([]*entry(nil), s.entries...)
	s.mu.RUnlock()
	now := s.now()
	queuedCounts, queuedBehind := s.queuedStats(now)
	out := make([]ProviderStats, 0, len(entries))
	for _, e := range entries {
		e.mu.Lock()
		st := e.stats
		st.ID, st.Name, st.Kind, st.Enabled, st.Priority = e.cfg.ID, displayName(e.cfg), e.cfg.Kind, e.cfg.Enabled, e.cfg.Priority
		st.LastError = e.lastError
		if e.api != nil {
			api := e.api.apiStats()
			st.API = &api
		}
		st.Slots, st.AccountSlots = e.sem.size(), providerParallel
		st.QueuedDownloads = queuedCounts[e.cfg.ID]
		if n := queuedBehind[e.cfg.ID]; n > 0 {
			st.QueuedDetail = (&core.DownloadQueuedError{
				Provider:        st.Name,
				ActiveDownloads: n,
			}).Detail()
		}
		switch {
		case !e.cfg.Enabled:
			st.State = "disabled"
		case e.err != nil:
			st.State, st.LastError = "auth_failed", e.err.Error()
		case now.Before(e.until):
			st.State = e.state
			u := e.until
			st.BackoffUntil = &u
		default:
			st.State = "ok"
		}
		if p, ok := e.p.(pacedProvider); ok && st.State != "disabled" && st.State != "auth_failed" {
			if until := p.accountPace().retryAt(); time.Now().Before(until) {
				st.State, st.RateLimitedUntil = "backoff", &until
				if st.BackoffUntil == nil || until.After(*st.BackoffUntil) {
					st.BackoffUntil = &until
				}
			}
		}
		e.mu.Unlock()
		out = append(out, st)
	}
	return out
}

// Latencies returns each enabled provider's latency EWMAs by provider id,
// for source routing.
func (s *Service) Latencies() map[string]Latency {
	out := map[string]Latency{}
	for _, e := range s.active() {
		e.mu.Lock()
		out[e.cfg.ID] = e.stats.Latency
		e.mu.Unlock()
	}
	return out
}

// Status is the admin status document (engine stats are filled in by the
// caller when an Engine is wired).
func (s *Service) Status() Status {
	cfg := s.Config()
	return Status{Enabled: s.Enabled(), CachedOnly: cfg.CachedOnly, Providers: s.Stats()}
}

func (s *Service) noteLatency(providerID string, fn func(*Latency)) {
	s.mu.RLock()
	var e *entry
	for _, x := range s.entries {
		if x.cfg.ID == providerID {
			e = x
		}
	}
	s.mu.RUnlock()
	if e != nil {
		e.count(func(st *ProviderStats) { fn(&st.Latency) })
	}
}

// RecordStream feeds CDN timings observed by the engine into the provider's
// EWMAs (connect and ttfb may be zero when unknown).
func (s *Service) RecordStream(providerID string, connect, ttfb time.Duration, bytes int64, elapsed time.Duration) {
	s.noteLatency(providerID, func(l *Latency) {
		first := l.Samples == 0
		if connect > 0 {
			l.ConnectMs = ewma(l.ConnectMs, ms(connect), first)
		}
		if ttfb > 0 {
			l.TTFBMs = ewma(l.TTFBMs, ms(ttfb), first)
		}
		if bytes >= 256<<10 && elapsed > 0 {
			l.ThroughputBps = ewma(l.ThroughputBps, float64(bytes)/elapsed.Seconds(), first)
			l.ThroughputAt = s.now()
		}
		l.Samples++
	})
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// ---------------------------------------------------------------------------
// Known cached / uncached hashes.

// setKnown remembers a provider's answer for a hash and tells the other
// cluster nodes to re-read it.
func (s *Service) setKnown(ctx context.Context, providerID, hash, torrentID string, cached bool) {
	s.notifyKnown(s.recordKnown(ctx, providerID, hash, torrentID, cached))
}

// recordKnown is setKnown without the cluster notification (bulk callers
// notify once for many hashes); it returns the hash's known-map key.
func (s *Service) recordKnown(ctx context.Context, providerID, hash, torrentID string, cached bool) string {
	return s.recordKnownState(ctx, providerID, hash, torrentID, cached, false)
}

func (s *Service) recordKnownState(ctx context.Context, providerID, hash, torrentID string, cached, checkUncertain bool) string {
	hash = strings.ToLower(hash)
	if cached {
		checkUncertain = false
	}
	ttl := s.opts.UncachedTTL
	if cached {
		ttl = s.opts.CachedTTL
	}
	now := s.now()
	key := knownKey(providerID, hash)
	s.knownMu.Lock()
	s.sweepKnownLocked(now)
	s.known[key] = knownState{cached: cached, checkUncertain: checkUncertain, until: now.Add(ttl)}
	s.knownMu.Unlock()
	if s.db == nil {
		return key
	}
	cachedInt := 0
	if cached {
		cachedInt = 1
	} else if checkUncertain {
		cachedInt = 2
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO debrid_torrents (provider_id, info_hash, torrent_id, cached, checked_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (provider_id, info_hash) DO UPDATE SET
  torrent_id = CASE WHEN excluded.torrent_id <> '' THEN excluded.torrent_id ELSE debrid_torrents.torrent_id END,
  cached = excluded.cached,
  checked_at = excluded.checked_at`, providerID, hash, torrentID, cachedInt, now.Unix())
	if err != nil {
		s.log.Debug("debrid: record torrent", "err", err)
	}
	return key
}

// knownFor reports a remembered answer for provider+hash.
func (s *Service) knownFor(ctx context.Context, providerID, hash string) (cached, ok bool, torrentID string) {
	cached, ok, _, torrentID = s.knownForDetail(ctx, providerID, hash)
	return cached, ok, torrentID
}

func (s *Service) knownForDetail(ctx context.Context, providerID, hash string) (cached, ok, checkUncertain bool, torrentID string) {
	hash = strings.ToLower(hash)
	now := s.now()
	s.knownMu.Lock()
	k, hit := s.known[knownKey(providerID, hash)]
	s.knownMu.Unlock()
	if hit && (now.Before(k.until) || (k.refused && k.until.IsZero())) {
		cached, ok, checkUncertain = k.cached, true, k.checkUncertain
	}
	if s.db != nil {
		var tid string
		var c int
		var at int64
		if err := s.db.QueryRowContext(ctx, `SELECT torrent_id, cached, checked_at FROM debrid_torrents WHERE provider_id = ? AND info_hash = ?`,
			providerID, hash).Scan(&tid, &c, &at); err == nil {
			torrentID = tid
			if !ok {
				ttl := s.opts.UncachedTTL
				if c == 1 {
					ttl = s.opts.CachedTTL
				}
				if now.Sub(time.Unix(at, 0)) < ttl {
					cached, ok, checkUncertain = c == 1, true, c == 2
				}
			}
		}
	}
	return cached, ok, checkUncertain, torrentID
}

// knownAvailability reads the verdicts used during bulk candidate discovery.
// Torrent IDs are only needed for the acquisition path, which reads the
// current row when it makes an add or reuse decision.
func (s *Service) knownAvailability(ctx context.Context, e *entry, hashes []string) map[string]bool {
	providerID := e.serviceRef().ID
	out := make(map[string]bool, len(hashes))
	now := s.now()
	var missing []string
	s.knownMu.Lock()
	for _, hash := range hashes {
		hash = strings.ToLower(hash)
		if e.refusesHash(hash) {
			out[hash] = false
			continue
		}
		if k, hit := s.known[knownKey(providerID, hash)]; hit && (now.Before(k.until) || k.refused && k.until.IsZero()) {
			out[hash] = k.cached
		} else {
			missing = append(missing, hash)
		}
	}
	s.knownMu.Unlock()
	if s.db == nil {
		return out
	}
	slices.Sort(missing)
	missing = slices.Compact(missing)
	for chunk := range store.Chunks(missing, 500) {
		args := make([]any, 1, len(chunk)+1)
		args[0] = providerID
		for _, hash := range chunk {
			args = append(args, hash)
		}
		rows, err := s.db.QueryContext(ctx, `SELECT info_hash, cached, checked_at FROM debrid_torrents WHERE provider_id = ? AND info_hash IN (`+store.Placeholders(len(chunk))+`)`, args...)
		if err != nil {
			break
		}
		for rows.Next() {
			var hash string
			var cached int
			var at int64
			if rows.Scan(&hash, &cached, &at) != nil {
				continue
			}
			ttl := s.opts.UncachedTTL
			if cached == 1 {
				ttl = s.opts.CachedTTL
			}
			if now.Sub(time.Unix(at, 0)) < ttl {
				out[hash] = cached == 1
			}
		}
		rows.Close()
	}
	return out
}

func zipOnlyCheckThreshold(p Provider) int {
	if z, ok := p.(zipOnlyCheckBlindProvider); ok {
		return z.zipOnlyCheckThreshold()
	}
	return 0
}

// likelyZipOnly reports whether rel is a public torrent at or above e's
// zip-only cache-check blindness threshold (e.g. TorBox's 100-file auto-zip),
// using metadata on rel or previously recorded torrent files in the vault.
func (s *Service) likelyZipOnly(ctx context.Context, e *entry, rel core.Release) bool {
	if rel.Private || e == nil {
		return false
	}
	thresh := zipOnlyCheckThreshold(e.p)
	if thresh <= 0 {
		return false
	}
	if rel.KnownFileCount() >= thresh {
		return true
	}
	if s.vault != nil {
		if hash := strings.ToLower(strings.TrimSpace(rel.InfoHash)); hash != "" {
			if rec, err := s.vault.Get(ctx, vault.TorrentKey(hash)); err == nil && rec != nil && rec.Torrent != nil && len(rec.Torrent.Files) >= thresh {
				return true
			}
		}
	}
	return false
}

// knownUncachedForRelease reports whether e is definitively known not to hold
// rel cached. A negative CheckCached verdict from a zip-blind provider is
// ignored for likely zip-only public torrents so Acquire/ListReleaseFiles
// tries the cached-only add.
func (s *Service) knownUncachedForRelease(ctx context.Context, e *entry, rel core.Release) bool {
	hash := strings.ToLower(strings.TrimSpace(rel.InfoHash))
	if hash == "" {
		return false
	}
	if e.api != nil {
		a := e.api.answer(ctx, answerKey{hash: hash}, e.api.now())
		if a.verdict != "" {
			refused := a.refusal() != nil
			if refused {
				e.api.noteRemembered()
			}
			return refused
		}
	}
	if _, paced := e.p.(pacedProvider); paced {
		return false
	}
	cached, ok, checkUncertain, _ := s.knownForDetail(ctx, e.cfg.ID, hash)
	if !ok || cached {
		return false
	}
	if checkUncertain && s.likelyZipOnly(ctx, e, rel) {
		return false
	}
	return true
}

// mayBeZipOnlyCached reports whether a public torrent that CheckCached marked
// uncached may still be held as a server-side zip by an active zip-blind
// provider whose cached-only add has not yet rejected it.
func (s *Service) mayBeZipOnlyCached(ctx context.Context, rel core.Release) bool {
	if rel.Private || rel.Protocol != core.ProtocolTorrent {
		return false
	}
	hash := strings.ToLower(strings.TrimSpace(rel.InfoHash))
	if hash == "" {
		return false
	}
	for _, e := range s.active() {
		if e.blocked(s.now()) != nil {
			continue
		}
		if !s.likelyZipOnly(ctx, e, rel) {
			continue
		}
		cached, ok, checkUncertain, _ := s.knownForDetail(ctx, e.cfg.ID, hash)
		if !ok || cached || checkUncertain {
			return true
		}
	}
	return false
}

// hintTTL bounds how long a source's cached marker stands in for a
// provider's own answer.
const hintTTL = 30 * time.Minute

// ApplyHints records the cached markers sources put on torrent releases
// (core.Release.DebridCached and DebridUncached) for providers that have
// no answer of their own about the hash. A provider the source reported as
// holding the torrent is tried ahead of unknown ones; with CachedOnly, one
// it reported as not holding it is skipped without adding the torrent.
// Hints live in memory only and never replace a provider's answer. Of the
// per-account answers acquisition remembers (read in one query per
// account), a "not cached" or refusal blocks any hint and any other answer
// blocks a negative one; a positive the account remembers as cached stands.
// Nothing is written once ctx has ended: reads it cut short may have missed
// an answer.
func (s *Service) ApplyHints(ctx context.Context, rels []core.Release) {
	cachedOnly := s.Config().CachedOnly
	for _, e := range s.active() {
		kind := e.kind()
		var hashes []string
		hints := map[string]bool{}
		for _, r := range rels {
			if r.Protocol != core.ProtocolTorrent || r.InfoHash == "" {
				continue
			}
			cached := slices.Contains(r.DebridCached, kind)
			if !cached && (!cachedOnly || !slices.Contains(r.DebridUncached, kind)) {
				continue
			}
			hash := strings.ToLower(r.InfoHash)
			if _, seen := hints[hash]; !seen {
				hashes = append(hashes, hash)
				hints[hash] = cached
			}
		}
		known := s.knownAvailability(ctx, e, hashes)
		var unknown []string
		for _, hash := range hashes {
			if _, ok := known[hash]; !ok {
				unknown = append(unknown, hash)
			}
		}
		for hash, a := range e.rememberedAnswers(ctx, unknown) {
			if a.refusal() != nil || !hints[hash] {
				known[hash] = false
			}
		}
		now := s.now()
		s.knownMu.Lock()
		if ctx.Err() != nil {
			s.knownMu.Unlock()
			return
		}
		s.sweepKnownLocked(now)
		for _, hash := range hashes {
			key := knownKey(e.cfg.ID, hash)
			k, hit := s.known[key]
			if _, ok := known[hash]; ok || hit && (now.Before(k.until) || k.refused && k.until.IsZero()) {
				continue
			}
			s.known[key] = knownState{cached: hints[hash], hint: true, until: now.Add(hintTTL)}
		}
		s.knownMu.Unlock()
	}
}

// markedFirst returns entries with those marked as holding the release moved
// ahead, owner order kept within each group: the release's own marker
// (rel.DebridCached), or a live cached hint ApplyHints recorded for that
// provider and hash. A provider's own "not cached" answer outranks either.
// Without a marker or hint the order is unchanged: answers from bulk checks
// alone never reorder.
func (s *Service) markedFirst(ctx context.Context, entries []*entry, rel core.Release) []*entry {
	hash := strings.ToLower(rel.InfoHash)
	now := s.now()
	hinted := func(e *entry) bool {
		e.mu.Lock() // UpdateConfig replaces cfg under it
		id, kind := e.cfg.ID, e.cfg.Kind
		e.mu.Unlock()
		if slices.Contains(rel.DebridCached, kind) {
			return true
		}
		if hash == "" {
			return false
		}
		s.knownMu.Lock()
		k, ok := s.known[knownKey(id, hash)]
		s.knownMu.Unlock()
		return ok && k.hint && k.cached && now.Before(k.until)
	}
	marked := make([]*entry, 0, len(entries))
	var rest []*entry
	for _, e := range entries {
		if hinted(e) && !s.knownUncachedForRelease(ctx, e, rel) {
			marked = append(marked, e)
		} else {
			rest = append(rest, e)
		}
	}
	return append(marked, rest...)
}

func (e *entry) kind() string {
	e.mu.Lock()
	kind := e.cfg.Kind
	e.mu.Unlock()
	return kind
}

const knownSweepThreshold = 4096

// sweepKnownLocked removes expired entries from s.known when the map grows
// past knownSweepThreshold. Permanent refusals (zero until) are kept.
func (s *Service) sweepKnownLocked(now time.Time) {
	if len(s.known) < knownSweepThreshold {
		return
	}
	for k, v := range s.known {
		if !v.until.IsZero() && !now.Before(v.until) {
			delete(s.known, k)
		}
	}
}

func (e *entry) serviceRef() core.DebridServiceRef {
	e.mu.Lock()
	cfg := e.cfg
	e.mu.Unlock()
	return core.DebridServiceRef{
		ID:   cfg.ID,
		Name: displayName(cfg),
	}
}

// ServiceRef returns the configured debrid service's id and display name for
// a provider id (e.g. from a decoded debrid Payload). Falls back to kind when
// the provider is no longer present in the active list.
func (s *Service) ServiceRef(id, kind string) (core.DebridServiceRef, bool) {
	id = strings.TrimSpace(id)
	kind = strings.TrimSpace(kind)
	if id == "" {
		return core.DebridServiceRef{}, false
	}
	s.mu.RLock()
	entries := slices.Clone(s.entries)
	s.mu.RUnlock()
	for _, e := range entries {
		ref := e.serviceRef()
		if ref.ID == id {
			return ref, true
		}
	}
	return core.DebridServiceRef{
		ID:   id,
		Name: displayName(config.DebridProvider{ID: id, Kind: kind}),
	}, true
}

// KnownCached reports, without any API call, whether an enabled provider
// recently reported the hash as cached.
func (s *Service) KnownCached(hash string) bool {
	hash = strings.ToLower(hash)
	now := s.now()
	entries := s.active()
	s.knownMu.Lock()
	defer s.knownMu.Unlock()
	for _, e := range entries {
		if e.refusesHash(hash) {
			continue
		}
		if k, ok := s.known[knownKey(e.cfg.ID, hash)]; ok && k.cached && now.Before(k.until) {
			return true
		}
	}
	return false
}

// KnownCachedBy returns, without any API call, the enabled debrid services
// that recently reported hash as cached, in acquisition priority order.
// When hintKinds is non-empty and an enabled provider has no fresh verdict in
// Ether's cache, a provider whose kind matches hintKinds is also included.
func (s *Service) KnownCachedBy(hash string, hintKinds []string) []core.DebridServiceRef {
	hash = strings.ToLower(strings.TrimSpace(hash))
	entries := s.active()
	if len(entries) == 0 {
		return nil
	}
	cfgs := make([]config.DebridProvider, len(entries))
	for i, e := range entries {
		e.mu.Lock()
		cfgs[i] = e.cfg
		e.mu.Unlock()
	}
	now := s.now()
	var out []core.DebridServiceRef
	s.knownMu.Lock()
	defer s.knownMu.Unlock()
	for i, cfg := range cfgs {
		if entries[i].refusesHash(hash) {
			continue
		}
		if hash != "" {
			if k, ok := s.known[knownKey(cfg.ID, hash)]; ok && now.Before(k.until) {
				if k.cached {
					out = append(out, core.DebridServiceRef{ID: cfg.ID, Name: displayName(cfg)})
				}
				continue
			}
		}
		if len(hintKinds) > 0 && slices.Contains(hintKinds, cfg.Kind) {
			out = append(out, core.DebridServiceRef{ID: cfg.ID, Name: displayName(cfg)})
		}
	}
	return out
}

// CheckCached asks providers with bulk availability checks about hashes,
// remembering the answers. It returns hash -> cached on some provider, and
// definitive=true when every enabled provider could answer (so a false
// entry means no provider has the torrent).
func (s *Service) CheckCached(ctx context.Context, hashes []string) (map[string]bool, bool) {
	hits, _, def := s.checkCachedDetail(ctx, hashes, false)
	return hits, def
}

// CheckCachedBy asks providers with bulk availability checks about hashes,
// remembering the answers. It returns hash -> the enabled debrid services
// holding that hash in acquisition priority order (omitting uncached hashes),
// and definitive=true when every enabled provider could answer.
func (s *Service) CheckCachedBy(ctx context.Context, hashes []string) (map[string][]core.DebridServiceRef, bool) {
	_, by, def := s.checkCachedDetail(ctx, hashes, false)
	return by, def
}

// VerifyCached asks configured providers directly for an owner-requested check.
func (s *Service) VerifyCached(ctx context.Context, hash string) (bool, bool) {
	hits, _, def := s.checkCachedDetail(ctx, []string{hash}, true)
	cached, seen := hits[hash]
	return cached, seen && (cached || def)
}

func (s *Service) checkCachedDetail(ctx context.Context, hashes []string, refresh bool) (map[string]bool, map[string][]core.DebridServiceRef, bool) {
	out := map[string]bool{}
	by := map[string][]core.DebridServiceRef{}
	entries := s.active()
	if len(hashes) == 0 || len(entries) == 0 || ctx.Err() != nil {
		return out, by, false
	}
	addService := func(h string, ref core.DebridServiceRef) {
		for _, existing := range by[h] {
			if existing.ID == ref.ID {
				return
			}
		}
		by[h] = append(by[h], ref)
	}
	definitive := true
	for _, e := range entries {
		if e.blocked(s.now()) != nil {
			definitive = false
			continue
		}
		ref := e.serviceRef()
		var ask []string
		var known map[string]bool
		if !refresh {
			known = s.knownAvailability(ctx, e, hashes)
		}
		for _, h := range hashes {
			if c, ok := known[strings.ToLower(h)]; ok {
				out[h] = out[h] || c
				if c {
					addService(h, ref)
				}
				continue
			}
			ask = append(ask, h)
		}
		if len(ask) == 0 {
			continue
		}
		if err := e.acquire(ctx); err != nil {
			return out, by, false
		}
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		res, err := e.p.CheckCached(cctx, ask)
		cancel()
		e.release()
		if errors.Is(err, ErrUnsupported) {
			definitive = false
			continue
		}
		if err != nil && ctx.Err() != nil {
			// A caller's short discovery budget says nothing about the
			// provider's availability. Do not open its circuit for that.
			return out, by, false
		}
		s.observe(e, err)
		if err != nil {
			definitive = false
			continue
		}
		keys := make([]string, 0, len(ask))
		zipBlind := zipOnlyCheckThreshold(e.p) > 0
		for _, h := range ask {
			c := res[strings.ToLower(h)]
			out[h] = out[h] || c
			if c {
				addService(h, ref)
			}
			keys = append(keys, s.recordKnownState(ctx, ref.ID, h, "", c, !c && zipBlind))
		}
		s.notifyKnown(keys...)
	}
	return out, by, definitive
}

// ---------------------------------------------------------------------------
// Payload persistence.

func payloadKey(rel core.Release) string {
	k := rel.ID
	if rel.Target != nil {
		k += fmt.Sprintf("|s%de%d", rel.Target.Season, rel.Target.Episode)
		if len(rel.Target.MovieTitles) > 0 {
			k += fmt.Sprintf("|movie:%q:%d", rel.Target.MovieTitles, rel.Target.MovieYear)
		}
		if len(rel.Target.WantedLanguages) > 0 || rel.Target.OriginalLanguage != "" {
			k += fmt.Sprintf("|lang:%q:%s", rel.Target.WantedLanguages, rel.Target.OriginalLanguage)
		}
		if rel.Target.FileIndex != nil {
			k += fmt.Sprintf("|fileIndex:%d", *rel.Target.FileIndex)
		}
		if rel.Target.ExactFile != "" {
			k += fmt.Sprintf("|exactFile:%q", rel.Target.ExactFile)
		}
		if rel.Target.AllowNonVideo {
			k += "|allowNonVideo"
		}
		if rel.Target.Member != "" {
			k += fmt.Sprintf("|member:%q", rel.Target.Member)
		}
		if rel.Target.MemberIndex != nil {
			k += fmt.Sprintf("|memberIndex:%d", *rel.Target.MemberIndex)
		}
		if rel.Target.MIME != "" {
			k += fmt.Sprintf("|mime:%s", nzb.NormalizeMIME(rel.Target.MIME))
		}
	}
	return k
}

func (s *Service) storedPayload(ctx context.Context, key string) (Payload, bool) {
	if s.db == nil || key == "" {
		return Payload{}, false
	}
	var raw string
	var at int64
	if err := s.db.QueryRowContext(ctx, `SELECT payload, updated_at FROM debrid_payloads WHERE release_key = ?`, key).Scan(&raw, &at); err != nil {
		return Payload{}, false
	}
	if s.now().Sub(time.Unix(at, 0)) > s.opts.PayloadTTL {
		return Payload{}, false
	}
	p, err := DecodePayload([]byte(raw))
	return p, err == nil
}

func (s *Service) savePayload(ctx context.Context, p Payload) {
	s.rememberCDNEdge(ctx, p)
	s.rememberPayloadMeta(p)
	if s.db == nil || p.Key == "" {
		return
	}
	_, err := s.db.ExecContext(context.WithoutCancel(ctx), `INSERT INTO debrid_payloads (release_key, payload, updated_at) VALUES (?, ?, ?)
ON CONFLICT (release_key) DO UPDATE SET payload = excluded.payload, updated_at = excluded.updated_at`,
		p.Key, string(p.Encode()), s.now().Unix())
	if err != nil {
		s.log.Debug("debrid: save payload", "err", err)
	}
}

// ---------------------------------------------------------------------------
// Acquire and refresh.

// Acquire makes a torrent release available on the first provider (by
// priority) that has it cached, and returns its payload. A payload stored
// for the same release within PayloadTTL is reused without API calls. The
// error wraps core.ErrNotCached when every provider answered "not cached",
// errNoVideo (core.ErrReleaseRejected) when the torrent has no playable
// file, and is a plain (transient) error otherwise.
//
// Acquire is a check: it never leaves a torrent downloading. The title's
// download queued by QueueDownload (cached-only off) is the exception it
// follows, by torrent id, until it plays: core.ErrDownloadQueued meanwhile.
func (s *Service) Acquire(ctx context.Context, rel core.Release) ([]byte, error) {
	entries := s.active()
	if len(entries) == 0 {
		return nil, ErrNoProvider
	}
	hash := strings.ToLower(rel.InfoHash)
	if hash == "" {
		return nil, fmt.Errorf("debrid: torrent %q has no info hash: %w", rel.Name, core.ErrReleaseRejected)
	}
	key := payloadKey(rel)
	if p, ok := s.reusablePayload(ctx, rel); ok {
		return p.Encode(), nil
	}
	magnet := rel.MagnetURL
	if rel.Private || magnet == "" {
		magnet = bareMagnet(hash)
	}
	base := Payload{Key: key, ReleaseID: rel.ID, InfoHash: hash, Magnet: magnet, Private: rel.Private, Target: rel.Target, Release: rel.Name}
	choose := func(files []File) (File, error) {
		s.recordTorrentFiles(ctx, rel, files)
		return ChooseFile(files, rel.Target)
	}
	if b, err, ok := s.followQueued(ctx, rel, entries, base, choose); ok {
		return b, err
	}
	var errs []error
	notCached := 0
	for _, e := range s.markedFirst(ctx, entries, rel) {
		if err := e.blocked(s.now()); err != nil {
			errs = append(errs, err)
			continue
		}
		if _, paced := e.p.(pacedProvider); !paced && s.knownUncachedForRelease(ctx, e, rel) {
			notCached++
			continue
		}
		started := s.now()
		play := &apiPlayCalls{account: e.api}
		playCtx := context.WithValue(ctx, apiPlayKey{}, play)
		p, err := s.acquireOn(playCtx, e, base, choose, false)
		if err == nil {
			if e.api != nil && !lowPriority(ctx) {
				e.api.mu.Lock()
				e.api.api.FirstPlays++
				e.api.api.FirstPlayCalls += play.calls.Load()
				e.api.mu.Unlock()
			}
			s.savePayload(ctx, p)
			s.log.Info("debrid release ready", "release", rel.Name, "provider", displayName(e.cfg), "file", p.Name())
			s.emitDebridCached(e, rel, hash, p.Size, s.now().Sub(started))
			return p.Encode(), nil
		}
		if ctx.Err() != nil {
			if errors.Is(err, core.ErrDebridThrottled) {
				return nil, err
			}
			return nil, ctx.Err()
		}
		switch {
		case errors.Is(err, core.ErrDownloadLimit):
			// The account adds nothing for now: say so, it is why this
			// release cannot be checked or played.
			notCached++
			s.emitQuotaExhausted(e.cfg, "active_downloads", 1, 1)
			errs = append(errs, fmt.Errorf("%s: %w", displayName(e.cfg), err))
		case errors.Is(err, ErrTooLarge), errors.Is(err, errProviderZipped):
			// Provider-level rejection (plan size limit or server-side zip):
			// try remaining providers, and keep the reason if none can serve it.
			notCached++
			errs = append(errs, fmt.Errorf("%s: %w", displayName(e.cfg), err))
		case errors.Is(err, ErrBlocked):
			notCached++
			errs = append(errs, err)
		case errors.Is(err, core.ErrNotCached):
			notCached++
		case errors.Is(err, core.ErrWrongContent), errors.Is(err, core.ErrReleaseRejected):
			s.emitDebridFailed(e, rel, hash, err)
			return nil, err // release-level outcome; another account cannot fix it
		default:
			errs = append(errs, fmt.Errorf("%s: %w", displayName(e.cfg), err))
		}
	}
	if len(errs) == 0 {
		if rel.Private {
			return nil, core.ErrPrivateUncached
		}
		return nil, fmt.Errorf("%q: %w", rel.Name, core.ErrNotCached)
	}
	for _, err := range errs {
		if errors.Is(err, core.ErrDebridThrottled) {
			return nil, err
		}
	}
	if notCached > 0 {
		for _, err := range errs {
			if errors.Is(err, core.ErrDownloadLimit) {
				finalErr := fmt.Errorf("%q: %w (%v)", rel.Name, core.ErrDownloadLimit, errors.Join(errs...))
				s.emitDebridFailed(nil, rel, hash, finalErr)
				return nil, finalErr
			}
		}
		if rel.Private {
			return nil, core.ErrPrivateUncached
		}
		return nil, fmt.Errorf("%q: %w (%w)", rel.Name, core.ErrNotCached, errors.Join(errs...))
	}
	joined := errors.Join(errs...)
	s.emitDebridFailed(nil, rel, hash, joined)
	return nil, joined
}

// ListReleaseFiles lists the files of a torrent cached on a debrid provider,
// in the provider's file-list order (the order ReleaseTarget FileIndex counts).
// Providers that support bulk cache checks are queried first so uncached
// torrents are skipped without adding; providers without bulk checks check
// cache via acquireOn (removing uncached torrents when RemoveUncached is on
// and never adding private torrents unless already known cached). Returns
// core.ErrNotCached when no provider holds the torrent cached.
func (s *Service) ListReleaseFiles(ctx context.Context, rel core.Release) ([]core.ReleaseFile, error) {
	if s == nil {
		return nil, ErrNoProvider
	}
	entries := s.active()
	if len(entries) == 0 {
		return nil, ErrNoProvider
	}
	hash := strings.ToLower(strings.TrimSpace(rel.InfoHash))
	if hash == "" {
		if mh := reMagnetHash.FindStringSubmatch(rel.MagnetURL); mh != nil {
			hash = strings.ToLower(mh[1])
		}
	}
	if hash == "" {
		return nil, fmt.Errorf("debrid: torrent %q has no info hash: %w", rel.Name, core.ErrReleaseRejected)
	}
	magnet := rel.MagnetURL
	if rel.Private || magnet == "" {
		magnet = bareMagnet(hash)
	}
	// Populate knownFor via bulk cache checks on providers that support them
	// (no-op for hashes already known and for providers without bulk checks).
	s.CheckCached(ctx, []string{hash})

	base := Payload{ReleaseID: rel.ID, InfoHash: hash, Magnet: magnet, Private: rel.Private, Release: rel.Name, Target: &core.ReleaseTarget{AllowNonVideo: true}}
	var errs []error
	for _, e := range s.markedFirst(ctx, entries, rel) {
		if e.blocked(s.now()) != nil {
			continue
		}
		if s.knownUncachedForRelease(ctx, e, rel) {
			continue
		}
		var captured []File
		choose := func(files []File) (File, error) {
			s.recordTorrentFiles(ctx, rel, files)
			captured = slices.Clone(files)
			for _, f := range files {
				if f.selected {
					return f, nil
				}
			}
			if f, err := ChooseFile(files, nil); err == nil {
				return f, nil
			}
			if f, ok := primaryNonVideoFile(files); ok {
				return f, nil
			}
			for _, f := range files {
				if f.Size > 0 {
					return f, nil
				}
			}
			if len(files) > 0 {
				return files[0], nil
			}
			return File{}, core.ErrWrongContent
		}
		_, err := s.acquireOn(ctx, e, base, choose, false)
		if (err == nil || errors.Is(err, errProviderZipped)) && len(captured) > 0 {
			out := make([]core.ReleaseFile, len(captured))
			for i, f := range captured {
				out[i] = core.ReleaseFile{Index: i, Path: f.Path, Size: f.Size}
			}
			return out, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err != nil && (!errors.Is(err, core.ErrNotCached) || errors.Is(err, ErrTooLarge) || errors.Is(err, errProviderZipped)) {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("%q: %w (%v)", rel.Name, core.ErrNotCached, errors.Join(errs...))
	}
	return nil, fmt.Errorf("%q: %w", rel.Name, core.ErrNotCached)
}

// primaryNonVideoFile picks a file from the dominant non-video pack media
// class (audio, book, or comic) by total byte size so listing a cached album,
// ebook, or comic torrent selects its media set rather than a leading sidecar
// like cover.jpg or album.cue.
func primaryNonVideoFile(files []File) (File, bool) {
	var totals [3]int64
	var best [3]File
	for _, f := range files {
		if f.Size <= 0 {
			continue
		}
		var i int
		switch rdMediaClass(f.Path) {
		case "audio":
			i = 0
		case "book":
			i = 1
		case "comic":
			i = 2
		default:
			continue
		}
		totals[i] += f.Size
		if best[i].Size < f.Size {
			best[i] = f
		}
	}
	winner := -1
	for i, tot := range totals {
		if tot > 0 && (winner < 0 || tot > totals[winner]) {
			winner = i
		}
	}
	if winner < 0 {
		return File{}, false
	}
	return best[winner], true
}

// CachedPackMatcher inspects a cached season-pack torrent once and returns a
// matcher reporting which episode targets exist in its file list, plus
// definitive=true when the configured providers gave a conclusive answer.
func (s *Service) CachedPackMatcher(ctx context.Context, rel core.Release) (func(*core.ReleaseTarget) bool, bool) {
	if s == nil {
		return nil, false
	}
	entries := s.active()
	if len(entries) == 0 {
		return nil, false
	}
	hash := strings.ToLower(rel.InfoHash)
	if hash == "" {
		return nil, true
	}
	magnet := rel.MagnetURL
	if rel.Private || magnet == "" {
		magnet = bareMagnet(hash)
	}
	base := Payload{ReleaseID: rel.ID, InfoHash: hash, Magnet: magnet, Private: rel.Private, Release: rel.Name}
	definitive := true
	for _, e := range s.markedFirst(ctx, entries, rel) {
		if e.blocked(s.now()) != nil {
			definitive = false
			continue
		}
		if e.kind() == config.DebridRealDebrid && e.api != nil {
			if files := e.api.selectedPack(ctx, hash); len(files) > 0 {
				e.api.noteRemembered()
				return func(target *core.ReleaseTarget) bool {
					if target == nil {
						return false
					}
					_, err := ChooseFile(files, target)
					return err == nil
				}, true
			}
		}
		if s.knownUncachedForRelease(ctx, e, rel) {
			continue
		}
		var captured []File
		choose := func(files []File) (File, error) {
			s.recordTorrentFiles(ctx, rel, files)
			captured = slices.Clone(files)
			return ChooseFile(files, nil)
		}
		_, err := s.acquireOn(ctx, e, base, choose, false)
		if err == nil && len(captured) > 0 {
			files := captured
			return func(target *core.ReleaseTarget) bool {
				if target == nil {
					return false
				}
				_, err := ChooseFile(files, target)
				return err == nil
			}, true
		}
		if ctx.Err() != nil {
			return nil, false
		}
		switch {
		case errors.Is(err, core.ErrNotCached) && !errors.Is(err, core.ErrDownloadLimit):
		case errors.Is(err, ErrBlocked), errors.Is(err, core.ErrReleaseRejected):
		default:
			definitive = false
		}
	}
	return nil, definitive
}

// queuedTTL is how long a title's queued download stands for the title: other
// releases of it are not sent as well.
const queuedTTL = 6 * time.Hour

// maxQueued bounds the queued-download memory (one entry per title).
const maxQueued = 10_000

type queuedDownload struct {
	hash, provider  string
	providerName    string // display name, for messages
	torrent         string // the provider's torrent id once added
	queuedID        string // provider queue id when queued behind active slots (TorBox)
	queuedHash      string // hash reported by the provider's queued reply, if any
	activeDownloads int    // active downloads ahead of this queued item, when known
	release         string // release name, for messages and logs
	at              time.Time
}

// queueKey identifies what a release is for (a movie, or one episode), so
// one download is queued per title rather than one per candidate release.
func queueKey(rel core.Release) string {
	ids := rel.TitleIDs
	se := ""
	if t := rel.Target; t != nil {
		if t.ExpectedIDs != (core.IDs{}) {
			ids = t.ExpectedIDs
		}
		se = fmt.Sprintf("|s%de%d", t.Season, t.Episode)
	}
	if ids != (core.IDs{}) {
		return fmt.Sprintf("%d|%s|%d%s", ids.TMDB, ids.IMDB, ids.TVDB, se)
	}
	if t := strings.ToLower(strings.TrimSpace(rel.Info.Title)); t != "" {
		return fmt.Sprintf("%s|%v|%v%s", t, rel.Info.Seasons, rel.Info.Episodes, se)
	}
	return "hash|" + strings.ToLower(rel.InfoHash) // nothing says what it is for
}

func queuedDownloadError(relName, provider string, activeDownloads int) error {
	if provider != "" && activeDownloads > 0 {
		return fmt.Errorf("%q on %s: %w", relName, provider, &core.DownloadQueuedError{
			Provider:        provider,
			ActiveDownloads: activeDownloads,
			Err:             core.ErrDownloadQueued,
		})
	}
	if provider != "" {
		return fmt.Errorf("%q on %s: %w", relName, provider, core.ErrDownloadQueued)
	}
	return fmt.Errorf("%q: %w", relName, core.ErrDownloadQueued)
}

func (s *Service) setQueuedLocked(key string, q queuedDownload) {
	if s.queued == nil || len(s.queued) >= maxQueued {
		s.queued = map[string]queuedDownload{}
		s.queuedByHash = map[string]string{}
	}
	if s.queuedByHash == nil {
		s.queuedByHash = map[string]string{}
	}
	if old, ok := s.queued[key]; ok && old.hash != "" && old.hash != q.hash && s.queuedByHash[old.hash] == key {
		delete(s.queuedByHash, old.hash)
	}
	s.queued[key] = q
	if q.hash != "" {
		s.queuedByHash[q.hash] = key
	}
}

func (s *Service) updateQueued(key string, q queuedDownload) {
	s.queuedMu.Lock()
	if cur, ok := s.queued[key]; ok && cur.hash == q.hash {
		s.queued[key] = q
		if q.hash != "" && s.queuedByHash != nil {
			s.queuedByHash[q.hash] = key
		}
	}
	s.queuedMu.Unlock()
}

// queuedFor returns the title's (or matching info hash's) live queued download.
func (s *Service) queuedFor(rel core.Release) (string, queuedDownload, bool) {
	s.queuedMu.Lock()
	defer s.queuedMu.Unlock()
	now := s.now()
	key := queueKey(rel)
	if q, ok := s.queued[key]; ok && now.Sub(q.at) < queuedTTL {
		return key, q, true
	}
	if hash := strings.ToLower(strings.TrimSpace(rel.InfoHash)); hash != "" {
		if hkey, ok := s.queuedByHash[hash]; ok {
			if q, ok := s.queued[hkey]; ok && q.hash == hash && now.Sub(q.at) < queuedTTL {
				return hkey, q, true
			}
		}
	}
	return "", queuedDownload{}, false
}

func (s *Service) forgetQueuedKey(key, hash string) {
	s.queuedMu.Lock()
	if q, ok := s.queued[key]; ok && q.hash == hash {
		delete(s.queued, key)
		if s.queuedByHash[hash] == key {
			delete(s.queuedByHash, hash)
		}
	}
	s.queuedMu.Unlock()
}

func (s *Service) forgetQueued(rel core.Release, hash string) {
	s.forgetQueuedKey(queueKey(rel), hash)
}

func (s *Service) queuedStats(now time.Time) (counts map[string]int, behind map[string]int) {
	s.queuedMu.Lock()
	defer s.queuedMu.Unlock()
	for _, q := range s.queued {
		if q.provider == "" || now.Sub(q.at) >= queuedTTL {
			continue
		}
		if counts == nil {
			counts = map[string]int{}
		}
		counts[q.provider]++
		if q.queuedID != "" && q.activeDownloads > 0 {
			if behind == nil {
				behind = map[string]int{}
			}
			if q.activeDownloads > behind[q.provider] {
				behind[q.provider] = q.activeDownloads
			}
		}
	}
	return counts, behind
}

func (s *Service) followQueuedOn(ctx context.Context, e *entry, q queuedDownload) (queuedTorrentStatus, error) {
	follower, ok := e.p.(queuedTorrentFollower)
	if !ok {
		return queuedTorrentStatus{}, nil
	}
	if err := e.acquire(ctx); err != nil {
		return queuedTorrentStatus{}, err
	}
	defer e.release()
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, providerOpTimeout)
	defer cancel()
	ctx = context.WithValue(ctx, diagnosticKey{}, providerDiagnostic{s.log, displayName(e.cfg), q.release, q.hash})
	st, err := follower.followQueuedTorrent(ctx, q.hash, q.queuedHash, q.queuedID)
	if err == nil || parent.Err() == nil {
		s.observe(e, err)
	}
	return st, err
}

// followQueued answers for the release that is its title's queued download:
// the payload once the provider has it, core.ErrDownloadQueued while it is
// still queued or downloading. The torrent is followed by id (or, when
// provider-queued without a torrent id yet, via the provider's torrent list
// and queued list), never added again, and whatever cached-only now says (it
// is already on the account). ok is false for any other release, or when the
// download is gone from the account.
func (s *Service) followQueued(ctx context.Context, rel core.Release, entries []*entry, base Payload, choose func([]File) (File, error)) ([]byte, error, bool) {
	key, q, ok := s.queuedFor(rel)
	if !ok || q.hash != base.InfoHash {
		return nil, nil, false
	}
	var e *entry
	for _, x := range entries {
		if x.cfg.ID == q.provider {
			e = x
		}
	}
	if e == nil || e.blocked(s.now()) != nil {
		return nil, nil, false
	}
	if q.torrent == "" && q.queuedID != "" {
		if _, canFollow := e.p.(queuedTorrentFollower); canFollow {
			st, err := s.followQueuedOn(ctx, e, q)
			switch {
			case err != nil:
				if ctx.Err() != nil {
					return nil, ctx.Err(), true
				}
				if errors.Is(err, core.ErrBudgetExhausted) {
					return nil, err, true
				}
				if errors.Is(err, ErrAuth) {
					s.forgetQueuedKey(key, q.hash)
					return nil, nil, false
				}
				// Transient lookup failure: keep remembering the queued item
				// instead of forgetting and re-adding it.
				return nil, queuedDownloadError(rel.Name, displayName(e.cfg), q.activeDownloads), true
			case st.TorrentID != "":
				q.torrent = st.TorrentID
				q.queuedID = ""
				q.activeDownloads = 0
				s.updateQueued(key, q)
				s.setKnown(ctx, e.cfg.ID, q.hash, st.TorrentID, false)
			case st.StillQueued:
				changed := false
				if st.ActiveDownloads > 0 && st.ActiveDownloads != q.activeDownloads {
					q.activeDownloads = st.ActiveDownloads
					changed = true
				}
				if st.QueuedID != "" && st.QueuedID != q.queuedID {
					q.queuedID = st.QueuedID
					changed = true
				}
				if changed {
					s.updateQueued(key, q)
				}
				return nil, queuedDownloadError(rel.Name, displayName(e.cfg), q.activeDownloads), true
			default:
				// Gone from both the torrent list and the queued list.
				s.forgetQueuedKey(key, q.hash)
				return nil, nil, false
			}
		}
	}
	if q.torrent != "" {
		base.TorrentID = q.torrent
	}
	started := s.now()
	p, err := s.acquireOn(ctx, e, base, choose, q.torrent != "")
	switch {
	case err == nil:
		s.savePayload(ctx, p)
		s.forgetQueuedKey(key, q.hash)
		s.log.Info("queued debrid download is ready", "release", rel.Name, "provider", displayName(e.cfg), "file", p.Name())
		wait := s.now().Sub(q.at)
		if wait <= 0 {
			wait = s.now().Sub(started)
		}
		s.emitDebridCached(e, rel, base.InfoHash, p.Size, wait)
		return p.Encode(), nil, true
	case errors.Is(err, core.ErrDebridThrottled):
		return nil, err, true
	case ctx.Err() != nil:
		return nil, ctx.Err(), true
	case errors.Is(err, core.ErrWrongContent), errors.Is(err, core.ErrReleaseRejected):
		return nil, err, true
	case errors.Is(err, core.ErrNotCached) && !refusal(err) && !errors.Is(err, core.ErrDownloadLimit):
		if tid := pendingTorrent(err); tid != "" && q.torrent == "" {
			q.torrent = tid
			q.queuedID = ""
			q.activeDownloads = 0
			s.updateQueued(key, q)
		}
		return nil, queuedDownloadError(rel.Name, displayName(e.cfg), q.activeDownloads), true
	}
	// Removed from the account, refused, or the provider failed: the
	// download no longer stands; the release is checked like any other.
	s.forgetQueuedKey(key, q.hash)
	return nil, nil, false
}

// QueueDownload sends an uncached torrent release to the owner's first
// debrid service (by priority) that will take it, when the owner turned
// cached-only off. The resolver calls it with the best release of a title
// whose releases are all uncached. One download is queued per title: a
// title with a download already queued answers core.ErrDownloadQueued
// without adding anything. It returns the payload when the provider turns
// out to hold the torrent after all, core.ErrDownloadQueued when the
// download started, and core.ErrDownloadRefused (or core.ErrDownloadLimit)
// when no provider would take it. Background checks never start downloads.
func (s *Service) QueueDownload(ctx context.Context, rel core.Release) ([]byte, error) {
	if rel.Private {
		return nil, core.ErrPrivateUncached
	}
	if s.Config().CachedOnly || core.IsBackgroundCheck(ctx) {
		return nil, fmt.Errorf("%q: %w", rel.Name, core.ErrNotCached)
	}
	hash := strings.ToLower(rel.InfoHash)
	if hash == "" || rel.Protocol != core.ProtocolTorrent {
		return nil, fmt.Errorf("debrid: %q is not a torrent with an info hash: %w", rel.Name, core.ErrReleaseRejected)
	}
	entries := s.active()
	if len(entries) == 0 {
		return nil, ErrNoProvider
	}
	key := queueKey(rel)
	now := s.now()
	s.queuedMu.Lock()
	if q, ok := s.queued[key]; ok && now.Sub(q.at) < queuedTTL {
		s.queuedMu.Unlock()
		if q.hash == hash && q.provider != "" {
			return s.Acquire(ctx, rel) // follow it
		}
		return nil, queuedDownloadError(q.release, q.providerName, q.activeDownloads)
	}
	if hkey, ok := s.queuedByHash[hash]; ok {
		if q, ok := s.queued[hkey]; ok && q.hash == hash && q.provider != "" && now.Sub(q.at) < queuedTTL {
			s.queuedMu.Unlock()
			return s.Acquire(ctx, rel) // follow the already-queued hash
		}
	}
	// Claimed before the provider call, so concurrent plays of the title
	// do not add it twice.
	s.setQueuedLocked(key, queuedDownload{hash: hash, release: rel.Name, at: now})
	s.queuedMu.Unlock()

	magnet := rel.MagnetURL
	if magnet == "" {
		magnet = "magnet:?xt=urn:btih:" + hash
	}
	base := Payload{Key: payloadKey(rel), ReleaseID: rel.ID, InfoHash: hash, Magnet: magnet, Target: rel.Target, Release: rel.Name}
	choose := func(files []File) (File, error) {
		s.recordTorrentFiles(ctx, rel, files)
		return ChooseFile(files, rel.Target)
	}
	var errs []error
	for _, e := range entries {
		if err := e.blocked(s.now()); err != nil {
			errs = append(errs, err)
			continue
		}
		if s.refusedFor(e.cfg.ID, hash) || e.refusesHash(hash) {
			errs = append(errs, fmt.Errorf("%s refused this torrent: %w", displayName(e.cfg), ErrAddRefused))
			continue
		}
		started := s.now()
		p, err := s.acquireOn(ctx, e, base, choose, true)
		if err == nil {
			s.forgetQueued(rel, hash)
			s.savePayload(ctx, p)
			s.log.Info("debrid release ready", "release", rel.Name, "provider", displayName(e.cfg), "file", p.Name())
			s.emitDebridCached(e, rel, hash, p.Size, s.now().Sub(started))
			return p.Encode(), nil
		}
		if ctx.Err() != nil {
			s.forgetQueued(rel, hash)
			if errors.Is(err, core.ErrDebridThrottled) {
				return nil, err
			}
			return nil, ctx.Err()
		}
		if errors.Is(err, core.ErrDownloadLimit) {
			s.emitQuotaExhausted(e.cfg, "active_downloads", 1, 1)
		}
		var untracked *untrackedPending
		qp, isQueued := pendingQueue(err)
		tid := pendingTorrent(err)
		confirmed := tid != "" || isQueued || errors.As(err, &untracked)
		kind := e.kind()
		if (confirmed || (kind != "realdebrid" && kind != "premiumize")) && errors.Is(err, core.ErrNotCached) && !refusal(err) && !errors.Is(err, core.ErrDownloadLimit) {
			qd := queuedDownload{
				hash:         hash,
				provider:     e.cfg.ID,
				providerName: displayName(e.cfg),
				torrent:      tid,
				release:      rel.Name,
				at:           now,
			}
			if isQueued {
				qd.queuedID = qp.queuedID
				qd.queuedHash = qp.hash
				qd.activeDownloads = qp.activeDownloads
			}
			s.queuedMu.Lock()
			s.setQueuedLocked(key, qd)
			s.queuedMu.Unlock()
			s.log.Info("sent uncached torrent to download", "release", rel.Name, "provider", displayName(e.cfg), "queued_id", qd.queuedID, "active_downloads", qd.activeDownloads)
			return nil, queuedDownloadError(rel.Name, displayName(e.cfg), qd.activeDownloads)
		}
		if errors.Is(err, core.ErrReleaseRejected) {
			s.forgetQueued(rel, hash)
			s.emitDebridFailed(e, rel, hash, err)
			return nil, err
		}
		errs = append(errs, fmt.Errorf("%s: %w", displayName(e.cfg), err))
	}
	s.forgetQueued(rel, hash)
	joined := errors.Join(errs...)
	s.log.Info("uncached torrent not sent to download", "release", rel.Name, "err", joined)
	s.emitDebridFailed(nil, rel, hash, joined)
	for _, err := range errs {
		if errors.Is(err, core.ErrDebridThrottled) {
			return nil, err
		}
		if errors.Is(err, core.ErrDownloadLimit) {
			return nil, fmt.Errorf("%q: %w (%v)", rel.Name, core.ErrDownloadLimit, joined)
		}
	}
	return nil, fmt.Errorf("%q: %w (%w)", rel.Name, core.ErrDownloadRefused, joined)
}

// errBackgroundUndecided is a background check that could not look at a
// torrent without adding it: availability stays unknown, and a foreground
// attempt may still add and play it.
var errBackgroundUndecided = fmt.Errorf("%w: %w", ErrUnsupported, core.ErrForegroundOnly)

// acquireOn resolves a torrent on one provider: reuse an existing torrent
// (base.TorrentID, else one remembered by hash), else add it; then generate
// a direct link. Unless download is set, a torrent that is not ready is
// removed again (RemoveUncached): only QueueDownload leaves one downloading.
func (s *Service) acquireOn(ctx context.Context, e *entry, base Payload, choose func([]File) (File, error), download bool) (Payload, error) {
	wallStarted := time.Now()
	var queueDuration, findDuration, readyDuration, linkDuration time.Duration
	defer func() {
		s.log.Info("debrid acquisition timings", "provider", e.cfg.ID,
			"queue", queueDuration, "find", findDuration, "ready", readyDuration,
			"link", linkDuration, "took", time.Since(wallStarted))
	}()
	if err := e.acquire(ctx); err != nil {
		return Payload{}, err
	}
	queueDuration = time.Since(wallStarted)
	defer e.release()
	// A caller's own budget (a resolver candidate check, say) ending says
	// nothing about the provider; only the provider's outcomes, including
	// its own providerOpTimeout, move the breaker.
	parent := ctx
	observe := func(err error) {
		if err == nil || parent.Err() == nil {
			s.observe(e, err)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, providerOpTimeout)
	defer cancel()
	ctx = context.WithValue(ctx, diagnosticKey{}, providerDiagnostic{s.log, displayName(e.cfg), base.Release, base.InfoHash})
	cfg := s.Config()
	hash := base.InfoHash
	_, trialAdd := e.p.(pacedProvider)
	var remembered learnedAnswer
	if e.api != nil {
		remembered = e.api.answer(ctx, answerKey{hash: hash}, e.api.now())
		if err := remembered.refusal(); err != nil && (remembered.verdict != answerCold || !download) {
			e.api.noteRemembered()
			return Payload{}, err
		}
	}

	if base.Private {
		download = false
	}
	started := s.now()
	findStarted := time.Now()
	torrentID := base.TorrentID
	knownCached := false
	if trialAdd || remembered.verdict != "" {
		knownCached = remembered.verdict == answerCached
		if torrentID == "" {
			torrentID = remembered.torrentID
			if torrentID != "" {
				e.api.noteRemembered()
			}
		}
	} else if kc, ok, uncertain, kid := s.knownForDetail(ctx, e.cfg.ID, hash); ok {
		knownCached = kc
		if torrentID == "" {
			torrentID = kid
		}
		if e.api != nil && !download && !kc && !uncertain && torrentID == "" {
			return Payload{}, core.ErrNotCached
		}
	}
	if torrentID == "" && !remembered.needsReconciliation() {
		if fast, ok := e.p.(knownTorrentFinder); ok {
			// Atomic cached-only providers need no account listing before
			// foreground creation. Background discovery reads only local
			// hints or a fresh snapshot, warming a cold list alongside A.
			torrentID = fast.findKnown(hash)
			if torrentID == "" && core.IsBackgroundCheck(ctx) {
				if finder, ok := e.p.(backgroundTorrentFinder); ok {
					torrentID = finder.findBackground(ctx, hash)
				}
			}
		} else if id, err := e.p.Find(ctx, hash); err == nil {
			torrentID = id
		} else if !errors.Is(err, ErrUnsupported) {
			observe(err)
			// Real-Debrid has no atomic cached-only API. If the account
			// list did not answer, an add could duplicate a pending item.
			if e.kind() == config.DebridRealDebrid || errors.Is(err, ErrAuth) || errors.Is(err, core.ErrDebridThrottled) {
				return Payload{}, err
			}
		}
		if torrentID != "" && e.api != nil {
			e.api.mu.Lock()
			e.api.api.ListHits++
			e.api.mu.Unlock()
		}
	}
	if trialAdd && torrentID == "" && !mayTrialAdd(ctx) {
		return Payload{}, errBackgroundUndecided
	}
	// Background validation never creates torrents or starts downloads. An
	// existing account item can be inspected; unknown remains unknown. The
	// one exception is a provider that serves cached content without
	// adding anything (Premiumize): a positive bulk CheckCached — a
	// read-only GET — stands in for the account lookup it does not have,
	// and its cached-only Acquire can then only read, never add.
	cacheOnlyBackground := false
	if core.IsBackgroundCheck(ctx) && torrentID == "" {
		fast, ok := e.p.(backgroundCacheOnlyProvider)
		if !ok || !fast.supportsBackgroundCacheOnly() {
			return Payload{}, errBackgroundUndecided
		}
		cached, err := e.p.CheckCached(ctx, []string{hash})
		if err != nil {
			if errors.Is(err, ErrUnsupported) {
				return Payload{}, errBackgroundUndecided
			}
			observe(err)
			if errors.Is(err, ErrAuth) {
				return Payload{}, err
			}
			return Payload{}, errBackgroundUndecided // availability stays unknown
		}
		if !cached[strings.ToLower(hash)] {
			return Payload{}, ErrUnsupported
		}
		cacheOnlyBackground = true
	}
	req := AcquireRequest{InfoHash: hash, Magnet: base.Magnet, TorrentID: torrentID, Choose: choose, CachedOnly: !download || core.IsBackgroundCheck(ctx),
		RemoveUncached: cfg.RemoveUncached, Private: base.Private, AllowCompressedZip: base.Target != nil && base.Target.AllowNonVideo, MayRemove: s.mayRemove(e.cfg.ID)}
	if base.Private {
		var perr error
		req, perr = prepareAcquire(ctx, e.p, req, knownCached)
		if perr != nil {
			findDuration = time.Since(findStarted)
			observe(perr)
			if errors.Is(perr, core.ErrNotCached) && !errors.Is(perr, core.ErrDownloadLimit) {
				e.count(func(st *ProviderStats) { st.NotCached++ })
				s.setKnown(ctx, e.cfg.ID, hash, "", false)
			}
			return Payload{}, perr
		}
		torrentID = req.TorrentID
	}
	findDuration = time.Since(findStarted)
	readyStarted := time.Now()
	if torrentID == "" && !cacheOnlyBackground {
		if err := e.addsPaused(s.now()); err != nil {
			return Payload{}, err
		}
		// Adding to the account: in a cluster one node adds, the others
		// wait for its torrent id (no-op standalone).
		id, release, err := s.claimAdd(ctx, e, hash)
		if err != nil {
			return Payload{}, err
		}
		defer release()
		torrentID = id
		req.TorrentID = id
	}
	acquire := func() (*Acquired, error) {
		if req.Private && !req.privateChecked {
			var perr error
			req, perr = prepareAcquire(ctx, e.p, req, false)
			if perr != nil {
				return nil, perr
			}
		}
		if req.TorrentID == "" && !cacheOnlyBackground {
			if err := e.addsPaused(s.now()); err != nil {
				return nil, err
			}
		}
		var acq *Acquired
		var err error
		if e.api != nil && !trialAdd {
			acq, err = e.api.acquireRemembered(ctx, req, e.api.now, e.p.Acquire)
		} else {
			acq, err = e.p.Acquire(ctx, req)
		}
		err = wrapPrivateNotCached(req.Private, err)
		switch {
		case errors.Is(err, ErrAddRefused):
			if duration := e.addRefused(s.now()); duration > 0 {
				s.log.Warn("debrid provider adds paused", "provider", cleanDiagnostic(displayName(e.cfg), e.cfg.APIKey), "reason", "repeated add refusals", "duration", duration, "release", cleanDiagnostic(base.Release, e.cfg.APIKey), "infohash", cleanDiagnostic(base.InfoHash, e.cfg.APIKey))
				s.notifyChange()
			}
		case err == nil && acq.Added:
			e.addAccepted()
		}
		return acq, err
	}
	acq, err := acquire()
	_, isQueuedPending := pendingQueue(err)
	if err != nil && !core.IsBackgroundCheck(ctx) && torrentID != "" && pendingTorrent(err) == "" && !isQueuedPending &&
		(errors.Is(err, ErrGone) || e.api == nil && errors.Is(err, core.ErrNotCached)) && !refusal(err) {
		// The remembered torrent is gone or lacks the wanted file: add anew.
		// One still downloading (pending) stays as it is.
		req.TorrentID = ""
		req.privateChecked = false
		id, release, cerr := s.reclaimAdd(ctx, e, hash, torrentID)
		if cerr == nil {
			defer release()
			req.TorrentID = id
			acq, err = acquire()
		} else {
			err = cerr
		}
	}
	readyDuration = time.Since(readyStarted)
	if errors.Is(err, errAddsPaused) {
		// Skipped without a provider call: says nothing about the torrent.
		return Payload{}, err
	}
	observe(err)
	if errors.Is(err, ErrAddRefused) {
		// Not evidence about the torrent (it may be the account's add
		// throttle): skip it briefly, in memory only, and do not queue it
		// on this provider as a download meanwhile.
		e.count(func(st *ProviderStats) { st.Blocked++ })
		s.markRefused(e.cfg.ID, hash, addRefusedTTL)
		return Payload{}, err
	}
	if err != nil {
		if !errors.Is(err, ErrBlocked) && errors.Is(err, core.ErrNotCached) && !errors.Is(err, core.ErrDownloadLimit) && !errors.Is(err, errProviderZipped) && !errors.Is(err, errSelectionMismatch) {
			// A download left on the account keeps its id, so a later
			// check follows it instead of adding the torrent again. Do not
			// remember errProviderZipped as uncached for 30 minutes: the
			// torrent is cached as a zip and another target may be streamable.
			e.count(func(st *ProviderStats) { st.NotCached++ })
			s.setKnown(ctx, e.cfg.ID, hash, pendingTorrent(err), false)
		}
		if errors.Is(err, ErrBlocked) && !errors.Is(err, ErrBlockedFile) {
			s.rememberBlocked(e, hash)
		}
		return Payload{}, err
	}
	resolved := s.now().Sub(started)
	if !acq.Added && acq.TorrentID != "" {
		e.count(func(st *ProviderStats) { st.Reused++ })
		if e.api != nil {
			e.api.mu.Lock()
			e.api.api.ReuseHits++
			e.api.mu.Unlock()
		}
	}
	p := base
	p.Provider, p.Kind = e.cfg.ID, e.cfg.Kind
	p.TorrentID, p.FileID, p.Path, p.Size, p.Link, p.Zip = acq.TorrentID, acq.File.ID, acq.File.Path, acq.File.Size, acq.File.Link, acq.File.Zip
	linkStart := s.now()
	linkWallStarted := time.Now()
	dl, err := e.link(ctx, p.handle())
	linkDuration = time.Since(linkWallStarted)
	observe(err)
	if err != nil {
		if errors.Is(err, ErrBlockedFile) {
			e.count(func(st *ProviderStats) { st.Blocked++ })
		} else if errors.Is(err, ErrBlocked) {
			s.blockedFile(ctx, e, hash, acq.TorrentID, acq.Added && cfg.RemoveUncached, req.MayRemove)
		}
		return Payload{}, err
	}
	linked := s.now().Sub(linkStart)
	s.noteLatency(e.cfg.ID, func(l *Latency) {
		first := l.Samples == 0
		l.ResolveMs = ewma(l.ResolveMs, ms(resolved), first)
		l.LinkMs = ewma(l.LinkMs, ms(linked), first)
		l.Samples++
	})
	p.URL, p.Expires = dl.URL, dl.Expires
	if dl.Size > 0 && p.Size <= 0 {
		p.Size = dl.Size
	}
	if p.Size <= 0 {
		return Payload{}, fmt.Errorf("debrid: %s reported no size for %s: %w", displayName(e.cfg), p.Name(), errNoVideo)
	}
	e.count(func(st *ProviderStats) { st.Cached++ })
	s.setKnown(ctx, e.cfg.ID, hash, acq.TorrentID, true)
	return p, nil
}

func (s *Service) setMediaPauseChecker(fn func(provider, rawURL string) error) {
	s.mediaPausedMu.Lock()
	s.mediaPaused = fn
	s.mediaPausedMu.Unlock()
}

func (s *Service) checkMediaPaused(provider, rawURL string) error {
	s.mediaPausedMu.RLock()
	fn := s.mediaPaused
	s.mediaPausedMu.RUnlock()
	if fn == nil {
		return nil
	}
	return fn(provider, rawURL)
}

// fresh reports whether a payload's direct link is usable now.
func (s *Service) fresh(p Payload) bool {
	return p.URL != "" && (p.Expires.IsZero() || s.now().Add(linkMargin).Before(p.Expires))
}

// Refresh returns p with a usable direct link. Unless force is set, a
// fresher link stored by another process path is reused without an API
// call. The same provider regenerates the link (re-adding the torrent by
// magnet when it was removed); otherwise another provider holding the same
// file takes over. ErrOriginalLost means every eligible account confirmed
// the loss. A missing torrent member also carries ErrArticleMissing.
func (s *Service) Refresh(ctx context.Context, p Payload, force bool) (Payload, error) {
	if p.IsDirect() {
		// An addon's link resolves itself on every request: a failed
		// redirect target is replaced by asking the link again.
		if force {
			p.URL = p.Link
		}
		return p, nil
	}
	if !force {
		if stored, ok := s.storedPayload(ctx, p.Key); ok && s.fresh(stored) && stored.Path == p.Path && stored.sameStreamEncoding(p) {
			return stored, nil
		}
		if s.fresh(p) {
			return p, nil
		}
	}
	var errs []error
	gone, missing := 0, 0
	note := func(err error) {
		switch {
		case errors.Is(err, errFileMissing):
			missing++
		case !refusal(err) && !errors.Is(err, core.ErrDownloadLimit) &&
			(errors.Is(err, ErrGone) || errors.Is(err, core.ErrNotCached)):
			gone++
		}
		errs = append(errs, err)
	}
	if e := s.entryByID(p.Provider); e != nil {
		if err := e.blocked(s.now()); err != nil {
			errs = append(errs, err)
		} else if pauseErr := s.checkMediaPaused(p.Provider, p.URL); pauseErr != nil {
			errs = append(errs, pauseErr)
		} else {
			out, err := s.relink(ctx, e, p)
			if err == nil {
				return out, nil
			}
			if ctx.Err() != nil {
				if errors.Is(err, core.ErrDebridThrottled) {
					return p, err
				}
				return p, ctx.Err()
			}
			note(err)
		}
	}
	// Another provider may hold the same torrent.
	for _, e := range s.active() {
		if e.cfg.ID == p.Provider {
			continue
		}
		if err := e.blocked(s.now()); err != nil {
			errs = append(errs, err)
			continue
		}
		base := Payload{Key: p.Key, ReleaseID: p.ReleaseID, InfoHash: p.InfoHash, Magnet: p.Magnet, Private: p.Private, Target: p.Target, Release: p.Release}
		out, err := s.acquireOn(ctx, e, base, sameFile(p), false)
		if err == nil {
			e.count(func(st *ProviderStats) { st.LinksRefreshed++ })
			s.savePayload(ctx, out)
			s.log.Info("debrid release moved to another provider", "release", p.Release, "provider", displayName(e.cfg))
			return out, nil
		}
		if ctx.Err() != nil {
			if errors.Is(err, core.ErrDebridThrottled) {
				return p, err
			}
			return p, ctx.Err()
		}
		note(err)
	}
	joined := errors.Join(errs...)
	if errors.Is(joined, core.ErrDebridThrottled) {
		return p, joined
	}
	if joined == nil {
		joined = ErrNoProvider
	}
	switch {
	case missing > 0 && missing+gone == len(errs):
		return p, fmt.Errorf("debrid: %s is gone: %w (%v)", p.Name(), errors.Join(core.ErrArticleMissing, core.ErrOriginalLost), joined)
	case gone > 0 && gone+missing == len(errs):
		return p, fmt.Errorf("debrid: %s: %w: %w", p.Name(), core.ErrOriginalLost, core.ErrNotCached)
	}
	return p, fmt.Errorf("debrid: refresh link for %s: %w (%v)", p.Name(), core.ErrSourceUnavailable, joined)
}

// alternateProvider obtains the same torrent file on another provider after
// a media host refuses connections. It never treats a different signed URL
// on the same host as a change of route.
func (s *Service) alternateProvider(ctx context.Context, p Payload) (Payload, error) {
	for _, e := range s.active() {
		if e.cfg.ID == p.Provider || e.blocked(s.now()) != nil {
			continue
		}
		base := Payload{Key: p.Key, ReleaseID: p.ReleaseID, InfoHash: p.InfoHash, Magnet: p.Magnet, Private: p.Private, Target: p.Target, Release: p.Release}
		out, err := s.acquireOn(ctx, e, base, sameFile(p), false)
		if err == nil && dialAddr(out.URL) != "" && dialAddr(out.URL) != dialAddr(p.URL) {
			s.savePayload(ctx, out)
			return out, nil
		}
		if ctx.Err() != nil {
			return p, ctx.Err()
		}
	}
	return p, core.ErrSourceUnavailable
}

func (e *entry) link(ctx context.Context, h Handle) (DirectLink, error) {
	if _, paced := e.p.(pacedProvider); e.api != nil && !paced {
		return e.api.linkRemembered(ctx, h, e.api.now, e.p.Link)
	}
	return e.p.Link(ctx, h)
}

// relink regenerates a link on the payload's own provider.
func (s *Service) relink(ctx context.Context, e *entry, p Payload) (Payload, error) {
	if err := e.acquire(ctx); err != nil {
		return p, err
	}
	lctx, cancel := context.WithTimeout(ctx, providerOpTimeout)
	lctx = context.WithValue(lctx, diagnosticKey{}, providerDiagnostic{s.log, displayName(e.cfg), p.Release, p.InfoHash})
	start := s.now()
	dl, err := e.link(lctx, p.handle())
	if err == nil || ctx.Err() == nil { // the caller's budget is not a provider failure
		s.observe(e, err)
	}
	cancel()
	e.release()
	if err == nil {
		s.noteLatency(e.cfg.ID, func(l *Latency) { l.LinkMs = ewma(l.LinkMs, ms(s.now().Sub(start)), l.Samples == 0) })
		// The provider re-signed the link: it stands on the host the
		// provider names, so any replay provenance the payload carried ends
		// right here — and ends before the save, because a stored link is
		// reused verbatim and must not come back wearing the old marker.
		p.URL, p.Expires, p.OnReplayEdge = dl.URL, dl.Expires, false
		e.count(func(st *ProviderStats) { st.LinksRefreshed++ })
		s.savePayload(ctx, p)
		return p, nil
	}
	if errors.Is(err, ErrBlockedFile) {
		e.count(func(st *ProviderStats) { st.Blocked++ })
		return p, err
	}
	if errors.Is(err, ErrBlocked) {
		s.blockedFile(ctx, e, p.InfoHash, p.TorrentID, false, nil)
		return p, err
	}
	if !errors.Is(err, ErrGone) {
		return p, err
	}
	// The torrent was removed from the account: re-add it by magnet.
	base := Payload{Key: p.Key, ReleaseID: p.ReleaseID, InfoHash: p.InfoHash, Magnet: p.Magnet, Private: p.Private, Target: p.Target, Release: p.Release}
	s.forgetTorrent(ctx, e.cfg.ID, p.InfoHash)
	out, err := s.acquireOn(ctx, e, base, sameFile(p), false)
	if err != nil {
		return p, err
	}
	e.count(func(st *ProviderStats) { st.LinksRefreshed++ })
	s.savePayload(ctx, out)
	return out, nil
}

// blockedFile records a file the provider refuses to serve as not cached
// there, so ranking stops promoting it and the next Play skips it without
// API calls. A torrent Ether added for this attempt is removed again when
// remove is set and no node streams from it.
func (s *Service) blockedFile(ctx context.Context, e *entry, hash, torrentID string, remove bool, mayRemove func(context.Context, string) bool) {
	s.rememberBlocked(e, hash)
	if remove && torrentID != "" {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		if mayRemove == nil || mayRemove(rctx, torrentID) {
			if err := e.p.Remove(rctx, torrentID); err == nil {
				s.forgetTorrent(rctx, e.cfg.ID, hash)
			}
		}
		cancel()
	}
	s.log.Info("debrid provider blocks file", "provider", displayName(e.cfg), "hash", hash)
}

// rememberedAnswers returns e's own remembered answers for hashes, the
// per-account answers acquisition consults (debrid_answers), which outlive
// the discovery memory and a restart: one read for the whole list.
func (e *entry) rememberedAnswers(ctx context.Context, hashes []string) map[string]learnedAnswer {
	if e.api == nil || len(hashes) == 0 {
		return nil
	}
	return e.api.hashAnswers(ctx, hashes, e.api.now())
}

func (e *entry) refusesHash(hash string) bool {
	pace := e.api
	if pace == nil {
		return false
	}
	pace.mu.Lock()
	defer pace.mu.Unlock()
	if pace.learned == nil {
		return false
	}
	a, _ := pace.learned.Peek(answerKey{hash: strings.ToLower(hash)})
	return a.verdict == answerBlockedHash && pace.now().Before(a.expires)
}

func (s *Service) rememberBlocked(e *entry, hash string) {
	e.count(func(st *ProviderStats) { st.Blocked++ })
	if e.api == nil {
		s.markRefused(e.cfg.ID, hash, blockedHashTTL)
	}
}

// refusal reports an outcome that left nothing downloading on the
// provider: it refused the torrent or its file, or its adds are paused.
func refusal(err error) bool {
	return errors.Is(err, ErrAddRefused) || errors.Is(err, ErrBlocked) || errors.Is(err, ErrTooLarge) || errors.Is(err, errProviderZipped) || errors.Is(err, errAddsPaused)
}

// markRefused remembers, in memory, that the provider refused this hash.
func (s *Service) markRefused(providerID, hash string, ttl time.Duration) {
	now := s.now()
	s.knownMu.Lock()
	s.sweepKnownLocked(now)
	until := time.Time{} // zero: permanent for this service lifetime
	if ttl > 0 {
		until = now.Add(ttl)
	}
	s.known[knownKey(providerID, strings.ToLower(hash))] = knownState{refused: true, until: until}
	s.knownMu.Unlock()
}

// refusedFor reports a remembered refusal of the hash by the provider.
func (s *Service) refusedFor(providerID, hash string) bool {
	s.knownMu.Lock()
	k, ok := s.known[knownKey(providerID, strings.ToLower(hash))]
	s.knownMu.Unlock()
	return ok && k.refused && (k.until.IsZero() || s.now().Before(k.until))
}

func (s *Service) forgetTorrent(ctx context.Context, providerID, hash string) {
	if s.db != nil {
		if _, err := s.db.ExecContext(ctx, `UPDATE debrid_torrents SET torrent_id = '' WHERE provider_id = ? AND info_hash = ?`, providerID, strings.ToLower(hash)); err != nil {
			s.log.Debug("debrid: forget torrent", "err", err)
		}
	}
}

// Test checks one provider config for the admin "test" button: the key
// works and the account can use the API.
func (s *Service) Test(ctx context.Context, cfg config.DebridProvider) error {
	p, err := s.opts.NewProvider(cfg, s.opts.ProviderOptions)
	if err != nil {
		return err
	}
	a, err := p.Account(ctx)
	if err != nil {
		if errors.Is(err, ErrAuth) {
			return fmt.Errorf("%s rejected the API key", displayName(cfg))
		}
		return err
	}
	s.emitAccountQuotaEvents(cfg, a)
	if !a.Premium {
		return fmt.Errorf("%s account %q has no active premium subscription", displayName(cfg), a.User)
	}
	return nil
}

// SetVault is called during service construction.
func (s *Service) SetVault(v vault.Store) { s.vault = v }
func (s *Service) recordTorrentFiles(ctx context.Context, r core.Release, files []File) {
	if s.vault == nil || len(files) == 0 {
		return
	}
	list := make([]vault.TorrentFile, len(files))
	for i, f := range files {
		list[i] = vault.TorrentFile{Path: f.Path, Size: f.Size}
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_, _ = s.vault.Put(ctx, vault.TorrentRecord(r, list, nil))
	}()
}
