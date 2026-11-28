package newznab

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"golang.org/x/text/unicode/norm"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/egress"
	"github.com/braedonsaunders/ether/internal/events"
	"github.com/braedonsaunders/ether/internal/logredact"
	"github.com/braedonsaunders/ether/internal/nzb"
	"github.com/braedonsaunders/ether/internal/releaseparse"
	"github.com/braedonsaunders/ether/internal/safefetch"
	"github.com/braedonsaunders/ether/internal/vault"
)

const (
	defaultMaxNZBBytes = 50 << 20
	defaultSearchLimit = 100
	defaultHTTPTimeout = 30 * time.Second
	defaultCacheTTL    = 6 * time.Hour
	// capsRetry spaces caps requests after a failure. Every caps request
	// spends a daily API hit, and searches work (ungated) without caps.
	capsRetry = time.Hour
	// nzbFlightTimeout caps one shared NZB fetch. The fetch runs apart
	// from any one caller's cancellation so a cancelled caller cannot fail
	// the others, and is cancelled when the last caller leaves; this bound
	// keeps an indexer that never answers from pinning it.
	nzbFlightTimeout = 90 * time.Second
)

var errCapsUnavailable = errors.New("newznab: capabilities unavailable; retrying later")

// redactURLError drops the request URL from a *url.Error: indexer URLs
// carry the API key, and callers log and surface these errors.
func redactURLError(err error) error {
	return logredact.TransportError(err, "newznab", nil)
}

// Options tunes a Client or Manager. Zero values get sensible defaults.
type Options struct {
	HTTPClient       *http.Client
	Logger           *slog.Logger
	SearchCacheTTL   time.Duration // <0 disables cache, 0 = default 6h
	Vault            vault.Store
	PluginReferences vault.Sealer
	LegacyNZBDir     func() string // non-empty only while migrating
	PublicIndexer    func(context.Context, config.Indexer) bool
	MaxNZBBytes      int64
	Now              func() time.Time // injected clock (tests)
	MaxParallel      int              // Manager fan-out; 0 dispatches every configured indexer
	MaxPerHost       int              // per-origin concurrency cap; 0 = DefaultMaxPerHost, <0 = unlimited
	HostLimiter      *HostLimiter     // optional shared per-origin concurrency limiter
	// ClientIdentity is the owner's policy for the client Ether presents to
	// indexers; the zero value keeps the Ether identity.
	ClientIdentity config.ClientIdentity
	// Backend builds the search backend for an indexer whose Kind is not
	// Newznab (built-in sites, internal/cardigann). Nil disables them.
	Backend func(config.Indexer) (Backend, error)
	// testDB shares the configured account's limits with one-off tests.
	testDB  *sql.DB
	onEvent func(Event)
	// Events receives non-blocking indexer.* domain events when set.
	Events *events.Bus
}

// Backend searches a site that is not a Newznab API, such as a built-in
// site run from an owner-installed definition. The Client keeps budgets,
// the result cache and events around it: a backend calls reserve with
// "hit" or "grab" before each request that counts against a daily budget.
type Backend interface {
	Search(ctx context.Context, req core.SearchRequest, reserve func(context.Context, string) error) ([]core.Release, error)
	FetchNZB(ctx context.Context, r core.Release, reserve func(context.Context, string) error) ([]byte, error)
	ResolveTorrent(ctx context.Context, r core.Release, reserve func(context.Context, string) error) (core.Release, error)
}

// CategoryBackend is a Backend that answers some category searches: an
// installed source plugin that declared them (#792). Every other add-on
// and Stremio source refuses a category search, the honest answer for a
// source that is asked by title id only (#478).
type CategoryBackend interface {
	AnswersCategories(ctx context.Context, cats []int) bool
}

// RecentBackend is a Backend that can list a site's latest releases, the
// release feed of a site with no Newznab API.
type RecentBackend interface {
	Recent(ctx context.Context, reserve func(context.Context, string) error) ([]core.Release, error)
}

// Client is one configured Newznab indexer. It implements core.Indexer.
type Client struct {
	cfg  config.Indexer
	db   *sql.DB
	http *http.Client
	// guarded fetches through safefetch: destinations are resolved,
	// validated and rewritten to the validated IP once per request,
	// direto ou via proxy do dono (see trustedCtx for the exception).
	guarded       *http.Client
	log           *slog.Logger
	cacheTTL      time.Duration
	vault         vault.Store
	legacyDir     func() string
	publicIndexer func(context.Context, config.Indexer) bool
	maxNZB        int64
	now           func() time.Time
	backends      func(config.Indexer) (Backend, error)
	hosts         *HostLimiter

	searchModeMu sync.Mutex
	searchMode   searchCatState
	budgetMu     sync.Mutex
	mem          memBudget
	rate         rateLimitState
	apiLimit     apiLimitState

	queryFlights searchFlights
	cacheMu      sync.RWMutex
	memCache     map[string]memCacheEntry
	torrents     torrentDescriptorCache
	br           breaker
	backgroundBR breaker
	// newznabPath is set once /api answered 404 and {base}/api/newznab
	// worked: some indexers serve Newznab there, which Radarr and
	// Sonarr reach through their API Path field.
	newznabPath atomic.Bool

	capsMu               sync.Mutex
	caps                 *Caps
	capsAt               time.Time
	capsEP               string    // capsEndpoint the cached caps came from
	capsFailAt           time.Time // last failed foreground caps request
	capsBackgroundFailAt time.Time

	// nzbFlights coalesces concurrent FetchNZB calls per release id, so
	// racing warm-up, playback and validation spend one grab.
	nzbFlightMu      sync.Mutex
	nzbFlights       map[string]*nzbFlight
	nzbRefusals      map[string]time.Time
	nzbStoreFailures map[string]time.Time

	cfgMu            sync.RWMutex
	eventMu          sync.RWMutex
	onEvent          func(Event)
	events           *events.Bus
	budgetEventMu    sync.Mutex
	budgetWarnDay    map[string]string
	budgetExhaustDay map[string]string

	// Resolved identity policy for this indexer, refreshed on every config reload.
	idMu   sync.RWMutex
	id     config.Identity
	policy config.ClientIdentity
}

// nzbFlight is one shared in-flight NZB fetch. done is closed after body
// and err are set. waiters counts callers that joined it (tests); readers
// counts those still to take the result. Both are guarded by nzbFlightMu.
type nzbFlight struct {
	done       chan struct{}
	body       []byte
	err        error
	cached     bool
	keep       bool // a plugin caller needs the grab persisted even if it leaves
	background bool // started by a background check (smaller grab share)
	waiters    int
	readers    int
	cancel     context.CancelFunc // stops the fetch once no caller waits
}

var _ core.Indexer = (*Client)(nil)

// New builds a Client for one indexer. db may be nil (budgets and cache
// then live in memory).
func New(cfg config.Indexer, db *sql.DB, opts Options) *Client {
	c := &Client{cfg: cfg, db: db, onEvent: opts.onEvent, events: opts.Events, vault: opts.Vault, legacyDir: opts.LegacyNZBDir, publicIndexer: opts.PublicIndexer, backends: opts.Backend}
	c.http = opts.HTTPClient
	if c.http == nil {
		c.http = egress.Client(egress.CapIndexers, defaultHTTPTimeout)
	}
	c.guarded = (&safefetch.Guard{}).Client(c.http)
	c.log = opts.Logger
	if c.log == nil {
		c.log = slog.Default()
	}
	c.now = opts.Now
	if c.now == nil {
		c.now = time.Now
	}
	switch {
	case opts.SearchCacheTTL < 0:
		c.cacheTTL = 0
	case opts.SearchCacheTTL == 0:
		c.cacheTTL = defaultCacheTTL
	default:
		c.cacheTTL = opts.SearchCacheTTL
	}
	c.maxNZB = opts.MaxNZBBytes
	if c.maxNZB <= 0 {
		c.maxNZB = defaultMaxNZBBytes
	}
	c.hosts = opts.HostLimiter
	if c.hosts == nil {
		c.hosts = NewHostLimiter(opts.MaxPerHost)
	}
	c.SetClientIdentity(opts.ClientIdentity)
	return c
}

func (c *Client) setOnEvent(fn func(Event)) {
	c.eventMu.Lock()
	c.onEvent = fn
	c.eventMu.Unlock()
}

func (c *Client) emit(e Event) {
	c.eventMu.RLock()
	fn := c.onEvent
	c.eventMu.RUnlock()
	if fn == nil {
		return
	}
	if e.Indexer == "" {
		e.Indexer = c.Name()
	}
	if e.At.IsZero() {
		e.At = c.now()
	}
	fn(e)
}

func searchEventKind(ctx context.Context) string {
	if core.IndexerPurpose(ctx) == core.IndexerPurposeFeed {
		return "feed"
	}
	return "search"
}

func averageReleaseAge(results []core.Release, now time.Time) time.Duration {
	var age time.Duration
	var dated int
	for _, r := range results {
		if !r.PubDate.IsZero() && now.After(r.PubDate) {
			age += now.Sub(r.PubDate)
			dated++
		}
	}
	if dated > 0 {
		age /= time.Duration(dated)
	}
	return age
}

func (c *Client) emitGrab(ctx context.Context, name string, started time.Time, reserved bool, err error, logWarn bool) {
	if err != nil && (errors.Is(err, context.Canceled) || ctx.Err() == context.Canceled) {
		return
	}
	if !reserved && (errors.Is(err, core.ErrReleaseRejected) || errors.Is(err, core.ErrForegroundOnly)) {
		return
	}
	localBudget := !reserved && errors.Is(err, core.ErrBudgetExhausted)
	err = logredact.SourceError(err)
	if err != nil && logWarn && !localBudget {
		c.log.Warn("indexer NZB fetch failed", "indexer", c.Name(), "release", logredact.Source(name), "err", err)
	}
	var dur time.Duration
	if !localBudget {
		dur = time.Since(started)
	}
	c.emit(Event{
		Kind:            "grab",
		Purpose:         usagePurpose(ctx),
		Success:         err == nil,
		BudgetExhausted: localBudget,
		SpeculativeHeld: localBudget && errors.Is(err, errSpeculativeShare),
		RateLimited:     reserved && errors.Is(err, core.ErrBudgetExhausted),
		Duration:        dur,
	})
	if err != nil && !localBudget {
		c.emitRequestFailed("grab", err, dur)
	}
}

// Config returns the indexer's current config.
func (c *Client) Config() config.Indexer {
	c.cfgMu.RLock()
	defer c.cfgMu.RUnlock()
	return c.cfg
}

// UpdateConfig swaps the indexer config (called on config reload).
func (c *Client) UpdateConfig(cfg config.Indexer) {
	c.cfgMu.Lock()
	changed := c.cfg.URL != cfg.URL || c.cfg.APIKey != cfg.APIKey
	c.cfg = cfg
	c.cfgMu.Unlock()
	c.idMu.Lock()
	c.id = c.policy.ResolveIndexer(cfg, config.RequestSearch, cfg.URL)
	c.idMu.Unlock()
	if changed {
		// A corrected URL or key deserves a fresh caps attempt, and the
		// old endpoint's caps no longer describe this indexer.
		c.newznabPath.Store(false)
		c.capsMu.Lock()
		c.capsFailAt, c.capsBackgroundFailAt = time.Time{}, time.Time{}
		c.caps, c.capsAt, c.capsEP = nil, time.Time{}, ""
		c.capsMu.Unlock()
		c.searchModeMu.Lock()
		c.searchMode = searchCatState{}
		c.searchModeMu.Unlock()
	}
}

// SetCacheTTL changes the result-cache TTL (called on config reload).
// A negative value disables the cache.
func (c *Client) SetCacheTTL(d time.Duration) {
	c.cfgMu.Lock()
	defer c.cfgMu.Unlock()
	if d < 0 {
		c.cacheTTL = 0
	} else {
		c.cacheTTL = d
	}
}

// SetClientIdentity stores the server-wide request identity rules and
// refreshes this indexer's resolved search/caps identity.
func (c *Client) SetClientIdentity(p config.ClientIdentity) {
	ix := c.Config()
	c.idMu.Lock()
	defer c.idMu.Unlock()
	c.policy = p
	c.id = p.ResolveIndexer(ix, config.RequestSearch, ix.URL)
}

// clientIdentity returns the resolved search/caps identity for this indexer.
func (c *Client) clientIdentity() config.Identity {
	c.idMu.RLock()
	defer c.idMu.RUnlock()
	if len(c.id.Ordered) == 0 && c.id.UserAgent == "" {
		// Built without SetClientIdentity (tests): resolve on the fly.
		ix := c.Config()
		return c.policy.ResolveIndexer(ix, config.RequestSearch, ix.URL)
	}
	return c.id
}

// identityFor resolves the request identity for a specific operation,
// destination URL (such as a feed-supplied NZB or .torrent URL on another
// host), and known media type.
func (c *Client) identityFor(kind config.RequestKind, rawURL string, media config.IdentityMediaType) config.Identity {
	ix := c.Config()
	c.idMu.RLock()
	policy := c.policy
	c.idMu.RUnlock()
	return policy.ResolveIndexerMedia(ix, kind, rawURL, media)
}

