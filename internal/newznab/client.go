package newznab

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/braedonsaunders/ether/internal/atomicfile"
	"github.com/braedonsaunders/ether/internal/logredact"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/egress"
	"github.com/braedonsaunders/ether/internal/releaseparse"
	"github.com/braedonsaunders/ether/internal/safefetch"
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
	HTTPClient     *http.Client
	Logger         *slog.Logger
	SearchCacheTTL time.Duration // <0 disables cache, 0 = default 6h
	NZBDir         string        // directory for downloaded NZBs; "" keeps them in memory only
	MaxNZBBytes    int64
	Now            func() time.Time // injected clock (tests)
	MaxParallel    int              // Manager fan-out; 0 dispatches every configured indexer
	// ClientIdentity is the owner's policy for the client Ether presents to
	// indexers; the zero value keeps the Ether identity.
	ClientIdentity config.ClientIdentity
	// Backend builds the search backend for an indexer whose Kind is not
	// Newznab (built-in sites, internal/cardigann). Nil disables them.
	Backend func(config.Indexer) (Backend, error)
	// usageDB records a one-off client's requests in the usage breakdown
	// without spending budgets (Manager.TestIndexer).
	usageDB *sql.DB
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
	guarded  *http.Client
	log      *slog.Logger
	cacheTTL time.Duration
	nzbDir   string
	maxNZB   int64
	now      func() time.Time
	backends func(config.Indexer) (Backend, error)

	feedMu   sync.Mutex
	feedMode recentFeedMode
	budgetMu sync.Mutex
	mem      memBudget
	usageDB  *sql.DB // see Options.usageDB
	rate     rateLimitState
	apiLimit apiLimitState

	cacheMu  sync.RWMutex
	memCache map[string]memCacheEntry
	torrents torrentDescriptorCache
	br       breaker

	capsMu     sync.Mutex
	caps       *Caps
	capsAt     time.Time
	capsEP     string    // capsEndpoint the cached caps came from
	capsFailAt time.Time // last failed caps request; searches skip caps for capsRetry

	// nzbSave serializes saveNZB per release id: concurrent FetchNZB calls
	// for the same release must not write/rename the same temp file.
	nzbSaveMu    sync.Mutex
	nzbSaveLocks map[string]*nzbSaveLock

	// nzbFlights coalesces concurrent FetchNZB calls per release id, so
	// racing warm-up, playback and validation spend one grab.
	nzbFlightMu sync.Mutex
	nzbFlights  map[string]*nzbFlight

	cfgMu sync.RWMutex

	// Resolved identities the client presents to this indexer, one per
	// purpose and media, refreshed from the policy on every config reload.
	idMu        sync.RWMutex
	idSearchTV  config.Identity
	idSearchMov config.Identity
	idGrabTV    config.Identity
	idGrabMov   config.Identity
}

// nzbFlight is one shared in-flight NZB fetch. done is closed after body
// and err are set. waiters counts callers that joined it (tests); readers
// counts those still to take the result. Both are guarded by nzbFlightMu.
type nzbFlight struct {
	done       chan struct{}
	body       []byte
	err        error
	background bool // started by a background check (smaller grab share)
	waiters    int
	readers    int
	cancel     context.CancelFunc // stops the fetch once no caller waits
}

// nzbSaveLock is a refcounted per-key mutex (see lockNZB).
type nzbSaveLock struct {
	mu   sync.Mutex
	refs int
}

// lockNZB takes the save lock for key and returns its release func. The
// entry is dropped once the last holder/waiter releases, so the map does
// not grow with the number of releases ever saved.
func (c *Client) lockNZB(key string) func() {
	c.nzbSaveMu.Lock()
	if c.nzbSaveLocks == nil {
		c.nzbSaveLocks = map[string]*nzbSaveLock{}
	}
	l, ok := c.nzbSaveLocks[key]
	if !ok {
		l = &nzbSaveLock{}
		c.nzbSaveLocks[key] = l
	}
	l.refs++
	c.nzbSaveMu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		c.nzbSaveMu.Lock()
		l.refs--
		if l.refs == 0 {
			delete(c.nzbSaveLocks, key)
		}
		c.nzbSaveMu.Unlock()
	}
}

var _ core.Indexer = (*Client)(nil)