func (c *Client) searchIdentityFor(req core.SearchRequest) config.Identity {
	return c.identityFor(config.RequestSearch, c.Config().URL, config.IdentityMediaTypeForSearch(req))
}

func (c *Client) releaseMediaType(r core.Release) config.IdentityMediaType {
	cfg := c.Config()
	return config.IdentityMediaTypeForRelease(r, cfg.MovieCategories, cfg.TVCategories)
}

// Name implements core.Indexer.
func (c *Client) Name() string {
	cfg := c.Config()
	if cfg.Name != "" {
		return cfg.Name
	}
	if cfg.ID != "" {
		return cfg.ID
	}
	if u, err := url.Parse(cfg.URL); err == nil && u.Host != "" {
		return u.Host
	}
	return cfg.URL
}

// zileanTorznabBase turns an owner-entered Zilean base URL into the base
// of its Torznab API. Zilean serves Torznab anonymously at
// {base}/torznab/api, so the owner only pastes the instance address
// (e.g. http://zilean:8181). An address that already names the endpoint
// (…/torznab or …/torznab/api) is left as the endpoint so editing an
// existing entry never doubles the path.
func zileanTorznabBase(base string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	for {
		lower := strings.ToLower(base)
		switch {
		case strings.HasSuffix(lower, "/torznab/api"):
			base = base[:len(base)-len("/torznab/api")]
		case strings.HasSuffix(lower, "/api"):
			base = base[:len(base)-len("/api")]
		case strings.HasSuffix(lower, "/torznab"):
			base = base[:len(base)-len("/torznab")]
		default:
			return base + "/torznab"
		}
	}
}

func (c *Client) apiURL(params url.Values) (string, error) {
	return c.apiURLFor(params, c.clientIdentity())
}

// explicitEndpoint reports whether the owner's URL already names the API
// endpoint: it ends in /api, or in /newznab (an /api/newznab/ endpoint, entered
// the way Radarr's API Path field takes it).
func explicitEndpoint(path string) bool {
	lower := strings.ToLower(strings.TrimRight(path, "/"))
	return strings.HasSuffix(lower, "/api") || strings.HasSuffix(lower, "/newznab")
}

// endpointPath turns the owner's URL path into the API endpoint: unchanged
// when it already names one, otherwise /api, or /api/newznab once that is
// what this indexer answered on.
func (c *Client) endpointPath(path string) string {
	return endpointPathFor(path, c.newznabPath.Load())
}

func endpointPathFor(path string, useNewznab bool) string {
	if explicitEndpoint(path) {
		return path
	}
	if useNewznab {
		return path + "/api/newznab"
	}
	return path + "/api"
}

// canFallbackNewznabPath reports whether a bare-host /api 404 error may be
// retried once against {base}/api/newznab. Unlike a CompareAndSwap gate, any
// concurrent request that raced on /api retries safely and only persists
// newznabPath after /api/newznab succeeds.
func (c *Client) canFallbackNewznabPath(err error, usedNewznab bool) bool {
	if usedNewznab {
		return false
	}
	var ae *APIError
	if !errors.As(err, &ae) || ae.Code != http.StatusNotFound {
		return false
	}
	cfg := c.Config()
	if cfg.IsZilean() {
		return false
	}
	if u, perr := url.Parse(strings.TrimSpace(cfg.URL)); perr == nil && explicitEndpoint(u.Path) {
		return false
	}
	return true
}

// rememberNewznabPath flips the in-memory /api/newznab flag and persists it
// into the cached Caps row so restarts reuse /api/newznab without hitting 404.
func (c *Client) rememberNewznabPath(ctx context.Context) {
	c.newznabPath.Store(true)
	ep := c.capsEndpoint()
	var updated *Caps
	c.capsMu.Lock()
	if c.caps != nil && c.capsEP == ep && !c.caps.NewznabPath {
		cp := *c.caps
		cp.NewznabPath = true
		c.caps = &cp
		updated = &cp
	}
	c.capsMu.Unlock()
	if updated != nil {
		c.capsToDB(ctx, updated, ep)
	}
}

func (c *Client) apiURLFor(params url.Values, id config.Identity) (string, error) {
	return c.apiURLForPath(params, id, c.newznabPath.Load())
}

func (c *Client) apiURLForPath(params url.Values, id config.Identity, useNewznab bool) (string, error) {
	cfg := c.Config()
	raw := strings.TrimSpace(cfg.URL)
	if raw == "" {
		return "", fmt.Errorf("newznab: indexer %q has no URL", c.Name())
	}
	if cfg.IsZilean() {
		// Zilean's Torznab API is appended to the owner's base URL; the
		// rest of the Torznab client (search, caps, grab) is unchanged.
		raw = zileanTorznabBase(raw)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		base := endpointPathFor(strings.TrimRight(raw, "/"), useNewznab)
		q := url.Values{}
		for k, vs := range params {
			q[k] = vs
		}
		if cfg.APIKey != "" && q.Get("apikey") == "" {
			q.Set("apikey", cfg.APIKey)
		}
		return base + "?" + id.ApplyParams(q), nil
	}
	u.Path = endpointPathFor(strings.TrimRight(u.Path, "/"), useNewznab)
	q := u.Query()
	q.Del("t")
	for k, vs := range params {
		q[k] = vs
	}
	if cfg.APIKey != "" && q.Get("apikey") == "" {
		q.Set("apikey", cfg.APIKey)
	}
	u.RawQuery = id.ApplyParams(q)
	return u.String(), nil
}

// identityOrigin returns the origin to which non-User-Agent headers of id are
// confined. Rules targeting a host or download type apply to the matched
// request's own origin; indexer and search rules stay confined to the
// configured indexer origin so a feed-supplied link on another host never
// receives indexer credentials.
func (c *Client) identityOrigin(reqURL string, id config.Identity) string {
	switch id.RuleTarget {
	case config.TargetHost, config.TargetNZBDownloads, config.TargetTorrentDownloads:
		return reqURL
	default:
		return c.Config().URL
	}
}

// applyIdentity writes the resolved client identity on one request.
func (c *Client) applyIdentity(req *http.Request, id config.Identity) {
	req.Header.Set("User-Agent", id.UserAgent)
	if !safefetch.SameOrigin(c.identityOrigin(req.URL.String(), id), req.URL.String()) {
		return
	}
	ordered := id.Ordered
	if len(ordered) == 0 {
		for k, v := range id.Headers {
			req.Header.Set(k, v)
		}
		return
	}
	for _, h := range ordered {
		switch {
		case strings.EqualFold(h.Name, "User-Agent"):
			req.Header.Set("User-Agent", h.Value)
		case strings.EqualFold(h.Name, "Connection"):
			if strings.EqualFold(h.Value, "close") {
				req.Close = true
			}
		default:
			req.Header.Set(h.Name, h.Value)
		}
	}
}

// send performs req with non-User-Agent headers confined to the matched origin:
// net/http copies non-sensitive headers onto a redirect, including one that
// leaves the origin.
func (c *Client) send(req *http.Request, id config.Identity) (*http.Response, error) {
	if len(id.Headers) == 0 && len(id.Ordered) <= 1 {
		return c.guarded.Do(req)
	}
	origin := c.identityOrigin(req.URL.String(), id)
	hc := *c.guarded
	inner := hc.CheckRedirect
	hc.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		c.dropForeignIdentityHeaders(next, origin, id)
		if inner != nil {
			return inner(next, via)
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	return hc.Do(req)
}

// dropForeignIdentityHeaders clears non-User-Agent identity headers from a
// redirect that leaves origin.
func (c *Client) dropForeignIdentityHeaders(next *http.Request, origin string, id config.Identity) {
	if safefetch.SameOrigin(origin, next.URL.String()) {
		return
	}
	for k := range id.Headers {
		next.Header.Del(k)
	}
	for _, h := range id.Ordered {
		if !strings.EqualFold(h.Name, "User-Agent") {
			next.Header.Del(h.Name)
		}
	}
}

// decodeBody undoes a Content-Encoding the transport left in place because
// the request identity set Accept-Encoding itself.
func decodeBody(resp *http.Response) (io.ReadCloser, error) {
	if resp.Uncompressed {
		return resp.Body, nil
	}
	switch strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding"))) {
	case "", "identity":
		return resp.Body, nil
	case "gzip":
		zr, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, err
		}
		return zr, nil
	case "deflate":
		return flate.NewReader(resp.Body), nil
	default:
		return nil, fmt.Errorf("newznab: indexer replied with Content-Encoding %q Ether cannot read; adjust the Accept-Encoding header in the request identity", resp.Header.Get("Content-Encoding"))
	}
}

// trustedCtx selects the safefetch policy for one fetch explicitly:
// the trusted-origin policy only when rawURL targets exactly the
// indexer origin the owner configured (a LAN indexer is legitimate);
// every other URL — an absolute NZB URL from a feed, a download link on
// another host — is fetched with the public policy from the start.
// Redirects are re-routed per hop by safefetch, so a redirect away
// from the indexer also lands in the public pool. When the owner
// configured no usable base URL (direct internal calls in tests), the
// fetch URL's own origin takes its place.
func (c *Client) trustedCtx(ctx context.Context, rawURL string) context.Context {
	trusted := c.Config().URL
	if u, err := url.Parse(trusted); err != nil || u.Host == "" {
		trusted = rawURL
	}
	return safefetch.WithTrustedOriginFor(ctx, trusted, rawURL)
}

// get performs a GET and returns the body, mapping HTTP failures to
// typed API errors. Responses are size-limited.
func (c *Client) get(ctx context.Context, rawURL string, limit int64, id config.Identity) ([]byte, error) {
	hctx, releaseHost, err := c.acquireHost(ctx, rawURL)
	if err != nil {
		return nil, err
	}
	defer releaseHost()
	req, err := http.NewRequestWithContext(c.trustedCtx(hctx, rawURL), http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, redactURLError(err)
	}
	c.applyIdentity(req, id)
	resp, err := c.send(req, id)
	if err != nil {
		// url.Error embeds the request URL, which carries the indexer API
		// key; callers log and surface this error.
		var ue *url.Error
		if errors.As(err, &ue) {
			return nil, fmt.Errorf("newznab: request to %q failed: %w", c.Name(), ue.Err)
		}
		return nil, err
	}
	defer resp.Body.Close()
	// Record throttling before reading the body: even a broken error response
	// must not cause every new play request to hammer a throttled indexer.
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, c.recordRateLimit(hctx, resp.Header.Get("Retry-After"), rateLimitResponse(resp, c.now()))
	}
	decoded, derr := decodeBody(resp)
	if derr != nil {
		return nil, derr
	}
	body, err := io.ReadAll(io.LimitReader(decoded, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("newznab: response exceeds %d bytes", limit)
	}
	c.noteAPILimit(hctx, body)
	// Error documents can arrive with HTTP 200 or with a failure status.
	// A release-specific refusal takes precedence over account quota backoff.
	hasErrorDocument := bytes.Contains(bytes.ToLower(body[:min(len(body), 4096)]), []byte("error"))
	if resp.StatusCode != http.StatusOK || hasErrorDocument {
		if _, perr := parseFeed(body, c.Name()); perr != nil {
			var ae *APIError
			if errors.As(perr, &ae) {
				now := c.now()
				if refusal := ClassifyGrabRefusal(ae, now); refusal != nil {
					refusal.setRetryAfter(resp.Header.Get("Retry-After"), now)
					return nil, refusal
				}
				if ae.Budget && hasErrorDocument {
					return nil, c.recordRateLimit(hctx, resp.Header.Get("Retry-After"), perr)
				}
				return nil, perr
			}
		}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, apiError(resp.StatusCode, resp.Status)
	}
	return body, nil
}

// searchIdentity fingerprints every indexer setting that changes what a
// search returns.
func searchIdentity(cfg config.Indexer) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x00%s\x00%v\x00%v", cfg.URL, cfg.APIKey, cfg.Protocol, cfg.Kind, cfg.Definition, cfg.MovieCategories, cfg.TVCategories)
	for _, m := range []map[string]string{cfg.Settings, cfg.SecretSettings} {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(h, "\x00%s=%s", k, m[k])
		}
		h.Write([]byte{1})
	}
	return hex.EncodeToString(h.Sum(nil)[:12])
}

// capsEndpoint fingerprints the URL and API key caps are fetched from.
func (c *Client) capsEndpoint() string {
	cfg := c.Config()
	sum := sha256.Sum256([]byte(cfg.URL + "\x00" + cfg.APIKey))
	return hex.EncodeToString(sum[:12])
}

// Caps returns the indexer's capabilities, cached for 24h per endpoint
// (URL and API key): an indexer edited to point elsewhere fetches anew,
// and a response from the old endpoint still in flight is not kept.
func (c *Client) Caps(ctx context.Context) (*Caps, error) {
	ep := c.capsEndpoint()
	background := backgroundSearch(ctx)
	c.capsMu.Lock()
	if c.caps != nil && c.capsEP == ep && c.now().Sub(c.capsAt) < 24*time.Hour {
		defer c.capsMu.Unlock()
		return c.caps, nil
	}
	failedAt := c.capsFailAt
	if background && c.capsBackgroundFailAt.After(failedAt) {
		failedAt = c.capsBackgroundFailAt
	}
	if !failedAt.IsZero() && c.now().Sub(failedAt) < capsRetry {
		c.capsMu.Unlock()
		return nil, errCapsUnavailable
	}
	c.capsMu.Unlock()
	if caps, ok := c.capsFromDB(ctx, ep); ok {
		if caps.NewznabPath {
			c.newznabPath.Store(true)
		}
		c.capsMu.Lock()
		if c.capsEndpoint() == ep {
			c.caps, c.capsAt, c.capsEP = caps, c.now(), ep
		}
		c.capsMu.Unlock()
		return caps, nil
	}
	capsStarted := time.Now()
	caps, err := c.fetchCaps(ctx)
	capsElapsed := time.Since(capsStarted)
	c.capsMu.Lock()
	current := c.capsEndpoint() == ep // the config did not change meanwhile
	if err != nil {
		if current && !callerLeft(ctx, capsElapsed) && !errors.Is(err, core.ErrBudgetExhausted) {
			if background {
				c.capsBackgroundFailAt = c.now()
			} else {
				c.capsFailAt = c.now()
			}
		}
		c.capsMu.Unlock()
		return nil, err
	}
	if current {
		c.caps, c.capsAt, c.capsEP = caps, c.now(), ep
		c.capsFailAt, c.capsBackgroundFailAt = time.Time{}, time.Time{}
	}
	c.capsMu.Unlock()
	if current {
		c.capsToDB(ctx, caps, ep)
	}
	return caps, nil
}

func (c *Client) fetchCaps(ctx context.Context) (*Caps, error) {
	id := c.clientIdentity()
	usedNewznab := c.newznabPath.Load()
	u, err := c.apiURLForPath(url.Values{"t": {"caps"}}, id, usedNewznab)
	if err != nil {
		return nil, err
	}
	hctx, releaseHost, err := c.acquireHost(ctx, u)
	if err != nil {
		return nil, err
	}
	defer releaseHost()
	if err := c.reserve(capsPurpose(hctx), "hit"); err != nil {
		return nil, err
	}
	body, err := c.get(hctx, u, 5<<20, id)
	if err != nil && c.canFallbackNewznabPath(err, usedNewznab) {
		alt, aerr := c.apiURLForPath(url.Values{"t": {"caps"}}, id, true)
		if aerr != nil {
			return nil, aerr
		}
		if err := c.reserve(capsPurpose(hctx), "hit"); err != nil {
			return nil, err
		}
		if body, err = c.get(hctx, alt, 5<<20, id); err == nil {
			usedNewznab = true
			c.newznabPath.Store(true)
		}
	}
	if err != nil {
		return nil, err
	}
	if trimmed := bytes.TrimSpace(body); len(trimmed) > 0 && trimmed[0] != '{' {
		// Error documents reach here with HTTP 200.
		if _, perr := parseFeed(body, c.Name()); perr != nil {
			return nil, perr
		}
	}
	caps, err := parseCaps(body)
	if err != nil {
		return nil, fmt.Errorf("newznab: parse caps: %w", err)
	}
	caps.NewznabPath = usedNewznab
	return caps, nil
}

func cacheKey(c *Client, req core.SearchRequest) string {
	h := sha1.New()
	fmt.Fprintf(h, "aliases:%v|only:%t|", core.EpisodeAliases(req), req.ReleaseAliasesOnly)
	if req.AltEpisode > 0 {
		fmt.Fprintf(h, "alt:%d:%d|", req.AltSeason, req.AltEpisode)
	}
	// Cached releases include parsed audio-language facts. Earlier names
	// with regional tags were persisted as unknown language; refresh them
	// instead of applying corrected ranking only after their TTL expires.
	// v2: title boundaries and year-like titles parse correctly.
	// v3: cam variants, groups, UHD-sourced 1080p, title words before
	// the episode marker and remaster years parse correctly.
	// v4: subtitle-only tags (VOSTFR), Latino/Brazilian variants, Dual
	// Audio and anime absolute episode numbers; Stremio addons keep HTTP
	// and Usenet streams and read flag languages.
	// v5: anime BD/BDMux source tags and three-letter audio languages.
	// v6: anime absolute numbers followed by language/edition tags ("- 750 - ENG UNCUT 1080p").
	// v7: Usenet poster and upload date preserved for cross-indexer content identity.
	// v8: Brazilian/Portuguese release language variants (PT.BR, PTBR, Dublado/Legendado, slash/plus delimiters).
	fmt.Fprint(h, "release-parse-v8|")
	if mode := c.currentSearchCatMode(); mode != searchCatDefault {
		fmt.Fprintf(h, "catmode:%d|", mode)
	}
	if req.Category != "" {
		fmt.Fprintf(h, "category:%s|", req.Category)
		if req.Lookup != nil && c.Config().IsAddon() {
			// Only plugin sources read a typed lookup; other indexers
			// search the text alone and keep sharing its cache entry.
			keys := make([]string, 0, len(req.Lookup.Fields))
			for k := range req.Lookup.Fields {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			fmt.Fprintf(h, "lookup:%s", req.Lookup.Type)
			for _, k := range keys {
				fmt.Fprintf(h, ":%s=%s", k, req.Lookup.Fields[k])
			}
			fmt.Fprint(h, "|")
		}
	}
	if req.Type == core.TypeEpisode && req.Season == 0 && (c.Config().IsStremio() || c.Config().IsAddon()) {
		// Previously these routes cached empty answers without asking season 0.
		fmt.Fprintf(h, "special-title-map-v1:%s|", req.EpisodeTitle)
	}
	if c.Config().IsBuiltin() && (req.Type == core.TypeEpisode || req.Type == core.TypeShow || req.Season > 0) {
		// Built-in TV keywords no longer require the series premiere year.
		// Do not reuse persisted empty answers from that narrower search.
		fmt.Fprint(h, "tv-keywords-v2|")
	}
	if !c.Config().UsesBackend() && !c.Config().IsTorrent() && isTVRequest(req) && req.Season > 0 && req.Episode > 0 {
		// Usenet episode searches now fall back to season-pack queries when
		// episode-specific queries miss; do not reuse persisted empty answers.
		fmt.Fprint(h, "usenet-season-pack-v1|")
	}
	// The indexer's search identity: an edited URL, key, category list or
	// built-in site setting must not be answered from the old config's
	// results (in memory, or persisted for a fresh client).
	fmt.Fprintf(h, "routed:%s|", searchIdentity(c.Config()))
	// The client identity decides outbound headers and parameter overrides: a
	// cached answer fetched under one identity must not serve another.
	fmt.Fprintf(h, "client-id-v2:%s|", c.searchIdentityFor(req).Fingerprint())
	fmt.Fprintf(h, "%s|%s|%d|%s|%d|%s|%d|%d|%d",
		c.indexerID(), req.Type, req.IDs.TMDB, req.IDs.IMDB, req.IDs.TVDB,
		strings.ToLower(strings.TrimSpace(req.Title)), req.Year, req.Season, req.Episode)
	if req.EpisodeSource == core.MetadataTVDB {
		fmt.Fprintf(h, "|tvdb:%s", req.Released.UTC().Format(time.DateOnly))
	}
	if orig := strings.ToLower(strings.TrimSpace(req.OriginalTitle)); orig != "" && orig != strings.ToLower(strings.TrimSpace(req.Title)) {
		fmt.Fprintf(h, "|%s", orig)
	}
	if eng := strings.ToLower(strings.TrimSpace(req.EnglishTitle)); eng != "" && eng != strings.ToLower(strings.TrimSpace(req.Title)) && eng != strings.ToLower(strings.TrimSpace(req.OriginalTitle)) {
		fmt.Fprintf(h, "|eng:%s", eng)
	}
	for _, sub := range subtitleQueryAliases(req) {
		fmt.Fprintf(h, "|sub:%s", strings.ToLower(sub))
	}
	// Anime requests search more (absolute numbers, kitsu ids); keys of
	// every other request stay as they were.
	if req.Anime {
		fmt.Fprintf(h, "|anime:%d", req.AbsoluteEpisode)
		if req.Cour > 0 && req.CourEpisode > 0 {
			fmt.Fprintf(h, "|cour:%d:%d", req.Cour, req.CourEpisode)
		}
		if req.CourTVDBSeason > 0 && req.CourTVDBEpisode > 0 {
			fmt.Fprintf(h, "|courtvdb:%d:%d", req.CourTVDBSeason, req.CourTVDBEpisode)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

func joinCats(cats []int) string {
	parts := make([]string, len(cats))
	for i, n := range cats {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ",")
}

func subtitleQueryAliases(req core.SearchRequest) []string {
	var out []string
	seen := map[string]bool{}
	addFrom := func(s string) {
		if s == "" {
			return
		}
		for _, a := range releaseparse.SubtitleAliases(s, req.AlternativeTitles) {
			k := strings.ToLower(strings.TrimSpace(a))
			if k == "" || seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, a)
		}
	}
	addFrom(req.Title)
	addFrom(req.OriginalTitle)
	addFrom(req.EnglishTitle)
	return out
}

// queryTitles returns the distinct search titles to try for req: Title
// first, then OriginalTitle and EnglishTitle (when distinct), then structural
// aliases and subtitle halves confirmed by AlternativeTitles.
func queryTitles(req core.SearchRequest) []string {
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		k := strings.ToLower(s)
		if seen[k] {
			return
		}
		seen[k] = true
		out = append(out, s)
	}
	// Indexers match release names, which never carry punctuation such as
	// colons, and often drop diacritics. Like Sonarr and Radarr, search the
	// cleaned title; an accented title is also searched unaccented.
	addClean := func(s string) {
		c := cleanSearchTitle(s)
		add(c)
		if f := foldSearchTitle(c); f != c {
			add(f)
		}
	}
	addClean(req.Title)
	if req.OriginalTitle != "" && !releaseparse.TitlesMatch(req.OriginalTitle, req.Title) {
		addClean(req.OriginalTitle)
	}
	if req.EnglishTitle != "" && !releaseparse.TitlesMatch(req.EnglishTitle, req.Title) &&
		(req.OriginalTitle == "" || !releaseparse.TitlesMatch(req.EnglishTitle, req.OriginalTitle)) {
		addClean(req.EnglishTitle)
	}
	for _, a := range releaseparse.TitleAliases(req.Title) {
		addClean(a)
	}
	if req.OriginalTitle != "" {
		for _, a := range releaseparse.TitleAliases(req.OriginalTitle) {
			addClean(a)
		}
	}
	if req.EnglishTitle != "" {
		for _, a := range releaseparse.TitleAliases(req.EnglishTitle) {
			addClean(a)
		}
	}
	for _, a := range subtitleQueryAliases(req) {
		addClean(a)
	}
	return out
}

// cleanSearchTitle drops the punctuation release names never contain
// (colons, quotes, brackets, commas…) and collapses spaces; hyphens stay
// ("Spider-Man"). Apostrophes are
// removed without a gap ("Ocean's" → "Oceans"); "&" is kept.
func cleanSearchTitle(s string) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		switch {
		case r == '\'' || r == '’' || r == '‘' || r == '`':
			continue
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '&' || r == '-':
			if space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			space = false
			b.WriteRune(r)
		default:
			space = true
		}
	}
	return b.String()
}

// foldSearchTitle removes diacritics ("Anéis" → "Aneis").
func foldSearchTitle(s string) string {
	var b strings.Builder
	for _, r := range norm.NFD.String(s) {
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		b.WriteRune(r)
	}
	return norm.NFC.String(b.String())
}

// isTVRequest reports whether a search asks about a show rather than a movie.
func isTVRequest(req core.SearchRequest) bool {
	return req.Type == core.TypeEpisode || req.Season > 0 || req.Type == core.TypeShow
}

func interleaveAttempts(lists [][]url.Values) []url.Values {
	var out []url.Values
	seen := map[string]bool{}
	for i := 0; ; i++ {
		any := false
		for _, list := range lists {
			if i < len(list) {
				any = true
				enc := list[i].Encode()
				if !seen[enc] {
					seen[enc] = true
					out = append(out, list[i])
				}
			}
		}
		if !any {
			break
		}
	}
	return out
}

func (c *Client) searchAttemptsSplit(req core.SearchRequest, caps *Caps) (primary, packs []url.Values) {
	if req.Category != "" || req.Type != core.TypeEpisode {
		return c.numberedSearchAttemptsSplit(req, caps)
	}
	var epLists, packLists [][]url.Values
	if !req.ReleaseAliasesOnly {
		ep, pk := c.numberedSearchAttemptsSplit(req, caps)
		epLists = append(epLists, ep)
		packLists = append(packLists, pk)
	}
	for _, n := range core.EpisodeAliases(req) {
		ep, pk := c.numberedSearchAttemptsSplit(core.AliasSearchRequest(req, n), caps)
		epLists = append(epLists, ep)
		packLists = append(packLists, pk)
	}
	return interleaveAttempts(epLists), interleaveAttempts(packLists)
}