// New builds a Client for one indexer. db may be nil (budgets and cache
// then live in memory).
func New(cfg config.Indexer, db *sql.DB, opts Options) *Client {
	c := &Client{cfg: cfg, db: db, usageDB: opts.usageDB, nzbDir: opts.NZBDir, backends: opts.Backend}
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
	c.SetClientIdentity(opts.ClientIdentity)
	return c
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
	if changed {
		// A corrected URL or key deserves a fresh caps attempt, and the
		// old endpoint's caps no longer describe this indexer.
		c.capsMu.Lock()
		c.capsFailAt = time.Time{}
		c.caps, c.capsAt, c.capsEP = nil, time.Time{}, ""
		c.capsMu.Unlock()
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

// SetClientIdentity resolves the owner's policy into the four identities this
// indexer presents, honoring the indexer's own override.
func (c *Client) SetClientIdentity(p config.ClientIdentity) {
	ix := c.Config()
	c.idMu.Lock()
	defer c.idMu.Unlock()
	c.idSearchTV = p.Resolve(ix.ClientIdentity, config.IdentitySearch, true)
	c.idSearchMov = p.Resolve(ix.ClientIdentity, config.IdentitySearch, false)
	c.idGrabTV = p.Resolve(ix.ClientIdentity, config.IdentityGrab, true)
	c.idGrabMov = p.Resolve(ix.ClientIdentity, config.IdentityGrab, false)
}

// clientIdentityFor returns the identity for one purpose and media.
func (c *Client) clientIdentityFor(tv, grab bool) config.Identity {
	c.idMu.RLock()
	defer c.idMu.RUnlock()
	switch {
	case grab && tv:
		return c.idGrabTV
	case grab:
		return c.idGrabMov
	case tv:
		return c.idSearchTV
	}
	return c.idSearchMov
}

// generalIdentity is the identity for requests that are neither a TV nor a
// movie search: caps, the connection test and the recent-release feed. It
// is the TV search choice, or the movie one when only movie searches were
// moved off Ether, so a site that rejects Ether/1.0 still answers them.
func (c *Client) generalIdentity() config.Identity {
	c.idMu.RLock()
	defer c.idMu.RUnlock()
	if isEtherIdentity(c.idSearchTV) && !isEtherIdentity(c.idSearchMov) {
		return c.idSearchMov
	}
	return c.idSearchTV
}

func isEtherIdentity(id config.Identity) bool {
	return id.Preset == "" || id.Preset == config.IdentityEther
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

func (c *Client) apiURL(params url.Values, id config.Identity) (string, error) {
	cfg := c.Config()
	raw := strings.TrimSpace(cfg.URL)
	if raw == "" {
		return "", fmt.Errorf("newznab: indexer %q has no URL", c.Name())
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		base := strings.TrimRight(raw, "/")
		if !strings.HasSuffix(base, "/api") {
			base += "/api"
		}
		q := url.Values{}
		for k, vs := range params {
			q[k] = vs
		}
		if cfg.APIKey != "" && q.Get("apikey") == "" {
			q.Set("apikey", cfg.APIKey)
		}
		return base + "?" + encodeQuery(q, id.Params), nil
	}
	u.Path = strings.TrimRight(u.Path, "/")
	if !strings.HasSuffix(u.Path, "/api") {
		u.Path += "/api"
	}
	q := u.Query()
	q.Del("t")
	for k, vs := range params {
		q[k] = vs
	}
	if cfg.APIKey != "" && q.Get("apikey") == "" {
		q.Set("apikey", cfg.APIKey)
	}
	u.RawQuery = encodeQuery(q, id.Params)
	return u.String(), nil
}

// applyIdentity writes the resolved client identity on one request. The
// identity's headers include the owner's custom values, which are credentials:
// they go only to the configured indexer origin, so a feed-supplied download
// URL on another host gets the user agent alone.
func (c *Client) applyIdentity(req *http.Request, id config.Identity) {
	req.Header.Set("User-Agent", id.UserAgent)
	if !safefetch.SameOrigin(c.Config().URL, req.URL.String()) {
		return
	}
	for k, v := range id.Headers {
		req.Header.Set(k, v)
	}
}

// send performs req with the identity's headers confined to the configured
// indexer origin: net/http copies non-sensitive headers onto a redirect,
// including one that leaves the origin.
func (c *Client) send(req *http.Request, id config.Identity) (*http.Response, error) {
	if len(id.Headers) == 0 {
		return c.guarded.Do(req)
	}
	hc := *c.guarded
	inner := hc.CheckRedirect
	hc.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		c.dropForeignIdentityHeaders(next, id)
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

// dropForeignIdentityHeaders clears the identity's headers from a redirect
// that leaves the configured indexer origin.
func (c *Client) dropForeignIdentityHeaders(next *http.Request, id config.Identity) {
	if safefetch.SameOrigin(c.Config().URL, next.URL.String()) {
		return
	}
	for k := range id.Headers {
		next.Header.Del(k)
	}
}

// encodeQuery renders q in the identity's parameter order, then the remaining
// keys alphabetically. No order keeps Go's alphabetical encoding.
func encodeQuery(q url.Values, order []string) string {
	if len(order) == 0 {
		return q.Encode()
	}
	var b strings.Builder
	add := func(k, v string) {
		if b.Len() > 0 {
			b.WriteByte('&')
		}
		b.WriteString(url.QueryEscape(k))
		b.WriteByte('=')
		b.WriteString(url.QueryEscape(v))
	}
	listed := make(map[string]bool, len(order))
	for _, k := range order {
		listed[k] = true
		for _, v := range q[k] {
			add(k, v)
		}
	}
	rest := make([]string, 0, len(q))
	for k := range q {
		if !listed[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	for _, k := range rest {
		for _, v := range q[k] {
			add(k, v)
		}
	}
	return b.String()
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
	req, err := http.NewRequestWithContext(c.trustedCtx(ctx, rawURL), http.MethodGet, rawURL, nil)
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
		return nil, c.recordRateLimit(ctx, resp.Header.Get("Retry-After"), apiError(429, "too many requests"))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("newznab: response exceeds %d bytes", limit)
	}
	c.noteAPILimit(ctx, body)
	// Newznab quota errors can arrive with HTTP 200, including NZB grabs.
	// Detect these here so caps, searches and downloads share the same backoff.
	if bytes.Contains(bytes.ToLower(body[:min(len(body), 4096)]), []byte("error")) {
		if _, perr := parseFeed(body, c.Name()); perr != nil {
			var ae *APIError
			if errors.As(perr, &ae) {
				if ae.Budget {
					return nil, c.recordRateLimit(ctx, resp.Header.Get("Retry-After"), perr)
				}
				return nil, perr
			}
		}
	}
	if resp.StatusCode != http.StatusOK {
		// Indexers often still return a Newznab error document.
		if _, perr := parseFeed(body, c.Name()); perr != nil {
			var ae *APIError
			if errors.As(perr, &ae) {
				return nil, perr
			}
		}
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
	c.capsMu.Lock()
	if c.caps != nil && c.capsEP == ep && c.now().Sub(c.capsAt) < 24*time.Hour {
		defer c.capsMu.Unlock()
		return c.caps, nil
	}
	if !c.capsFailAt.IsZero() && c.now().Sub(c.capsFailAt) < capsRetry {
		c.capsMu.Unlock()
		return nil, errCapsUnavailable
	}
	c.capsMu.Unlock()
	if caps, ok := c.capsFromDB(ctx, ep); ok {
		c.capsMu.Lock()
		if c.capsEndpoint() == ep {
			c.caps, c.capsAt, c.capsEP = caps, c.now(), ep
		}
		c.capsMu.Unlock()
		return caps, nil
	}
	caps, err := c.fetchCaps(ctx)
	c.capsMu.Lock()
	current := c.capsEndpoint() == ep // the config did not change meanwhile
	if err != nil {
		if current && ctx.Err() == nil && !errors.Is(err, core.ErrBudgetExhausted) {
			c.capsFailAt = c.now()
		}
		c.capsMu.Unlock()
		return nil, err
	}
	if current {
		c.caps, c.capsAt, c.capsEP, c.capsFailAt = caps, c.now(), ep, time.Time{}
	}
	c.capsMu.Unlock()
	if current {
		c.capsToDB(ctx, caps, ep)
	}
	return caps, nil
}

func (c *Client) fetchCaps(ctx context.Context) (*Caps, error) {
	id := c.generalIdentity()
	u, err := c.apiURL(url.Values{"t": {"caps"}}, id)
	if err != nil {
		return nil, err
	}
	if err := c.reserve(capsPurpose(ctx), "hit"); err != nil {
		return nil, err
	}
	body, err := c.get(ctx, u, 5<<20, id)
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
	return caps, nil
}

func cacheKey(c *Client, req core.SearchRequest) string {
	h := sha1.New()
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
	fmt.Fprint(h, "release-parse-v5|")
	if c.Config().IsBuiltin() && (req.Type == core.TypeEpisode || req.Type == core.TypeShow || req.Season > 0) {
		// Built-in TV keywords no longer require the series premiere year.
		// Do not reuse persisted empty answers from that narrower search.
		fmt.Fprint(h, "tv-keywords-v2|")
	}
	// The indexer's search identity: an edited URL, key, category list or
	// built-in site setting must not be answered from the old config's
	// results (in memory, or persisted for a fresh client).
	fmt.Fprintf(h, "%s|", searchIdentity(c.Config()))
	// The client identity decides headers and parameter order, and an indexer
	// may answer a blocked or decoy persona differently: a cached answer
	// fetched as another identity must not serve the one switched to. The
	// whole identity is hashed, so editing a custom user agent or header
	// counts as a change.
	fmt.Fprintf(h, "client-id-v1:%v|", c.clientIdentityFor(isTVRequest(req), false))
	fmt.Fprintf(h, "%s|%s|%d|%s|%d|%s|%d|%d|%d",
		c.indexerID(), req.Type, req.IDs.TMDB, req.IDs.IMDB, req.IDs.TVDB,
		strings.ToLower(strings.TrimSpace(req.Title)), req.Year, req.Season, req.Episode)
	if orig := strings.ToLower(strings.TrimSpace(req.OriginalTitle)); orig != "" && orig != strings.ToLower(strings.TrimSpace(req.Title)) {
		fmt.Fprintf(h, "|%s", orig)
	}
	// Anime requests search more (absolute numbers, kitsu ids); keys of
	// every other request stay as they were.
	if req.Anime {
		fmt.Fprintf(h, "|anime:%d", req.AbsoluteEpisode)
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

// queryTitles returns the distinct search titles to try for req: Title
// first, then OriginalTitle (when distinct), then structural aliases.
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
	add(req.Title)
	if req.OriginalTitle != "" && !releaseparse.TitlesMatch(req.OriginalTitle, req.Title) {
		add(req.OriginalTitle)
	}
	for _, a := range releaseparse.TitleAliases(req.Title) {
		add(a)
	}
	if req.OriginalTitle != "" {
		for _, a := range releaseparse.TitleAliases(req.OriginalTitle) {
			add(a)
		}
	}
	return out
}

// isTVRequest reports whether a search asks about a show rather than a movie.
func isTVRequest(req core.SearchRequest) bool {
	return req.Type == core.TypeEpisode || req.Season > 0 || req.Type == core.TypeShow
}

// releaseIsTV reports the media type of a release being grabbed. A feed URL
// carries no media type, and only a movie category is evidence of one, so an
// unknown category takes the TV identity.
func releaseIsTV(r core.Release) bool {
	return !(r.Category >= 2000 && r.Category < 3000)
}

// searchAttempts builds the ordered fallback queries for a request.
func (c *Client) searchAttempts(req core.SearchRequest, caps *Caps, id config.Identity) []url.Values {
	cfg := c.Config()
	isTV := isTVRequest(req)
	base := func() url.Values {
		v := url.Values{"extended": {"1"}, "limit": {strconv.Itoa(defaultSearchLimit)}}
		if id.Offset0 {
			v.Set("offset", "0")
		}
		if isTV {
			cats := cfg.TVCategories
			if len(cats) == 0 {
				cats = []int{5000}
			}
			v.Set("cat", joinCats(cats))
		} else {
			cats := cfg.MovieCategories
			if len(cats) == 0 {
				cats = []int{2000}
			}
			v.Set("cat", joinCats(cats))
		}
		return v
	}
	imdb := strings.TrimPrefix(req.IDs.IMDB, "tt")
	titles := queryTitles(req)
	var attempts []url.Values
	if isTV {
		mk := func(idKey, idVal string) url.Values {
			v := base()
			v.Set("t", "tvsearch")
			if idKey != "" {
				v.Set(idKey, idVal)
			}
			if req.Season > 0 {
				v.Set("season", strconv.Itoa(req.Season))
			}
			if req.Episode > 0 {
				v.Set("ep", strconv.Itoa(req.Episode))
			}
			return v
		}
		if req.IDs.TVDB > 0 && (caps == nil || (!caps.TVAvail || hasParam(caps.TVParams, "tvdbid"))) {
			attempts = append(attempts, mk("tvdbid", strconv.FormatInt(req.IDs.TVDB, 10)))
		}
		if req.IDs.TMDB > 0 && (caps == nil || (!caps.TVAvail || hasParam(caps.TVParams, "tmdbid"))) {
			attempts = append(attempts, mk("tmdbid", strconv.FormatInt(req.IDs.TMDB, 10)))
		}
		if imdb != "" && (caps == nil || (!caps.TVAvail || hasParam(caps.TVParams, "imdbid"))) {
			attempts = append(attempts, mk("imdbid", imdb))
		}
		for _, title := range titles {
			q := title
			switch {
			case req.Season > 0 && req.Episode > 0:
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
		// Torznab season-pack and complete-series fallback: older TV shows
		// on torrent indexers often exist only as full-season or complete
		// packs, which an episode-specific query (ep=N / SxxExx) misses.
		if cfg.IsTorrent() && req.Season > 0 {
			mkSeason := func(idKey, idVal string) url.Values {
				v := base()
				v.Set("t", "tvsearch")
				if idKey != "" {
					v.Set(idKey, idVal)
				}
				v.Set("season", strconv.Itoa(req.Season))
				return v
			}
			if req.Episode > 0 {
				if req.IDs.TVDB > 0 && (caps == nil || (!caps.TVAvail || hasParam(caps.TVParams, "tvdbid"))) {
					attempts = append(attempts, mkSeason("tvdbid", strconv.FormatInt(req.IDs.TVDB, 10)))
				}
				if req.IDs.TMDB > 0 && (caps == nil || (!caps.TVAvail || hasParam(caps.TVParams, "tmdbid"))) {
					attempts = append(attempts, mkSeason("tmdbid", strconv.FormatInt(req.IDs.TMDB, 10)))
				}
				if imdb != "" && (caps == nil || (!caps.TVAvail || hasParam(caps.TVParams, "imdbid"))) {
					attempts = append(attempts, mkSeason("imdbid", imdb))
				}
				for _, title := range titles {
					q := fmt.Sprintf("%s S%02d", title, req.Season)
					v := mkSeason("", "")
					v.Set("q", q)
					attempts = append(attempts, v)
					v2 := base()
					v2.Set("t", "search")
					v2.Set("q", q)
					attempts = append(attempts, v2)
				}
			}
			for _, title := range titles {
				v := base()
				v.Set("t", "search")
				v.Set("q", fmt.Sprintf("%s Season %d", title, req.Season))
				attempts = append(attempts, v)
				vc := base()
				vc.Set("t", "search")
				vc.Set("q", title+" Complete")
				attempts = append(attempts, vc)
			}
		}
		return attempts
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
	return attempts
}

func (c *Client) doSearch(ctx context.Context, params url.Values, id config.Identity) ([]core.Release, error) {
	if err := c.searchPaused(); err != nil {
		return nil, err
	}
	u, err := c.apiURL(params, id)
	if err != nil {
		return nil, err
	}
	results, err := c.guardSearch(ctx, true, defaultHTTPTimeout, func(ctx context.Context) ([]core.Release, error) {
		// The hit is reserved once the breaker has admitted the request,
		// right before it is sent: the budget and the usage breakdown count
		// HTTP requests the indexer actually receives, as Prowlarr does.
		if err := c.reserve(ctx, "hit"); err != nil {
			return nil, err
		}
		if emit, ok := ctx.Value(searchArrivalKey{}).(func([]core.Release)); ok {
			return c.getSearchStream(ctx, u, emit, id)
		}
		body, err := c.get(ctx, u, 20<<20, id)
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
	return c.feedReleases(results), err
}

// Search implements core.Indexer.
// primaryMissKey suffixes the cache key of a primary query that found
// nothing for speculative work, which never asks the fallbacks.
const primaryMissKey = "|primary-miss"

func (c *Client) Search(ctx context.Context, req core.SearchRequest) ([]core.Release, error) {
	if cfg := c.Config(); !cfg.Enabled {
		return nil, fmt.Errorf("indexer %q is disabled", c.Name())
	}
	key := cacheKey(c, req)
	ctx = withCacheAge(ctx, req.Released)
	if cached, ok := c.cacheGet(ctx, key); ok {
		return cached, nil
	}
	speculative := core.SpeculativeIndexerWork(ctx) || core.IsBackgroundCheck(ctx)
	if speculative {
		// A primary query that already missed for speculative work.
		if cached, ok := c.cacheGet(ctx, key+primaryMissKey); ok {
			return cached, nil
		}
	}
	// Exhausted limits prohibit new requests, not reuse of already fetched
	// results and NZBs. The manager must reach this cache before this check.
	if c.Exhausted(ctx) {
		return nil, fmt.Errorf("indexer %q: %w", c.Name(), core.ErrBudgetExhausted)
	}
	if err := c.searchPaused(); err != nil {
		return nil, err
	}
	if c.Config().UsesBackend() {
		results, err := c.backendSearch(ctx, req)
		if err != nil {
			return nil, err
		}
		c.cachePut(ctx, key, results)
		return results, nil
	}
	id := c.clientIdentityFor(isTVRequest(req), false)
	caps, _ := c.Caps(ctx) // best effort; failures just mean no param gating
	attempts := c.searchAttempts(req, caps, id)
	if len(attempts) == 0 {
		return nil, fmt.Errorf("newznab: search request has no usable identifiers")
	}
	var firstErr error
	results, err := c.doSearch(ctx, attempts[0], id)
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
	if len(rest) == 0 {
		if firstErr != nil {
			return nil, firstErr
		}
		c.cachePut(ctx, key, nil)
		return nil, nil
	}
	// When the primary attempt misses on an uncapped indexer, fan out the
	// fallback queries (alternate IDs, keyword search, OriginalTitle/aliases,
	// season packs) in bounded parallel bursts so obscure titles resolve in
	// a single round-trip instead of N serial timeouts. An indexer with a
	// personal cap tries them one at a time and stops at the first hit.
	if c.Config().DailyLimit == 0 && firstErr == nil {
		const burstWidth = 4
		for base := 0; base < len(rest); base += burstWidth {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			end := min(base+burstWidth, len(rest))
			batch := rest[base:end]
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
					slots[i].res, slots[i].err = c.doSearch(ctx, p, id)
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
			results, err := c.doSearch(ctx, params, id)
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
				c.cachePut(ctx, key, results)
				return results, nil
			}
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
func (c *Client) rescueAttempts(req core.SearchRequest, caps *Caps, id config.Identity) []url.Values {
	cfg := c.Config()
	isTV := isTVRequest(req)
	base := func() url.Values {
		v := url.Values{"extended": {"1"}, "limit": {strconv.Itoa(defaultSearchLimit)}}
		if id.Offset0 {
			v.Set("offset", "0")
		}
		if isTV {
			cats := cfg.TVCategories
			if len(cats) == 0 {
				cats = []int{5000}
			}
			v.Set("cat", joinCats(cats))
		} else {
			cats := cfg.MovieCategories
			if len(cats) == 0 {
				cats = []int{2000}
			}
			v.Set("cat", joinCats(cats))
		}
		return v
	}
	primary := c.searchAttempts(req, caps, id)
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
	if isTV {
		for _, title := range titles {
			if req.Season > 0 && req.Episode > 0 {
				// Season pack queries (S01 / Season 1 / Complete).
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
	for _, title := range titles {
		v := base()
		v.Set("t", "search")
		v.Set("q", title)
		add(v)
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
	key := cacheKey(c, req) + "|rescue"
	ctx = withCacheAge(ctx, req.Released)
	if cached, ok := c.cacheGet(ctx, key); ok {
		return cached, nil
	}
	if c.Exhausted(ctx) {
		return nil, fmt.Errorf("indexer %q: %w", c.Name(), core.ErrBudgetExhausted)
	}
	if err := c.searchPaused(); err != nil {
		return nil, err
	}
	if cfg := c.Config(); cfg.UsesBackend() {
		// Built-in sites and addons: if Episode > 0, rescue via season-level
		// search; built-in sites also via OriginalTitle/aliases (an addon
		// searches by identifier, so another title asks the same question).
		var merged []core.Release
		seen := map[string]bool{}
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
		if !cfg.IsStremio() && !cfg.IsAddon() && req.OriginalTitle != "" && !releaseparse.TitlesMatch(req.OriginalTitle, req.Title) {
			alt := req
			alt.Title, alt.OriginalTitle = req.OriginalTitle, ""
			if rs, err := c.backendSearch(ctx, alt); err == nil {
				appendUnique(rs)
			}
		}
		if req.Season > 0 && req.Episode > 0 {
			packReq := req
			packReq.Episode = 0
			if rs, err := c.backendSearch(ctx, packReq); err == nil {
				appendUnique(rs)
			}
		}
		c.cachePut(ctx, key, merged)
		return merged, nil
	}
	id := c.clientIdentityFor(isTVRequest(req), false)
	caps, _ := c.Caps(ctx)
	attempts := c.rescueAttempts(req, caps, id)
	if len(attempts) == 0 {
		return nil, nil
	}
	if len(attempts) > 6 {
		attempts = attempts[:6]
	}
	var merged []core.Release
	seen := map[string]bool{}
	var firstErr error
	// As for Search: a capped indexer's rescue goes one query at a time.
	if c.Config().DailyLimit == 0 {
		type slot struct {
			res []core.Release
			err error
		}
		slots := make([]slot, len(attempts))
		var wg sync.WaitGroup
		for i, p := range attempts {
			wg.Add(1)
			go func(i int, p url.Values) {
				defer wg.Done()
				slots[i].res, slots[i].err = c.doSearch(ctx, p, id)
			}(i, p)
		}
		wg.Wait()
		for _, s := range slots {
			if s.err != nil {
				if firstErr == nil {
					firstErr = s.err
				}
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
	} else {
		for _, p := range attempts {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			res, err := c.doSearch(ctx, p, id)
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
	c.cachePut(ctx, key, merged)
	return merged, nil
}

// nzbDiskPath returns the on-disk NZB path for a release id, or "".
func (c *Client) nzbDiskPath(releaseID string) string {
	if c.nzbDir == "" || releaseID == "" {
		return ""
	}
	// Opaque/backend IDs and persisted releases can collide between clients.
	// A different configured origin must never inherit another client's bytes.
	key := c.indexerID() + "\x00" + searchIdentity(c.Config()) + "\x00" + releaseID
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(c.nzbDir, hex.EncodeToString(sum[:])+".nzb")
}

// FetchNZB implements core.Indexer. Every call returns a slice of its own,
// which the caller may keep or modify.
func (c *Client) FetchNZB(ctx context.Context, r core.Release) ([]byte, error) {
	body, _, err := c.fetchNZBReporting(ctx, r)
	return body, err
}

// fetchNZBReporting is FetchNZB, also reporting whether this call asked
// the indexer: false for an NZB read from the disk cache or one taken from
// another caller's download. Only the indexer's own downloads are grabs.
func (c *Client) fetchNZBReporting(ctx context.Context, r core.Release) ([]byte, bool, error) {
	if r.IndexerID != "" && r.IndexerID != c.indexerID() {
		return nil, false, fmt.Errorf("newznab: release belongs to another originating indexer")
	}
	if r.Protocol == core.ProtocolTorrent {
		return nil, false, fmt.Errorf("newznab: %q is a torrent release; it streams through a debrid provider", r.Name)
	}
	if path := c.nzbDiskPath(r.ID); path != "" {
		if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
			c.touchNZB(path)
			return b, false, nil
		}
	}
	if r.ID == "" {
		body, err := c.fetchNZB(ctx, r)
		return body, true, err
	}
	for {
		f, leader := c.joinNZBFlight(ctx, r)
		select {
		case <-f.done:
		case <-ctx.Done():
			// The shared fetch carries on for the other callers; once
			// none is left it is cancelled (a losing health check must
			// not finish a grab nobody will read).
			c.leaveNZB(r.ID, f)
			return nil, leader, ctx.Err()
		}
		body := c.takeNZB(f)
		// A background check runs on a reduced grab share; a foreground
		// caller must not inherit its budget refusal.
		if !leader && f.background && !core.IsBackgroundCheck(ctx) && errors.Is(f.err, core.ErrBudgetExhausted) {
			continue
		}
		if f.err != nil {
			return nil, leader, f.err
		}
		return body, leader, nil
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
	if f.readers > 0 {
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

// joinNZBFlight attaches to the in-flight fetch for r.ID, or starts one.
// The shared fetch keeps ctx's values (budget class, trust) but not its
// cancellation: it ends when the last caller leaves (leaveNZB), and is
// bounded by nzbFlightTimeout. The flight is forgotten as soon as it ends,
// so errors are not cached past it.
func (c *Client) joinNZBFlight(ctx context.Context, r core.Release) (*nzbFlight, bool) {
	c.nzbFlightMu.Lock()
	defer c.nzbFlightMu.Unlock()
	if f, ok := c.nzbFlights[r.ID]; ok {
		f.waiters++
		f.readers++
		return f, false
	}
	if c.nzbFlights == nil {
		c.nzbFlights = map[string]*nzbFlight{}
	}
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), nzbFlightTimeout)
	f := &nzbFlight{done: make(chan struct{}), background: core.IsBackgroundCheck(ctx), waiters: 1, readers: 1, cancel: cancel}
	c.nzbFlights[r.ID] = f
	go func() {
		defer cancel()
		f.body, f.err = c.fetchNZB(fctx, r)
		c.nzbFlightMu.Lock()
		if c.nzbFlights[r.ID] == f {
			delete(c.nzbFlights, r.ID)
		}
		c.nzbFlightMu.Unlock()
		close(f.done)
	}()
	return f, true
}

// fetchNZB downloads r's NZB from the indexer, spending one grab, and
// keeps it on disk.
func (c *Client) fetchNZB(ctx context.Context, r core.Release) ([]byte, error) {
	if c.Config().UsesBackend() {
		b, err := c.backend()
		if err != nil {
			return nil, err
		}
		body, err := b.FetchNZB(ctx, r, c.reserve)
		if err != nil {
			return nil, err
		}
		c.saveNZB(r.ID, body)
		return body, nil
	}
	id := c.clientIdentityFor(releaseIsTV(r), true)
	// r.NZBURL comes from the indexer's feed, so it is untrusted data:
	// c.get dials it through the safefetch guard. Only a URL that stays
	// on the owner-configured indexer origin (relative links resolve
	// there) may reach a private address; absolute feed URLs to any
	// other host, and redirects away from the indexer, must be public.
	rawURL := r.NZBURL
	if rawURL == "" {
		if r.GUID == "" {
			return nil, fmt.Errorf("newznab: release %q has no NZB URL or GUID", r.Name)
		}
		u, err := c.apiURL(url.Values{"t": {"get"}, "id": {r.GUID}}, id)
		if err != nil {
			return nil, err
		}
		rawURL = u
	} else if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
		base, err := url.Parse(strings.TrimRight(c.Config().URL, "/") + "/")
		if err != nil {
			return nil, redactURLError(err)
		}
		ref, err := url.Parse(rawURL)
		if err != nil {
			return nil, redactURLError(err)
		}
		rawURL = base.ResolveReference(ref).String()
	}
	// Reserved right before the request, so a release with no usable URL
	// spends nothing.
	if err := c.reserve(ctx, "grab"); err != nil {
		return nil, err
	}
	body, err := c.get(ctx, rawURL, c.maxNZB, id)
	if err != nil {
		return nil, err
	}
	// Gzipped NZB payload (magic bytes) — Go's transport only undoes
	// Content-Encoding, not a stored .nzb.gz body.
	if len(body) >= 2 && body[0] == 0x1f && body[1] == 0x8b {
		gz, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("newznab: gunzip NZB: %w", err)
		}
		body, err = io.ReadAll(io.LimitReader(gz, c.maxNZB+1))
		gz.Close()
		if err != nil {
			return nil, err
		}
		if int64(len(body)) > c.maxNZB {
			return nil, fmt.Errorf("newznab: NZB exceeds %d bytes", c.maxNZB)
		}
	}
	trimmed := bytes.TrimSpace(body)
	if !bytes.Contains(bytes.ToLower(trimmed[:min(len(trimmed), 2048)]), []byte("<nzb")) {
		if _, perr := parseFeed(body, c.Name()); perr != nil {
			return nil, perr
		}
		return nil, fmt.Errorf("newznab: response for %q is not an NZB", r.Name)
	}
	c.saveNZB(r.ID, body)
	return body, nil
}

// saveNZB keeps a fetched NZB on disk so later plays need no grab. Saves
// for the same release are serialized and each writes its own temp file,
// so concurrent fetches cannot truncate each other's temp or race the
// rename; the rename onto the final path stays atomic.
func (c *Client) saveNZB(releaseID string, body []byte) {
	path := c.nzbDiskPath(releaseID)
	if path == "" {
		return
	}
	unlock := c.lockNZB(path)
	defer unlock()
	if err := os.MkdirAll(c.nzbDir, 0o755); err != nil {
		return
	}
	if err := atomicfile.Write(path, body, 0644); err != nil {
		c.log.Debug("newznab save NZB failed", "indexer", c.Name(), "err", err)
	}
}

// backend returns the search backend of a built-in site or addon.
func (c *Client) backend() (Backend, error) {
	if c.backends == nil {
		return nil, fmt.Errorf("indexer %q: built-in sites and addons are not available", c.Name())
	}
	return c.backends(c.Config())
}

// backendSearch runs a built-in indexer's search and stamps the results
// the way Newznab results are stamped.
func (c *Client) backendSearch(ctx context.Context, req core.SearchRequest) ([]core.Release, error) {
	b, err := c.backend()
	if err != nil {
		return nil, err
	}
	return c.stampBackend(ctx, func(ctx context.Context) ([]core.Release, error) {
		return b.Search(ctx, req, c.reserve)
	})
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
	return c.stampBackend(ctx, func(ctx context.Context) ([]core.Release, error) {
		return rb.Recent(ctx, c.reserve)
	})
}

// stampBackend runs a backend request behind the search guard and stamps
// its results the way Newznab results are stamped.
func (c *Client) stampBackend(ctx context.Context, run func(context.Context) ([]core.Release, error)) ([]core.Release, error) {
	results, err := c.guardSearch(ctx, false, 0, run)
	if err != nil {
		var rl *RateLimitError
		if errors.As(err, &rl) {
			return nil, c.recordRateLimit(ctx, rl.RetryAfter, err)
		}
		return nil, err
	}
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
	c := New(testCfg, nil, opts)
	name := cfg.Name
	if name == "" {
		name = c.Name()
	}
	if _, err := c.Caps(ctx); err != nil {
		return humanError(name, err)
	}
	params := url.Values{"t": {"search"}, "q": {"test"}, "limit": {"1"}, "extended": {"1"}}
	id := c.generalIdentity()
	if id.Offset0 {
		params.Set("offset", "0")
	}
	if _, err := c.doSearch(ctx, params, id); err != nil {
		return humanError(name, err)
	}
	return nil
}

// An NZB on disk is reused for as long as it is kept, so a release is
// grabbed from its indexer once. Unused NZBs leave after nzbKeep, and the
// directory stays under nzbDiskCap (least recently used first).
const (
	nzbKeep    = 60 * 24 * time.Hour
	nzbDiskCap = 1 << 30
)

// touchNZB marks a cached NZB used, at most once a day.
func (c *Client) touchNZB(path string) {
	if info, err := os.Stat(path); err == nil && c.now().Sub(info.ModTime()) > 24*time.Hour {
		now := c.now()
		_ = os.Chtimes(path, now, now)
	}
}

// pruneNZBs bounds the NZB disk cache in dir; it returns the files removed.
func pruneNZBs(dir string, now time.Time) int64 {
	if dir == "" {
		return 0
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	type file struct {
		path string
		at   time.Time
		size int64
	}
	var files []file
	var total, removed int64
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".nzb") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if now.Sub(info.ModTime()) > nzbKeep {
			if os.Remove(path) == nil {
				removed++
			}
			continue
		}
		files = append(files, file{path, info.ModTime(), info.Size()})
		total += info.Size()
	}
	sort.Slice(files, func(i, j int) bool { return files[i].at.Before(files[j].at) })
	for _, f := range files {
		if total <= nzbDiskCap {
			break
		}
		if os.Remove(f.path) == nil {
			removed++
			total -= f.size
		}
	}
	return removed
}