// searchAttempts builds the ordered fallback queries for a request.
func (c *Client) searchAttempts(req core.SearchRequest, caps *Caps) []url.Values {
	primary, packs := c.searchAttemptsSplit(req, caps)
	if len(packs) == 0 {
		return primary
	}
	seen := make(map[string]bool, len(primary))
	for _, p := range primary {
		seen[p.Encode()] = true
	}
	out := slices.Clone(primary)
	for _, p := range packs {
		enc := p.Encode()
		if !seen[enc] {
			seen[enc] = true
			out = append(out, p)
		}
	}
	return out
}

func ordinalSeason(n int) string {
	suffix := "th"
	if n%100 < 11 || n%100 > 13 {
		switch n % 10 {
		case 1:
			suffix = "st"
		case 2:
			suffix = "nd"
		case 3:
			suffix = "rd"
		}
	}
	return fmt.Sprintf("%d%s Season", n, suffix)
}

func (c *Client) numberedSearchAttemptsSplit(req core.SearchRequest, caps *Caps) ([]url.Values, []url.Values) {
	cfg := c.Config()
	if req.Category != "" {
		cats, err := core.SearchCategories(req.Category)
		if err != nil {
			return nil, nil
		}
		v := url.Values{"extended": {"1"}, "limit": {strconv.Itoa(defaultSearchLimit)}, "t": {"search"}, "cat": {joinCats(cats)}}
		if q := strings.TrimSpace(req.Title); q != "" {
			v.Set("q", q)
		}
		return []url.Values{v}, nil
	}
	isTV := isTVRequest(req)
	catMode := c.currentSearchCatMode()
	base := func() url.Values {
		v := url.Values{"extended": {"1"}, "limit": {strconv.Itoa(defaultSearchLimit)}}
		applyDefaultSearchCats(v, cfg, caps, isTV, catMode)
		return v
	}
	imdb := strings.TrimPrefix(req.IDs.IMDB, "tt")
	titles := queryTitles(req)
	var attempts []url.Values
	if isTV {
		tvdbOrder := req.EpisodeSource == core.MetadataTVDB
		mkWith := func(idKey, idVal string, season, episode int) url.Values {
			v := base()
			v.Set("t", "tvsearch")
			if idKey != "" {
				v.Set(idKey, idVal)
			}
			if season > 0 || (tvdbOrder && episode > 0 && idKey == "tvdbid") {
				v.Set("season", strconv.Itoa(season))
			}
			if episode > 0 {
				v.Set("ep", strconv.Itoa(episode))
			}
			return v
		}
		mk := func(idKey, idVal string) url.Values {
			return mkWith(idKey, idVal, req.Season, req.Episode)
		}
		if req.IDs.TVDB > 0 && (caps == nil || (!caps.TVAvail || hasParam(caps.TVParams, "tvdbid"))) {
			tvdbSeason, tvdbEp := req.Season, req.Episode
			if req.CourTVDBSeason > 0 && req.Episode > 0 && req.CourTVDBEpisode > 0 {
				tvdbSeason, tvdbEp = req.CourTVDBSeason, req.CourTVDBEpisode
			} else if req.CourTVDBSeason > 0 && req.Episode == 0 {
				tvdbSeason = req.CourTVDBSeason
			}
			attempts = append(attempts, mkWith("tvdbid", strconv.FormatInt(req.IDs.TVDB, 10), tvdbSeason, tvdbEp))
		}
		if !tvdbOrder && req.IDs.TMDB > 0 && (caps == nil || (!caps.TVAvail || hasParam(caps.TVParams, "tmdbid"))) {
			attempts = append(attempts, mk("tmdbid", strconv.FormatInt(req.IDs.TMDB, 10)))
		}
		if !tvdbOrder && imdb != "" && (caps == nil || (!caps.TVAvail || hasParam(caps.TVParams, "imdbid"))) {
			attempts = append(attempts, mk("imdbid", imdb))
		}
		if tvdbOrder && req.Episode > 0 && !req.Released.IsZero() {
			date := req.Released.UTC().Format("2006.01.02")
			for _, title := range titles {
				v := base()
				v.Set("t", "search")
				v.Set("q", title+" "+date)
				attempts = append(attempts, v)
			}
		}
		for _, title := range titles {
			q := title
			switch {
			case (req.Season > 0 || tvdbOrder) && req.Episode > 0:
				q = fmt.Sprintf("%s S%02dE%02d", title, req.Season, req.Episode)
			case req.Season > 0:
				q = fmt.Sprintf("%s S%02d", title, req.Season)
			}
			v := mk("", "")
			v.Set("q", q)
			attempts = append(attempts, v)
			v2 := base()
			v2.Set("t", "search")
			v2.Set("q", q)
			attempts = append(attempts, v2)
		}
		// Long TMDB anime seasons often map to separate TVDB seasons in
		// Fribb/anime-lists ("S03E01", "3rd Season - 01"); search those before
		// bare absolute numbers so season-1 batches do not short-circuit.
		if req.CourTVDBSeason > 0 && req.CourTVDBEpisode > 0 && req.Episode > 0 {
			for _, title := range titles {
				if req.CourTVDBSeason != req.Season || req.CourTVDBEpisode != req.Episode {
					q := fmt.Sprintf("%s S%02dE%02d", title, req.CourTVDBSeason, req.CourTVDBEpisode)
					v := mkWith("", "", req.CourTVDBSeason, req.CourTVDBEpisode)
					v.Set("q", q)
					attempts = append(attempts, v)
					v2 := base()
					v2.Set("t", "search")
					v2.Set("q", q)
					attempts = append(attempts, v2)
				}
				vOrd := base()
				vOrd.Set("t", "search")
				vOrd.Set("q", fmt.Sprintf("%s %s - %02d", title, ordinalSeason(req.CourTVDBSeason), req.CourTVDBEpisode))
				attempts = append(attempts, vOrd)
			}
		}
		// Anime releases are numbered from the series start ("Show - 1071")
		// and indexers rarely map them to seasons: search the absolute
		// number too. Identity checks still require the title and number.
		if abs := releaseparse.AnimeSearchEpisode(req); abs > 0 {
			for _, title := range titles {
				v := base()
				v.Set("t", "search")
				v.Set("q", fmt.Sprintf("%s %02d", title, abs))
				attempts = append(attempts, v)
			}
		}
		// Multi-cour anime seasons ("Show S01 Part 2 - 01") reset episode
		// numbers per cour; search the mapped cour and relative episode too.
		if req.Cour > 0 && req.CourEpisode > 0 && req.Season > 0 {
			for _, title := range titles {
				for _, q := range []string{
					fmt.Sprintf("%s S%02d Part %d %02d", title, req.Season, req.Cour, req.CourEpisode),
					fmt.Sprintf("%s Part %d %02d", title, req.Cour, req.CourEpisode),
				} {
					v := base()
					v.Set("t", "search")
					v.Set("q", q)
					attempts = append(attempts, v)
				}
			}
		}
		// Season-pack fallback: TV shows on both torrent and Usenet indexers
		// often exist only as full-season packs, which episode-specific queries
		// (ep=N / SxxExx) miss. Usenet runs targeted ID and "Sxx" queries;
		// Torznab also adds "tvsearch", "Season N" and "Complete" queries.
		var packs []url.Values
		if req.Season > 0 {
			mkSeasonWith := func(idKey, idVal string, season int) url.Values {
				v := base()
				v.Set("t", "tvsearch")
				if idKey != "" {
					v.Set(idKey, idVal)
				}
				v.Set("season", strconv.Itoa(season))
				return v
			}
			mkSeason := func(idKey, idVal string) url.Values {
				return mkSeasonWith(idKey, idVal, req.Season)
			}
			if req.Episode > 0 {
				if req.IDs.TVDB > 0 && (caps == nil || (!caps.TVAvail || hasParam(caps.TVParams, "tvdbid"))) {
					tvdbSeason := req.Season
					if req.CourTVDBSeason > 0 {
						tvdbSeason = req.CourTVDBSeason
					}
					packs = append(packs, mkSeasonWith("tvdbid", strconv.FormatInt(req.IDs.TVDB, 10), tvdbSeason))
				}
				if !tvdbOrder && req.IDs.TMDB > 0 && (caps == nil || (!caps.TVAvail || hasParam(caps.TVParams, "tmdbid"))) {
					packs = append(packs, mkSeason("tmdbid", strconv.FormatInt(req.IDs.TMDB, 10)))
				}
				if !tvdbOrder && imdb != "" && (caps == nil || (!caps.TVAvail || hasParam(caps.TVParams, "imdbid"))) {
					packs = append(packs, mkSeason("imdbid", imdb))
				}
				if req.CourTVDBSeason > 0 && req.CourTVDBSeason != req.Season {
					for _, title := range titles {
						q := fmt.Sprintf("%s S%02d", title, req.CourTVDBSeason)
						if cfg.IsTorrent() {
							v := mkSeasonWith("", "", req.CourTVDBSeason)
							v.Set("q", q)
							packs = append(packs, v)
							for _, sq := range []string{
								q,
								fmt.Sprintf("%s %s", title, ordinalSeason(req.CourTVDBSeason)),
								fmt.Sprintf("%s Season %d", title, req.CourTVDBSeason),
							} {
								v2 := base()
								v2.Set("t", "search")
								v2.Set("q", sq)
								packs = append(packs, v2)
							}
						} else {
							v2 := base()
							v2.Set("t", "search")
							v2.Set("q", q)
							packs = append(packs, v2)
						}
					}
				}
				for _, title := range titles {
					q := fmt.Sprintf("%s S%02d", title, req.Season)
					if cfg.IsTorrent() {
						v := mkSeason("", "")
						v.Set("q", q)
						packs = append(packs, v)
					}
					v2 := base()
					v2.Set("t", "search")
					v2.Set("q", q)
					packs = append(packs, v2)
				}
			}
			if cfg.IsTorrent() {
				for _, title := range titles {
					v := base()
					v.Set("t", "search")
					v.Set("q", fmt.Sprintf("%s Season %d", title, req.Season))
					packs = append(packs, v)
					vc := base()
					vc.Set("t", "search")
					vc.Set("q", title+" Complete")
					packs = append(packs, vc)
				}
			}
		}
		return attempts, packs
	}
	if imdb != "" && (caps == nil || (!caps.MovieAvail || hasParam(caps.MovieParams, "imdbid"))) {
		v := base()
		v.Set("t", "movie")
		v.Set("imdbid", imdb)
		attempts = append(attempts, v)
	}
	if req.IDs.TMDB > 0 && (caps == nil || (!caps.MovieAvail || hasParam(caps.MovieParams, "tmdbid"))) {
		v := base()
		v.Set("t", "movie")
		v.Set("tmdbid", strconv.FormatInt(req.IDs.TMDB, 10))
		attempts = append(attempts, v)
	}
	for _, title := range titles {
		q := title
		if req.Year > 0 {
			q = fmt.Sprintf("%s %d", title, req.Year)
		}
		v := base()
		v.Set("t", "search")
		v.Set("q", q)
		attempts = append(attempts, v)
	}
	return attempts, nil
}

func stampMissingCategory(results []core.Release, media config.IdentityMediaType) {
	cat := config.DefaultCategoryForIdentityMedia(media)
	if cat == 0 {
		return
	}
	for i := range results {
		if results[i].Category == 0 {
			results[i].Category = cat
		}
	}
}

// doSearch caches each exact wire query across title, rescue and plugin paths.
func (c *Client) doSearch(ctx context.Context, params url.Values, id config.Identity, media config.IdentityMediaType) ([]core.Release, error) {
	if !hasTitleQuery(params) {
		if core.IndexerPurpose(ctx) != core.IndexerPurposeFeed {
			return nil, fmt.Errorf("newznab: search request has no usable identifiers")
		}
		// Availability paginates a changing feed using its saved boundary.
		return c.requestSearch(ctx, params, id, media)
	}
	if core.IndexerPurpose(ctx) == core.IndexerPurposeTest {
		return c.requestSearch(ctx, params, id, media)
	}
	hash := sha256.New()
	fmt.Fprintf(hash, "%s|%s|%s|%v|%s", c.indexerID(), searchIdentity(c.Config()), id.Fingerprint(), media, params.Encode())
	key := "query:" + hex.EncodeToString(hash.Sum(nil))
	emit, _ := ctx.Value(searchArrivalKey{}).(func([]core.Release))
	return c.queryFlights.run(ctx, key, emit, func() ([]core.Release, error) {
		if cached, ok := c.cacheGet(ctx, key); ok {
			if emit != nil && len(cached) > 0 {
				emit(slices.Clone(cached))
			}
			return cached, nil
		}
		rs, err := c.requestSearch(ctx, params, id, media)
		if err == nil {
			c.cachePut(context.WithoutCancel(ctx), key, rs)
		}
		return rs, err
	})
}

func (c *Client) requestSearch(ctx context.Context, params url.Values, id config.Identity, media config.IdentityMediaType) ([]core.Release, error) {
	if err := c.searchPaused(ctx); err != nil {
		return nil, err
	}
	usedNewznab := c.newznabPath.Load()
	u, err := c.apiURLForPath(params, id, usedNewznab)
	if err != nil {
		return nil, err
	}
	parsed, err := url.Parse(u)
	if err != nil {
		return nil, redactURLError(err)
	}
	if !hasTitleQuery(parsed.Query()) && core.IndexerPurpose(ctx) != core.IndexerPurposeFeed {
		return nil, fmt.Errorf("newznab: search request has no usable identifiers")
	}
	started := time.Now()
	admitted := false
	reserved := false
	results, err := c.guardSearch(ctx, u, true, defaultHTTPTimeout, func(ctx context.Context) ([]core.Release, error) {
		admitted = true
		started = time.Now()
		// The hit is reserved once the breaker has admitted the request,
		// right before it is sent: the budget and the usage breakdown count
		// HTTP requests the indexer actually receives, as Prowlarr does.
		if err := c.reserve(ctx, "hit"); err != nil {
			return nil, err
		}
		reserved = true
		if emit, ok := ctx.Value(searchArrivalKey{}).(func([]core.Release)); ok {
			wrappedEmit := emit
			if config.DefaultCategoryForIdentityMedia(media) != 0 {
				wrappedEmit = func(rs []core.Release) {
					stampMissingCategory(rs, media)
					emit(rs)
				}
			}
			rs, serr := c.getSearchStream(ctx, u, wrappedEmit, id)
			if serr != nil && c.canFallbackNewznabPath(serr, usedNewznab) {
				// {base}/api answered 404: retry once on {base}/api/newznab.
				alt, aerr := c.apiURLForPath(params, id, true)
				if aerr != nil {
					return nil, aerr
				}
				if err := c.reserve(ctx, "hit"); err != nil {
					return nil, err
				}
				if rs, serr = c.getSearchStream(ctx, alt, wrappedEmit, id); serr == nil {
					c.rememberNewznabPath(ctx)
				}
			}
			return rs, serr
		}
		body, err := c.get(ctx, u, 20<<20, id)
		if err != nil && c.canFallbackNewznabPath(err, usedNewznab) {
			// {base}/api answered 404: retry once on {base}/api/newznab.
			alt, aerr := c.apiURLForPath(params, id, true)
			if aerr != nil {
				return nil, aerr
			}
			if err := c.reserve(ctx, "hit"); err != nil {
				return nil, err
			}
			if body, err = c.get(ctx, alt, 20<<20, id); err == nil {
				c.rememberNewznabPath(ctx)
			}
		}
		if err != nil {
			return nil, err
		}
		return parseFeed(body, c.Name())
	})
	if err == nil && c.Config().IsTorrent() {
		// A Torznab indexer's results are torrents even when an item
		// carries only a .torrent download link.
		for i := range results {
			results[i].Protocol = core.ProtocolTorrent
		}
	}
	if err == nil {
		stampMissingCategory(results, media)
	}
	out := c.feedReleases(results)
	if admitted && !(err != nil && (errors.Is(err, context.Canceled) || errors.Is(err, ErrIndexerPaused))) {
		localBudget := !reserved && errors.Is(err, core.ErrBudgetExhausted)
		now := c.now()
		var dur, age time.Duration
		if !localBudget {
			dur = time.Since(started)
			age = averageReleaseAge(out, now)
		}
		c.emit(Event{
			Kind:            searchEventKind(ctx),
			Purpose:         usagePurpose(ctx),
			Success:         err == nil,
			Empty:           err == nil && len(out) == 0,
			BudgetExhausted: localBudget,
			SpeculativeHeld: localBudget && errors.Is(err, errSpeculativeShare),
			RateLimited:     reserved && errors.Is(err, core.ErrBudgetExhausted),
			Results:         len(out),
			Duration:        dur,
			AverageAge:      age,
			At:              now,
		})
	}
	c.noteQuery(ctx, params.Get("t"), params.Get("q"), params, len(out), err)
	return out, err
}

// Search implements core.Indexer.
// primaryMissKey suffixes the cache key of a primary query that found
// nothing for speculative work, which never asks the fallbacks.
const primaryMissKey = "|primary-miss"

func (c *Client) Search(ctx context.Context, req core.SearchRequest) ([]core.Release, error) {
	cats, err := core.SearchCategories(req.Category)
	if err != nil {
		return nil, err
	}
	cfg := c.Config()
	if namedAnimeSource(cfg) && !isAnimeSearch(req) {
		return nil, nil
	}
	if req.Category != "" && (cfg.IsStremio() || cfg.IsAddon()) && !c.backendAnswersCategories(ctx, cfg, cats) {
		return nil, fmt.Errorf("indexer %q does not support category searches", c.Name())
	}
	if !cfg.Enabled {
		return nil, fmt.Errorf("indexer %q is disabled", c.Name())
	}
	if core.TorrentOnlySearch(ctx) && !cfg.IsTorrent() {
		return nil, nil
	}
	identity := searchIdentity(cfg)
	catState := c.loadSearchCatState(ctx, identity)
	usesDefaultCat := !cfg.UsesBackend() && c.usesDefaultSearchCats(cfg, req)
	key := cacheKey(c, req)
	ctx = withCacheAge(ctx, req.Released)
	if req.Category != "" && strings.TrimSpace(req.Title) == "" && (!cfg.UsesBackend() || req.Lookup == nil) {
		return c.cachedCategoryListing(ctx, cats)
	}
	if cached, ok := c.cacheGet(ctx, key); ok {
		c.emit(Event{Kind: "cache", Purpose: usagePurpose(ctx), Success: true, Cached: true, Results: len(cached)})
		c.noteQuery(ctx, "cache", req.Title, nil, len(cached), nil)
		return cached, nil
	}
	speculative := core.SpeculativeIndexerWork(ctx) || core.IsBackgroundCheck(ctx)
	if speculative {
		// A primary query that already missed for speculative work.
		if cached, ok := c.cacheGet(ctx, key+primaryMissKey); ok {
			c.emit(Event{Kind: "cache", Purpose: usagePurpose(ctx), Success: true, Cached: true, Results: len(cached)})
			return cached, nil
		}
	}
	if cacheOnlySearch(ctx, c) {
		return nil, core.ErrForegroundOnly
	}
	// Exhausted limits prohibit new requests, not reuse of already fetched
	// results and NZBs. The manager must reach this cache before this check.
	if c.searchExhausted(ctx) {
		c.emit(Event{Kind: "search", Purpose: usagePurpose(ctx), BudgetExhausted: true})
		return nil, fmt.Errorf("indexer %q: %w: %w", c.Name(), errLocalBudget, core.ErrBudgetExhausted)
	}
	if err := c.searchPaused(ctx); err != nil {
		return nil, err
	}
	if c.Config().UsesBackend() {
		results, incomplete, err := c.backendSearch(ctx, req)
		if err != nil {
			return nil, err
		}
		if !incomplete {
			c.cachePut(ctx, key, results)
		}
		return results, nil
	}
	media := config.IdentityMediaTypeForSearch(req)
	id := c.identityFor(config.RequestSearch, cfg.URL, media)
	capsStarted := time.Now()
	br := c.searchBreaker(ctx)
	caps, capsErr := c.Caps(ctx) // best effort; failures just mean no param gating
	if capsErr != nil && ctx.Err() != nil {
		capsElapsed := time.Since(capsStarted)
		if !callerLeft(ctx, capsElapsed) {
			c.recordSearchTimeout(ctx, br, capsErr, capsElapsed)
		}
		return nil, capsErr
	}
	cats = routedCategories(cfg, caps, req)
	if len(cats) == 0 {
		return nil, nil
	}
	attempts := c.searchAttempts(req, caps)
	usesDefaultCat = usesDefaultCat && !animeOnly(cfg, caps)
	routeSearchAttempts(attempts, cats, usesDefaultCat, catState.mode)
	if len(attempts) == 0 {
		return nil, fmt.Errorf("newznab: search request has no usable identifiers")
	}
	searchCtx := ctx
	unfiltered := attempts[0].Get("cat") == ""
	if unfiltered {
		if emit, ok := ctx.Value(searchArrivalKey{}).(func([]core.Release)); ok {
			searchCtx = context.WithValue(ctx, searchArrivalKey{}, func(rs []core.Release) {
				if kept := filterVideoReleases(rs); len(kept) > 0 {
					emit(kept)
				}
			})
		}
	}
	confirmCatMode := func() {
		if usesDefaultCat && !catState.verified {
			catState.verified = true
			catState.checked = c.now()
			c.saveSearchCatState(ctx, catState)
		}
	}
	var firstErr error
	results, err := c.doSearch(searchCtx, attempts[0], id, media)
	if err == nil && unfiltered {
		results = filterVideoReleases(results)
	}
	if err != nil {
		if errors.Is(err, core.ErrBudgetExhausted) {
			return nil, err
		}
		firstErr = err
		c.log.Debug("newznab search attempt failed", "indexer", c.Name(), "t", attempts[0].Get("t"), "err", err)
		if transientFailure(err) {
			// The indexer did not answer at all (timeout, outage, 5xx):
			// the fallback queries would each wait out the same failure.
			return nil, err
		}
	} else if len(results) > 0 {
		confirmCatMode()
		c.cachePut(ctx, key, results)
		return results, nil
	}
	if speculative {
		// Speculative work (warm-ups, detail discovery, watchlist, cache
		// fill, availability) asks the targeted primary query only. The
		// fallback queries (other ids, keyword and alias searches, season
		// packs) cost a request each and are kept for a viewer's Play, whose
		// rescue also runs them when nothing found here plays. The miss is
		// remembered for speculative work only: Play still runs fallbacks.
		if firstErr == nil {
			c.cachePut(ctx, key+primaryMissKey, nil)
		}
		return nil, firstErr
	}
	rest := attempts[1:]
	if len(rest) > 0 {
		// When the primary attempt misses on an uncapped indexer, fan out the
		// fallback queries (alternate IDs, keyword search, OriginalTitle/aliases,
		// season packs) in bounded parallel bursts so obscure titles resolve in
		// a single round-trip instead of N serial timeouts. An indexer with a
		// personal cap tries them one at a time and stops at the first hit.
		if c.DailyAPILimit() == 0 && firstErr == nil {
			const maxBurstWidth = 4
			for base := 0; base < len(rest); {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				width := c.searchBreaker(ctx).burstLimit(maxBurstWidth)
				end := min(base+width, len(rest))
				batch := rest[base:end]
				base = end
				type outSlot struct {
					res []core.Release
					err error
				}
				slots := make([]outSlot, len(batch))
				var wg sync.WaitGroup
				for i, p := range batch {
					wg.Add(1)
					go func(i int, p url.Values) {
						defer wg.Done()
						res, derr := c.doSearch(searchCtx, p, id, media)
						if derr == nil && unfiltered {
							res = filterVideoReleases(res)
						}
						slots[i].res, slots[i].err = res, derr
					}(i, p)
				}
				wg.Wait()
				var merged []core.Release
				seen := map[string]bool{}
				var budgetErr error
				down := false
				for _, s := range slots {
					if s.err != nil {
						if errors.Is(s.err, core.ErrBudgetExhausted) && budgetErr == nil {
							budgetErr = s.err
						} else if firstErr == nil {
							firstErr = s.err
						}
						down = down || transientFailure(s.err)
						continue
					}
					for _, r := range s.res {
						k := r.ID
						if r.InfoHash != "" {
							k = r.InfoHash
						}
						if !seen[k] {
							seen[k] = true
							merged = append(merged, r)
						}
					}
				}
				if len(merged) > 0 {
					confirmCatMode()
					c.cachePut(ctx, key, merged)
					return merged, nil
				}
				if budgetErr != nil {
					return nil, budgetErr
				}
				if down {
					return nil, firstErr
				}
			}
		} else {
			for _, params := range rest {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				results, err := c.doSearch(searchCtx, params, id, media)
				if err == nil && unfiltered {
					results = filterVideoReleases(results)
				}
				if err != nil {
					if errors.Is(err, core.ErrBudgetExhausted) {
						return nil, err
					}
					if firstErr == nil {
						firstErr = err
					}
					c.log.Debug("newznab search attempt failed", "indexer", c.Name(), "t", params.Get("t"), "err", err)
					if transientFailure(err) {
						return nil, err
					}
					continue
				}
				if len(results) > 0 {
					confirmCatMode()
					c.cachePut(ctx, key, results)
					return results, nil
				}
			}
		}
	}
	if firstErr == nil && usesDefaultCat && !catState.verified && catState.mode == searchCatDefault && !c.now().Before(catState.probeBackoffUntil) &&
		len(capsSubcategories(caps, isTVRequest(req))) > 0 {
		probed, mode, perr := c.probeBroaderSearchCats(ctx, req, caps, attempts, id, media, identity)
		if perr != nil {
			if errors.Is(perr, core.ErrBudgetExhausted) || transientFailure(perr) {
				return nil, perr
			}
			firstErr = perr
		} else if len(probed) > 0 {
			if mode != searchCatDefault {
				key = cacheKey(c, req)
			}
			c.cachePut(ctx, key, probed)
			return probed, nil
		}
	}
	if firstErr != nil {
		return nil, firstErr
	}
	c.cachePut(ctx, key, nil)
	return nil, nil
}

// rescueAttempts builds broader keyword, alias, season-pack and miniseries
// queries for a second-pass rescue when the primary search returned only
// dead/unhealthy or non-matching releases.
func (c *Client) rescueAttempts(req core.SearchRequest, caps *Caps) []url.Values {
	cfg := c.Config()
	isTV := isTVRequest(req)
	catMode := c.currentSearchCatMode()
	base := func() url.Values {
		v := url.Values{"extended": {"1"}, "limit": {strconv.Itoa(defaultSearchLimit)}}
		applyDefaultSearchCats(v, cfg, caps, isTV, catMode)
		return v
	}
	primary, packs := c.searchAttemptsSplit(req, caps)
	alreadyFirst := ""
	if len(primary) > 0 {
		alreadyFirst = primary[0].Encode()
	}
	seen := map[string]bool{}
	if alreadyFirst != "" {
		seen[alreadyFirst] = true
	}
	var out []url.Values
	add := func(v url.Values) {
		enc := v.Encode()
		if enc == "" || seen[enc] {
			return
		}
		seen[enc] = true
		out = append(out, v)
	}
	for _, p := range primary[min(1, len(primary)):] {
		add(p)
	}
	titles := queryTitles(req)
	altTitles := rescueAlternativeTitles(req, titles)
	nonLatinOriginal := hasNonLatinRune(req.OriginalTitle)
	if isTV {
		// Prioritize distinct alternative/English titles (e.g. "I Live Alone"
		// when TMDB's primary English title is "Home Alone") with explicit
		// episode/bare-episode/air-date queries before falling back to broad
		// season-pack queries.
		if req.Season > 0 && req.Episode > 0 {
			for _, alt := range altTitles {
				vSE := base()
				vSE.Set("t", "search")
				vSE.Set("q", fmt.Sprintf("%s S%02dE%02d", alt, req.Season, req.Episode))
				add(vSE)
				if req.Season == 1 || req.Episode >= 100 || nonLatinOriginal {
					vEp := base()
					vEp.Set("t", "search")
					vEp.Set("q", fmt.Sprintf("%s E%02d", alt, req.Episode))
					add(vEp)
				}
			}
			// Bare E### and broadcast air-date queries for long-running /
			// Asian variety shows that omit Sxx (e.g. "I Live Alone E674",
			// "나 혼자 산다 E674", "I Live Alone 241115").
			if req.Season == 1 || req.Episode >= 100 || nonLatinOriginal {
				for _, title := range titles {
					if req.Episode > 20 || nonLatinOriginal {
						vEp := base()
						vEp.Set("t", "search")
						vEp.Set("q", fmt.Sprintf("%s E%02d", title, req.Episode))
						add(vEp)
					}
				}
			}
			if !req.Released.IsZero() && (req.EpisodeSource == core.MetadataTVDB || nonLatinOriginal || (req.Season == 1 && req.Episode > 20)) {
				// Every title's broadcast-style YYMMDD date before any
				// YYYY.MM.DD form, so the capped rescue reaches the date
				// form Korean releases use for each title.
				for _, layout := range []string{"060102", "2006.01.02"} {
					date := req.Released.UTC().Format(layout)
					for _, title := range append(slices.Clone(altTitles), titles...) {
						v := base()
						v.Set("t", "search")
						v.Set("q", fmt.Sprintf("%s %s", title, date))
						add(v)
					}
				}
			}
		}
		for _, p := range packs {
			add(p)
		}
		for _, title := range titles {
			if req.Season > 0 && req.Episode > 0 {
				// Season pack queries (mapped TVDB season first, then S01 / Season 1 / Complete).
				if req.CourTVDBSeason > 0 && req.CourTVDBSeason != req.Season {
					for _, sq := range []string{
						fmt.Sprintf("%s S%02d", title, req.CourTVDBSeason),
						fmt.Sprintf("%s %s", title, ordinalSeason(req.CourTVDBSeason)),
						fmt.Sprintf("%s Season %d", title, req.CourTVDBSeason),
					} {
						vTVDB := base()
						vTVDB.Set("t", "search")
						vTVDB.Set("q", sq)
						add(vTVDB)
					}
				}
				vS := base()
				vS.Set("t", "search")
				vS.Set("q", fmt.Sprintf("%s S%02d", title, req.Season))
				add(vS)
				vSeason := base()
				vSeason.Set("t", "search")
				vSeason.Set("q", fmt.Sprintf("%s Season %d", title, req.Season))
				add(vSeason)
				if req.Season == 1 && req.Episode <= 20 {
					vPart := base()
					vPart.Set("t", "search")
					vPart.Set("q", fmt.Sprintf("%s Part %d", title, req.Episode))
					add(vPart)
					vEp := base()
					vEp.Set("t", "search")
					vEp.Set("q", fmt.Sprintf("%s E%02d", title, req.Episode))
					add(vEp)
				}
			}
			vComp := base()
			vComp.Set("t", "search")
			vComp.Set("q", title+" Complete")
			add(vComp)
		}
		return out
	}
	// Movies: also try bare title without year for uploads whose indexed
	// release name omitted the year or used a festival release year.
	for _, title := range append(slices.Clone(titles), altTitles...) {
		v := base()
		v.Set("t", "search")
		v.Set("q", title)
		add(v)
	}
	return out
}

// rescueAttemptLimit bounds Play's second-pass queries per indexer. Weekly
// and daily broadcast shows numbered by bare episode or air date (long single
// seasons, non-Latin originals) need each title's episode and date forms on
// top of the retried primary queries, so they get a few more.
func rescueAttemptLimit(req core.SearchRequest) int {
	if isTVRequest(req) && req.Season > 0 && req.Episode > 0 && !req.Released.IsZero() &&
		(hasNonLatinRune(req.OriginalTitle) || (req.Season == 1 && req.Episode > 20)) {
		return 12
	}
	return 8
}

func hasNonLatinRune(s string) bool {
	for _, r := range s {
		if r >= 128 && !unicode.In(r, unicode.Latin) {
			return true
		}
	}
	return false
}

// rescueAlternativeTitles picks up to 2 distinct, clean alternative/English
// titles not already covered by queryTitles (preferring Latin/ASCII aliases
// for foreign-script shows, such as "I Live Alone" for "나 혼자 산다" / "Home Alone").
func rescueAlternativeTitles(req core.SearchRequest, existing []string) []string {
	var latin, other []string
	seen := map[string]bool{}
	for _, e := range existing {
		seen[strings.ToLower(cleanSearchTitle(e))] = true
	}
	addCand := func(raw string) {
		c := cleanSearchTitle(raw)
		if c == "" || len([]rune(c)) < 3 || len([]rune(c)) > 64 {
			return
		}
		k := strings.ToLower(c)
		if seen[k] {
			return
		}
		for _, e := range existing {
			if releaseparse.TitlesMatch(c, e) {
				return
			}
		}
		seen[k] = true
		if hasNonLatinRune(c) {
			other = append(other, c)
		} else {
			latin = append(latin, c)
		}
	}
	addCand(req.EnglishTitle)
	for _, alt := range req.AlternativeTitles {
		addCand(alt)
	}
	out := append(latin, other...)
	if len(out) > 2 {
		out = out[:2]
	}
	return out
}

// SearchRescue runs broader keyword, alias, OriginalTitle, miniseries and
// season-pack queries when the primary Search results had no playable
// candidate. Results are cached under a separate rescue key and respect
// the indexer's daily budget and rate-limit state.
func (c *Client) SearchRescue(ctx context.Context, req core.SearchRequest) ([]core.Release, error) {
	if cfg := c.Config(); !cfg.Enabled {
		return nil, fmt.Errorf("indexer %q is disabled", c.Name())
	}
	if core.SpeculativeIndexerWork(ctx) || core.IsBackgroundCheck(ctx) {
		// Rescue is Play's second pass; speculative work never fans out.
		return nil, nil
	}
	cfg := c.Config()
	if namedAnimeSource(cfg) && !isAnimeSearch(req) {
		return nil, nil
	}
	catState := c.loadSearchCatState(ctx, searchIdentity(cfg))
	key := cacheKey(c, req) + "|rescue"
	ctx = withCacheAge(core.WithSearchRescue(ctx), req.Released)
	if cached, ok := c.cacheGet(ctx, key); ok {
		c.emit(Event{Kind: "cache", Purpose: usagePurpose(ctx), Success: true, Cached: true, Results: len(cached)})
		c.noteQuery(ctx, "cache", req.Title, nil, len(cached), nil)
		return cached, nil
	}
	if c.searchExhausted(ctx) {
		c.emit(Event{Kind: "search", Purpose: usagePurpose(ctx), BudgetExhausted: true})
		return nil, fmt.Errorf("indexer %q: %w: %w", c.Name(), errLocalBudget, core.ErrBudgetExhausted)
	}
	if err := c.searchPaused(ctx); err != nil {
		return nil, err
	}
	if cfg := c.Config(); cfg.UsesBackend() {
		// Built-in sites and addons: if Episode > 0, rescue via season-level
		// search; built-in sites also via OriginalTitle/aliases (an addon
		// searches by identifier, so another title asks the same question).
		var merged []core.Release
		seen := map[string]bool{}
		incomplete := false
		var firstErr error
		appendUnique := func(rs []core.Release) {
			for _, r := range rs {
				k := r.ID
				if r.InfoHash != "" {
					k = r.InfoHash
				}
				if !seen[k] {
					seen[k] = true
					merged = append(merged, r)
				}
			}
		}
		run := func(r core.SearchRequest) error {
			rs, inc, err := c.backendSearch(ctx, r)
			if inc {
				incomplete = true
			}
			if err != nil {
				if ctx.Err() != nil {
					return err
				}
				incomplete = true
				core.NoteSearchIncomplete(ctx)
				if firstErr == nil {
					firstErr = err
				}
				return nil
			}
			appendUnique(rs)
			return nil
		}
		if !cfg.IsStremio() && !cfg.IsAddon() {
			if req.OriginalTitle != "" && !releaseparse.TitlesMatch(req.OriginalTitle, req.Title) {
				alt := req
				alt.Title, alt.OriginalTitle = req.OriginalTitle, ""
				if err := run(alt); err != nil {
					return nil, err
				}
			}
			for _, altTitle := range rescueAlternativeTitles(req, queryTitles(req))[:min(1, len(rescueAlternativeTitles(req, queryTitles(req))))] {
				alt := req
				alt.Title, alt.OriginalTitle = altTitle, ""
				if err := run(alt); err != nil {
					return nil, err
				}
			}
		}
		if req.Season > 0 && req.Episode > 0 {
			if req.CourTVDBSeason > 0 && req.CourTVDBSeason != req.Season {
				tvdbPack := req
				tvdbPack.Season, tvdbPack.Episode = req.CourTVDBSeason, 0
				if err := run(tvdbPack); err != nil {
					return nil, err
				}
			}
			packReq := req
			packReq.Episode = 0
			if err := run(packReq); err != nil {
				return nil, err
			}
		}
		if len(merged) == 0 && firstErr != nil {
			return nil, firstErr
		}
		if !incomplete {
			c.cachePut(ctx, key, merged)
		}
		return merged, nil
	}
	media := config.IdentityMediaTypeForSearch(req)
	id := c.identityFor(config.RequestSearch, c.Config().URL, media)
	caps, _ := c.Caps(ctx)
	cats := routedCategories(cfg, caps, req)
	if len(cats) == 0 {
		return nil, nil
	}
	usesDefaultCat := c.usesDefaultSearchCats(cfg, req) && !animeOnly(cfg, caps)
	attempts := c.rescueAttempts(req, caps)
	routeSearchAttempts(attempts, cats, usesDefaultCat, catState.mode)
	unfiltered := len(attempts) > 0 && attempts[0].Get("cat") == ""
	if len(attempts) == 0 {
		return nil, nil
	}
	if limit := rescueAttemptLimit(req); len(attempts) > limit {
		attempts = attempts[:limit]
	}
	var merged []core.Release
	seen := map[string]bool{}
	var firstErr error
	// As for Search: a capped indexer's rescue goes one query at a time.
	if c.Config().DailyLimit == 0 {
		for base := 0; base < len(attempts); {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			width := c.searchBreaker(ctx).burstLimit(len(attempts))
			end := min(base+width, len(attempts))
			batch := attempts[base:end]
			base = end
			type slot struct {
				res []core.Release
				err error
			}
			slots := make([]slot, len(batch))
			var wg sync.WaitGroup
			for i, p := range batch {
				wg.Add(1)
				go func(i int, p url.Values) {
					defer wg.Done()
					res, derr := c.doSearch(ctx, p, id, media)
					if derr == nil && unfiltered {
						res = filterVideoReleases(res)
					}
					slots[i].res, slots[i].err = res, derr
				}(i, p)
			}
			wg.Wait()
			var budgetErr error
			down := false
			for _, s := range slots {
				if s.err != nil {
					if errors.Is(s.err, core.ErrBudgetExhausted) && budgetErr == nil {
						budgetErr = s.err
					} else if firstErr == nil {
						firstErr = s.err
					}
					down = down || transientFailure(s.err)
					continue
				}
				for _, r := range s.res {
					k := r.ID
					if r.InfoHash != "" {
						k = r.InfoHash
					}
					if !seen[k] {
						seen[k] = true
						merged = append(merged, r)
					}
				}
			}
			if budgetErr != nil || down {
				if firstErr == nil {
					firstErr = budgetErr
				}
				break
			}
		}
	} else {
		for _, p := range attempts {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			res, err := c.doSearch(ctx, p, id, media)
			if err == nil && unfiltered {
				res = filterVideoReleases(res)
			}
			if err != nil {
				if errors.Is(err, core.ErrBudgetExhausted) {
					// A refused query proves nothing: never cache it as a miss.
					firstErr = err
					break
				}
				if firstErr == nil {
					firstErr = err
				}
				if transientFailure(err) {
					break
				}
				continue
			}
			for _, r := range res {
				k := r.ID
				if r.InfoHash != "" {
					k = r.InfoHash
				}
				if !seen[k] {
					seen[k] = true
					merged = append(merged, r)
				}
			}
			if len(merged) > 0 {
				break
			}
		}
	}
	if len(merged) == 0 && firstErr != nil {
		return nil, firstErr
	}
	if firstErr != nil {
		core.NoteSearchIncomplete(ctx)
	} else {
		c.cachePut(ctx, key, merged)
	}
	return merged, nil
}

func nzbFlightKey(r core.Release) string {
	if r.Target != nil && r.Target.PackID != "" {
		return r.Target.PackID
	}
	return r.ID
}

// FetchNZB implements core.Indexer. Every call returns a slice of its own,
// which the caller may keep or modify.
func (c *Client) FetchNZB(ctx context.Context, r core.Release) ([]byte, error) {
	body, _, err := c.fetchNZBReporting(ctx, r)
	return body, err
}

// fetchNZBReporting is FetchNZB, also reporting whether this call asked
// the indexer: false for an NZB read from the vault or one taken from
// another caller's download. Only the indexer's own downloads are grabs.
func (c *Client) fetchNZBReporting(ctx context.Context, r core.Release) ([]byte, bool, error) {
	if !c.Config().Enabled {
		return nil, false, core.ErrNotAvailable
	}
	if r.IndexerID != "" && r.IndexerID != c.indexerID() {
		return nil, false, fmt.Errorf("newznab: release belongs to another originating indexer")
	}
	if r.Protocol == core.ProtocolTorrent {
		return nil, false, fmt.Errorf("newznab: %q is a torrent release; it streams through a debrid provider", r.Name)
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	flightKey := nzbFlightKey(r)
	if flightKey == "" {
		started := time.Now()
		body, reserved, err := c.fetchNZB(ctx, r)
		c.emitGrab(ctx, r.Name, started, reserved, err, true)
		return body, true, err
	}
	for {
		f, leader := c.joinNZBFlight(ctx, flightKey, r)
		select {
		case <-f.done:
		case <-ctx.Done():
			// An abandoned plugin grab can still reach the vault for its retry.
			c.leaveNZB(flightKey, f)
			return nil, leader, ctx.Err()
		}
		body := c.takeNZB(f)
		// A background check runs on a reduced grab share; a foreground
		// caller must not inherit its budget refusal.
		if !leader && f.background {
			foreground := !backgroundSearch(ctx)
			budgetRefused := foreground && errors.Is(f.err, core.ErrBudgetExhausted)
			grabAllowed := (foreground || core.SpeculativeGrabsAllowed(ctx)) && errors.Is(f.err, core.ErrForegroundOnly)
			if budgetRefused || grabAllowed {
				continue
			}
		}
		if f.err != nil {
			return nil, leader, f.err
		}
		if !leader {
			c.emit(Event{Kind: "cache", Purpose: usagePurpose(ctx), Success: true, Cached: true})
		}
		return body, leader && !f.cached, nil
	}
}

// takeNZB gives one caller of f, after f.done, its own copy of the
// fetched body. Copies are made under the lock, and only the last caller
// to take one gets the original, so a caller may modify what it got: no
// other caller is still reading it. A lone caller (the usual case) pays no
// copy.
func (c *Client) takeNZB(f *nzbFlight) []byte {
	c.nzbFlightMu.Lock()
	defer c.nzbFlightMu.Unlock()
	f.readers--
	if f.readers > 0 {
		return bytes.Clone(f.body)
	}
	return f.body
}

// leaveNZB gives up a caller's turn without reading the body, which may
// still be in flight. The last caller to leave an unfinished fetch cancels
// it and forgets it, so a later caller starts a fetch of its own.
func (c *Client) leaveNZB(id string, f *nzbFlight) {
	c.nzbFlightMu.Lock()
	defer c.nzbFlightMu.Unlock()
	f.readers--
	if f.readers > 0 || f.keep {
		return
	}
	select {
	case <-f.done:
		return
	default:
	}
	if c.nzbFlights[id] == f {
		delete(c.nzbFlights, id)
	}
	f.cancel()
}

// joinNZBFlight attaches to the in-flight fetch for the pack or release ID, or starts one.
// The shared fetch keeps ctx's values (budget class, trust) but not its
// cancellation. Plugin grabs can finish after the last caller leaves.
// Every fetch is bounded by nzbFlightTimeout.
func (c *Client) joinNZBFlight(ctx context.Context, id string, r core.Release) (*nzbFlight, bool) {
	c.nzbFlightMu.Lock()
	defer c.nzbFlightMu.Unlock()
	if f, ok := c.nzbFlights[id]; ok {
		f.keep = f.keep || isPluginNZB(ctx)
		f.waiters++
		f.readers++
		return f, false
	}
	if c.nzbFlights == nil {
		c.nzbFlights = map[string]*nzbFlight{}
	}
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), nzbFlightTimeout)
	f := &nzbFlight{done: make(chan struct{}), background: backgroundSearch(ctx), keep: isPluginNZB(ctx), waiters: 1, readers: 1, cancel: cancel}
	c.nzbFlights[id] = f
	go func() {
		defer func() {
			if p := recover(); p != nil {
				f.err = fmt.Errorf("newznab: fetch NZB panic: %v", p)
			}
			c.nzbFlightMu.Lock()
			if c.nzbFlights[id] == f {
				delete(c.nzbFlights, id)
			}
			c.nzbFlightMu.Unlock()
			close(f.done)
			cancel()
		}()
		// A known vault hit never waits for another indexer grab.
		loadKept := func() bool {
			body, err := c.keptNZB(fctx, r)
			if body == nil && err == nil {
				return false
			}
			f.body, f.cached, f.err = body, body != nil, err
			if body != nil {
				c.emit(Event{Kind: "cache", Purpose: usagePurpose(fctx), Success: true, Cached: true})
			}
			return true
		}
		if loadKept() {
			return
		}
		// Recheck after taking the shared lock. Another node may have
		// saved the release while this caller waited.
		unlock, err := c.lockNZBGrab(fctx, id)
		if err != nil {
			f.err = err
			return
		}
		if unlock != nil {
			defer unlock()
			if loadKept() {
				return
			}
		}
		started := time.Now()
		var reserved bool

		if f.err = c.nzbRefusal(fctx, r); f.err != nil {
			return
		}
		f.body, reserved, f.err = c.fetchNZB(fctx, r)
		c.emitGrab(fctx, r.Name, started, reserved, f.err, true)
		now := c.now()
		refusal := ClassifyGrabRefusal(f.err, now)
		var apiErr *APIError
		if refusal == nil && isPluginNZB(fctx) && errors.As(f.err, &apiErr) && apiErr.Code == http.StatusConflict {
			refusal = &GrabRefusal{RetryAt: now.Add(defaultRateLimitPause)}
		}
		if refusal != nil {
			f.body, f.err = c.keptNZB(fctx, r)
			f.cached = f.body != nil
			if f.body == nil && f.err == nil {
				f.err = refusal
				c.rememberNZBRefusal(fctx, r, refusal.RetryAt)
			}
		}
	}()
	return f, true
}

// fetchNZB downloads r's NZB from the indexer, spending one grab, and
// stores it in the vault. The bool reports whether a grab slot was reserved.
func (c *Client) fetchNZB(ctx context.Context, r core.Release) ([]byte, bool, error) {
	if backgroundSearch(ctx) && !core.SpeculativeGrabsAllowed(ctx) {
		return nil, false, core.ErrForegroundOnly
	}
	if cfg := c.Config(); cfg.UsesBackend() {
		b, err := c.backend()
		if err != nil {
			return nil, false, err
		}
		fetchRel := r
		if fetchRel.NZBURL != "" {
			authURL, aerr := c.authenticateLink(fetchRel.NZBURL)
			if aerr != nil {
				return nil, false, aerr
			}
			fetchRel.NZBURL = authURL
		}
		hctx := ctx
		if cfg.IsStremio() {
			target := fetchRel.NZBURL
			if target == "" {
				target = cfg.URL
			}
			var releaseHost func()
			hctx, releaseHost, err = c.acquireHost(ctx, target)
			if err != nil {
				return nil, false, err
			}
			defer releaseHost()
		}
		reserved := false
		body, err := b.FetchNZB(hctx, fetchRel, func(rctx context.Context, kind string) error {
			if err := c.reserve(rctx, kind); err != nil {
				return err
			}
			reserved = true
			return nil
		})
		if err != nil {
			return nil, reserved, err
		}
		c.keepNZB(ctx, r, body)
		return body, reserved, nil
	}
	// r.NZBURL comes from the indexer's feed, so it is untrusted data:
	// c.get dials it through the safefetch guard. Only a URL that stays
	// on the owner-configured indexer origin (relative links resolve
	// there) may reach a private address; absolute feed URLs to any
	// other host, and redirects away from the indexer, must be public.
	var id config.Identity
	media := c.releaseMediaType(r)
	rawURL := r.NZBURL
	var guidParams url.Values
	usedNewznab := false
	if rawURL == "" {
		if r.GUID == "" {
			return nil, false, fmt.Errorf("newznab: release %q has no NZB URL or GUID: %w", r.Name, core.ErrReleaseRejected)
		}
		id = c.identityFor(config.RequestNZBDownload, c.Config().URL, media)
		guidParams = url.Values{"t": {"get"}, "id": {r.GUID}}
		usedNewznab = c.newznabPath.Load()
		u, err := c.apiURLForPath(guidParams, id, usedNewznab)
		if err != nil {
			return nil, false, err
		}
		rawURL = u
	} else {
		var err error
		rawURL, err = c.resolveLink(rawURL)
		if err != nil {
			return nil, false, err
		}
		rawURL, err = c.authenticateLink(rawURL)
		if err != nil {
			return nil, false, err
		}
		id = c.identityFor(config.RequestNZBDownload, rawURL, media)
		rawURL = id.ApplyToURL(rawURL)
	}
	hctx, releaseHost, err := c.acquireHost(ctx, rawURL)
	if err != nil {
		return nil, false, err
	}
	defer releaseHost()
	// Reserved right before the request, so a release with no usable URL
	// spends nothing.
	if err := c.reserve(hctx, "grab"); err != nil {
		return nil, false, err
	}
	body, err := c.get(hctx, rawURL, c.maxNZB, id)
	if err != nil && guidParams != nil && c.canFallbackNewznabPath(err, usedNewznab) {
		alt, aerr := c.apiURLForPath(guidParams, id, true)
		if aerr != nil {
			return nil, true, aerr
		}
		if err := c.reserve(hctx, "grab"); err != nil {
			return nil, true, err
		}
		if body, err = c.get(hctx, alt, c.maxNZB, id); err == nil {
			c.rememberNewznabPath(ctx)
		}
	}
	if err != nil {
		return nil, true, err
	}
	nzb, kind, ok, err := unwrapNZB(body, c.maxNZB)
	if err != nil {
		return nil, true, err
	}
	if !ok {
		// A Newznab error document (bad API key, grab limit) says why.
		if _, perr := parseFeed(body, c.Name()); perr != nil {
			return nil, true, perr
		}
		return nil, true, fmt.Errorf("newznab: response for %q is not an NZB: the indexer sent %s", r.Name, kind)
	}
	body = nzb
	c.keepNZB(ctx, r, body)
	return body, true, nil
}

// backendAnswersCategories reports whether an add-on row's backend
// declared one of the requested categories. Stremio rows never do.
func (c *Client) backendAnswersCategories(ctx context.Context, cfg config.Indexer, cats []int) bool {
	if !cfg.IsAddon() || len(cats) == 0 {
		return false
	}
	b, err := c.backend()
	if err != nil {
		return false
	}
	cb, ok := b.(CategoryBackend)
	return ok && cb.AnswersCategories(ctx, cats)
}

// backend returns the search backend of a built-in site or addon.
func (c *Client) backend() (Backend, error) {
	if c.backends == nil {
		return nil, fmt.Errorf("indexer %q: built-in sites and addons are not available", c.Name())
	}
	return c.backends(c.Config())
}

// backendSearch runs a built-in indexer's search and stamps the results
// the way Newznab results are stamped. incomplete is true when the backend
// returned a partial answer (some lookups failed, were rate-limited or
// stopped by budget); callers must not cache an incomplete answer and the
// caller's fleet search coverage is marked incomplete.
func (c *Client) backendSearch(ctx context.Context, req core.SearchRequest) ([]core.Release, bool, error) {
	b, err := c.backend()
	if err != nil {
		return nil, false, err
	}
	subCtx, subCov := core.WithSearchCoverage(ctx)
	rels, err := c.stampBackend(subCtx, "search", func(ctx context.Context, reserve func(context.Context, string) error) ([]core.Release, error) {
		return b.Search(ctx, req, reserve)
	})
	incomplete := subCov.Incomplete()
	if incomplete {
		core.NoteSearchIncomplete(ctx)
	}
	params := url.Values{}
	if req.Season > 0 || req.Episode > 0 {
		params.Set("season", strconv.Itoa(req.Season))
	}
	if req.Episode > 0 {
		params.Set("ep", strconv.Itoa(req.Episode))
	}
	c.noteQuery(ctx, "source", req.Title, params, len(rels), err)
	return rels, incomplete, err
}

// backendRecent lists a built-in site's latest releases.
func (c *Client) backendRecent(ctx context.Context) ([]core.Release, error) {
	b, err := c.backend()
	if err != nil {
		return nil, err
	}
	rb, ok := b.(RecentBackend)
	if !ok {
		return nil, fmt.Errorf("recent-release feed is not supported by this source")
	}
	return c.stampBackend(ctx, "feed", func(ctx context.Context, reserve func(context.Context, string) error) ([]core.Release, error) {
		return rb.Recent(ctx, reserve)
	})
}

// stampBackend runs a backend request behind the search guard and stamps
// its results the way Newznab results are stamped.
func (c *Client) stampBackend(ctx context.Context, kind string, run func(context.Context, func(context.Context, string) error) ([]core.Release, error)) ([]core.Release, error) {
	started := time.Now()
	admitted := false
	hits := 0
	var refused error
	reserve := func(rctx context.Context, rkind string) error {
		if err := c.reserve(rctx, rkind); err != nil {
			if errors.Is(err, core.ErrBudgetExhausted) && refused == nil {
				refused = err
			}
			return err
		}
		if rkind == "hit" {
			hits++
		}
		return nil
	}
	hostURL := ""
	if c.Config().IsStremio() {
		hostURL = c.Config().URL
	}
	prl := &partialRateLimit{}
	runCtx := context.WithValue(ctx, partialRateLimitKey{}, prl)
	results, err := c.guardSearch(runCtx, hostURL, false, 0, func(ctx context.Context) ([]core.Release, error) {
		admitted = true
		started = time.Now()
		return run(ctx, reserve)
	})
	if err != nil {
		var rl *RateLimitError
		if errors.As(err, &rl) {
			err = c.recordRateLimit(ctx, rl.RetryAfter, err)
		}
	} else if rl := prl.get(); rl != nil {
		_ = c.recordRateLimit(ctx, rl.RetryAfter, rl)
	}
	if err == nil {
		name, id := c.Name(), c.indexerID()
		for i := range results {
			r := &results[i]
			r.Indexer = name
			r.IndexerID = id
			if r.ID == "" {
				r.ID = releaseID(id, r.GUID, r.NZBURL+r.MagnetURL+r.Name)
			}
			if r.Info.Title == "" && r.Name != "" {
				r.Info = releaseparse.Parse(r.Name)
			}
		}
		c.markOnDemand(results)
		c.stampSourceOrder(results)
	}
	if admitted && !(err != nil && (errors.Is(err, context.Canceled) || errors.Is(err, ErrIndexerPaused))) {
		now := c.now()
		dur := time.Since(started)
		age := averageReleaseAge(results, now)
		purpose := usagePurpose(ctx)
		for i := 0; i < hits; i++ {
			success := err == nil || i < hits-1
			empty := success && len(results) == 0 && i == hits-1
			stepDur := dur / time.Duration(hits)
			var stepAge time.Duration
			var stepRes int
			if i == hits-1 && err == nil {
				stepAge = age
				stepRes = len(results)
			}
			c.emit(Event{
				Kind:        kind,
				Purpose:     purpose,
				Success:     success,
				Empty:       empty,
				RateLimited: !success && errors.Is(err, core.ErrBudgetExhausted) && !errors.Is(err, errLocalBudget),
				Results:     stepRes,
				Duration:    stepDur,
				AverageAge:  stepAge,
				At:          now,
			})
		}
		if refused != nil {
			c.emit(Event{
				Kind:            kind,
				Purpose:         purpose,
				BudgetExhausted: true,
				SpeculativeHeld: errors.Is(refused, errSpeculativeShare),
				At:              now,
			})
		} else if hits == 0 && err != nil {
			c.emit(Event{
				Kind:        kind,
				Purpose:     purpose,
				Success:     false,
				RateLimited: errors.Is(err, core.ErrBudgetExhausted),
				Duration:    dur,
				At:          now,
			})
		}
	}
	if err != nil {
		return nil, err
	}
	return results, nil
}

// Test verifies an indexer config for the setup wizard: caps plus one
// search, with human-readable errors. policy is the client identity the
// probe presents, so the probe matches the traffic the indexer will see.
func Test(ctx context.Context, cfg config.Indexer, policy config.ClientIdentity) error {
	return TestWithOptions(ctx, cfg, Options{ClientIdentity: policy})
}

// TestWithOptions is Test with injectable transport/clock (tests).
func TestWithOptions(ctx context.Context, cfg config.Indexer, opts Options) error {
	if cfg.URL == "" {
		return fmt.Errorf("indexer %q: URL is empty", cfg.Name)
	}
	// Test always runs, even for a not-yet-enabled config.
	testCfg := cfg
	testCfg.Enabled = true
	c := New(testCfg, opts.testDB, opts)
	name := cfg.Name
	if name == "" {
		name = c.Name()
	}
	if _, err := c.fetchCaps(ctx); err != nil {
		return humanError(name, err)
	}
	params := url.Values{"t": {"search"}, "q": {"test"}, "limit": {"1"}, "extended": {"1"}}
	if _, err := c.doSearch(ctx, params, c.clientIdentity(), config.IdentityMediaAny); err != nil {
		return humanError(name, err)
	}
	return nil
}

// TestCaps runs a single t=caps request against cfg using the identity
// resolved from policy, returning the resolved identity and any error.
func TestCaps(ctx context.Context, cfg config.Indexer, policy config.ClientIdentity) (config.Identity, error) {
	return TestCapsWithOptions(ctx, cfg, Options{ClientIdentity: policy})
}

// TestCapsWithOptions is TestCaps with injectable options (usage tracking/tests).
func TestCapsWithOptions(ctx context.Context, cfg config.Indexer, opts Options) (config.Identity, error) {
	if cfg.URL == "" {
		return config.DefaultIdentity(), fmt.Errorf("indexer %q: URL is empty", cfg.Name)
	}
	testCfg := cfg
	testCfg.Enabled = true
	c := New(testCfg, opts.testDB, opts)
	id := c.clientIdentity()
	name := cfg.Name
	if name == "" {
		name = c.Name()
	}
	if _, err := c.fetchCaps(ctx); err != nil {
		return id, humanError(name, err)
	}
	return id, nil
}

func (c *Client) keptNZB(ctx context.Context, r core.Release) ([]byte, error) {
	if c.vault == nil {
		return nil, nil
	}
	read := func(key vault.Key) ([]byte, error) {
		record, err := c.vault.Get(ctx, key)
		if err != nil {
			return nil, err
		}
		if record == nil || len(record.XML) == 0 {
			return nil, vault.ErrPayloadMissing
		}
		_ = c.vault.Touch(ctx, key, true)
		return bytes.Clone(record.XML), nil
	}
	lookup := func(source, id string) ([]byte, error) {
		if id == "" {
			return nil, nil
		}
		key, ok, err := c.vault.ByAlias(ctx, source, id)
		if !ok || err != nil {
			return nil, err
		}
		return read(key)
	}
	baseIDs := nzbReleaseIDs(r)
	ids := baseIDs
	if isPluginNZB(ctx) {
		ids = c.nzbIdentities(r)
	}
	for _, id := range ids {
		if body, err := lookup(c.indexerID(), id); body != nil || err != nil {
			return body, err
		}
	}
	if isPluginNZB(ctx) && strings.HasPrefix(r.ContentID, nzb.ContentIDPrefix) {
		if body, err := read(vault.Key(r.ContentID)); !errors.Is(err, vault.ErrNotFound) {
			return body, err
		}
	}
	if names, ok := c.vault.(vault.NZBIdentityLookup); ok && isPluginNZB(ctx) && r.Name != "" && r.Size > 0 {
		key, found, err := names.ByNZBName(ctx, r.Name, r.Size)
		if err != nil {
			return nil, err
		}
		if found {
			return read(key)
		}
	}
	for _, rid := range baseIDs {
		hash := sha256.Sum256([]byte(c.indexerID() + "\x00" + searchIdentity(c.Config()) + "\x00" + rid))
		id := hex.EncodeToString(hash[:])
		if c.legacyDir != nil {
			if dir := c.legacyDir(); dir != "" {
				if body, err := os.ReadFile(filepath.Join(dir, id+".nzb")); err == nil && len(body) > 0 {
					c.keepNZB(ctx, r, body)
					return body, nil
				}
			}
		}
		if body, err := lookup(vault.LegacyAliasIndexer, id); body != nil || err != nil {
			return body, err
		}
	}
	return nil, nil
}

func (c *Client) keepNZB(ctx context.Context, r core.Release, body []byte) {
	if c.vault == nil {
		return
	}
	r.IndexerID = c.indexerID()
	private := c.publicIndexer == nil || !c.publicIndexer(ctx, c.Config())
	aliases := c.nzbIdentities(r)[1:]
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if _, err := c.vault.Put(writeCtx, vault.PutRequest{Release: r, Source: r.IndexerID, Private: private, NZB: body, Aliases: aliases}); err != nil {
		const nzbStoreFailureTTL = 5 * time.Minute
		now := c.now()
		until := now.Add(nzbStoreFailureTTL)
		c.nzbFlightMu.Lock()
		if c.nzbStoreFailures == nil {
			c.nzbStoreFailures = make(map[string]time.Time)
		}
		if len(c.nzbStoreFailures) >= 4096 {
			maps.DeleteFunc(c.nzbStoreFailures, func(_ string, deadline time.Time) bool { return !now.Before(deadline) })
			if len(c.nzbStoreFailures) >= 4096 {
				clear(c.nzbStoreFailures)
			}
		}
		c.nzbStoreFailures[r.ID] = until
		for _, alias := range aliases {
			c.nzbStoreFailures[alias] = until
		}
		c.nzbFlightMu.Unlock()
		c.log.Warn("could not store indexer release in vault", "indexer", r.IndexerID)
	}
}

// kindLabel names how the indexer is searched, for diagnostics that must
// not carry the owner's indexer name.
func kindLabel(cfg config.Indexer) string {
	switch {
	case cfg.IsStremio():
		return "stremio add-on"
	case cfg.IsAddon():
		return "plugin source"
	case cfg.IsBuiltin():
		return "built-in site"
	case cfg.IsZilean():
		return "zilean"
	case cfg.IsTorrent():
		return "torznab"
	}
	return "newznab"
}

// noteQuery adds one indexer request to the resolution's query log.
func (c *Client) noteQuery(ctx context.Context, mode, query string, params url.Values, results int, err error) {
	q := core.SearchQuery{Indexer: c.Name(), IndexerKind: kindLabel(c.Config()), Mode: mode, Query: query, Results: results}
	if params != nil {
		q.Season, q.Episode = params.Get("season"), params.Get("ep")
		var ids []string
		for _, name := range []string{"imdbid", "tmdbid", "tvdbid", "tvmazeid", "rid", "traktid"} {
			if v := params.Get(name); v != "" {
				ids = append(ids, name+"="+v)
			}
		}
		q.IDs = strings.Join(ids, " ")
	}
	if err != nil {
		q.Err = err.Error()
	}
	core.NoteSearchQuery(ctx, q)
}
