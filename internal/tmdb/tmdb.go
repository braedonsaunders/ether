// Package tmdb is a TMDB API v3 client with rate limiting, retries,
// request coalescing and an ETag/TTL response cache.
package tmdb

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/egress"
	"github.com/braedonsaunders/ether/internal/flight"
)

// BundledKey is set at build time via
// -ldflags "-X github.com/braedonsaunders/ether/internal/tmdb.BundledKey=...".
// Never commit a real key here.
var BundledKey string

const DefaultBaseURL = "https://api.themoviedb.org/3"
const defaultImageBase = "https://image.tmdb.org/t/p/"

// ErrNoKey is returned when no TMDB key is configured or bundled.
var ErrNoKey = errors.New("tmdb: no API key configured")

// HTTPError is a non-2xx TMDB response.
type HTTPError struct {
	StatusCode int
	StatusMsg  string
	Body       string
}

func (e *HTTPError) Error() string {
	if e.StatusMsg != "" {
		return fmt.Sprintf("tmdb: HTTP %d: %s", e.StatusCode, e.StatusMsg)
	}
	return fmt.Sprintf("tmdb: HTTP %d: %s", e.StatusCode, e.Body)
}
func (e *HTTPError) Is(target error) bool {
	return target == core.ErrNotFound && e.StatusCode == http.StatusNotFound
}

// IsBearerKey reports whether key looks like a v4 read-access token (JWT)
// rather than a 32-char v3 api_key.
func IsBearerKey(key string) bool {
	key = strings.TrimSpace(key)
	return strings.Count(key, ".") >= 2 || len(key) > 64
}

// ResolveKey picks the owner override, else the bundled key.
func ResolveKey(cfg config.Catalog) string {
	if strings.TrimSpace(cfg.TMDBKey) != "" {
		return strings.TrimSpace(cfg.TMDBKey)
	}
	return strings.TrimSpace(BundledKey)
}

type options struct {
	baseURL    string
	httpClient *http.Client
	ratePerSec float64
	burst      int
	cacheTTL   time.Duration
	maxRetries int
	userAgent  string
	// Response cache bounds; zero means the package defaults.
	cacheMaxEntries int
	cacheMaxBytes   int64
}

// Option customises a Client.
type Option func(*options)

func WithBaseURL(u string) Option          { return func(o *options) { o.baseURL = strings.TrimRight(u, "/") } }
func WithHTTPClient(c *http.Client) Option { return func(o *options) { o.httpClient = c } }
func WithRateLimit(perSec float64, burst int) Option {
	return func(o *options) { o.ratePerSec, o.burst = perSec, burst }
}
func WithCacheTTL(d time.Duration) Option { return func(o *options) { o.cacheTTL = d } }
func WithMaxRetries(n int) Option         { return func(o *options) { o.maxRetries = n } }
func WithUserAgent(s string) Option       { return func(o *options) { o.userAgent = s } }

// Client is a TMDB API client. Safe for concurrent use.
type Client struct {
	key      string
	bearer   bool
	language string
	region   string
	log      *slog.Logger
	opts     options

	lim *rateLimiter

	mu         sync.Mutex
	cache      *respCache
	flights    flight.Group[string, []byte]
	generation uint64
	cacheEpoch uint64 // advances when an authoritative fetch updates the cache

	cfgMu sync.RWMutex
	img   *Configuration
}

type cacheEntry struct {
	body    []byte
	etag    string
	expires time.Time
}

// New creates a client with an explicit key (v3 api_key or v4 bearer).
func New(key string, log *slog.Logger, opts ...Option) *Client {
	return newClient(key, "en-US", "US", log, opts...)
}

// NewFromConfig creates a client from catalogue config, falling back to BundledKey.
func NewFromConfig(cfg config.Catalog, log *slog.Logger, opts ...Option) *Client {
	lang, region := cfg.Language, cfg.Region
	if lang == "" {
		lang = "en-US"
	}
	if region == "" {
		region = "US"
	}
	return newClient(ResolveKey(cfg), lang, region, log, opts...)
}

func newClient(key, lang, region string, log *slog.Logger, opts ...Option) *Client {
	o := options{baseURL: DefaultBaseURL, httpClient: egress.Client(egress.CapMetadata, 30*time.Second),
		ratePerSec: 40, burst: 40, cacheTTL: 10 * time.Minute, maxRetries: 3, userAgent: "Ether/0.1 (+https://github.com/braedonsaunders/ether)"}
	for _, fn := range opts {
		fn(&o)
	}
	if log == nil {
		log = slog.Default()
	}
	key = strings.TrimSpace(key)
	return &Client{key: key, bearer: IsBearerKey(key), language: lang, region: region, log: log,
		opts: o, lim: newRateLimiter(o.ratePerSec, o.burst),
		cache: newRespCache(o.cacheMaxEntries, o.cacheMaxBytes)}
}

// MinClusterRate is the floor, in requests per second, of a node's TMDB
// rate when a cluster divides the per-key budget (SetRateShare).
const MinClusterRate = 5

// SetRateShare divides the configured request rate and burst by nodes, the
// number of live cluster nodes sharing the API key, with a floor of
// MinClusterRate requests per second (and burst) so a node of a large
// cluster still makes progress; nodes ≤ 1 restores the configured rate.
// TMDB's limit is per key (or per IP), so N nodes at the full rate would
// together exceed it.
func (c *Client) SetRateShare(nodes int) {
	rate, burst := c.opts.ratePerSec, c.opts.burst
	if nodes > 1 {
		floor := min(float64(MinClusterRate), rate)
		rate = max(rate/float64(nodes), floor)
		burst = max(burst/nodes, min(MinClusterRate, burst), 1)
	}
	c.lim.setRate(rate, burst)
}

// RateLimit reports the current request rate (per second) and burst.
func (c *Client) RateLimit() (perSec float64, burst int) { return c.lim.current() }

// HasKey reports whether a key is available.
func (c *Client) HasKey() bool { return c.settings().key != "" }

// Language returns the configured TMDB language.
func (c *Client) Language() string { return c.settings().language }

// Region returns the configured region.
func (c *Client) Region() string { return c.settings().region }

// clientSettings is one immutable request snapshot, including its cache generation.
type clientSettings struct {
	key, language, region string
	bearer                bool
	generation            uint64
}

func (c *Client) settings() clientSettings {
	c.mu.Lock()
	defer c.mu.Unlock()
	return clientSettings{c.key, c.language, c.region, c.bearer, c.generation}
}

// UpdateConfig changes credentials and locale without replacing the client held
// by catalogue workers. Existing requests finish with their original snapshot;
// they cannot populate the new configuration's cache.
func (c *Client) UpdateConfig(cfg config.Catalog) {
	key, lang, region := ResolveKey(cfg), cfg.Language, cfg.Region
	if lang == "" {
		lang = "en-US"
	}
	if region == "" {
		region = "US"
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.key == key && c.language == lang && c.region == region {
		return
	}
	c.key, c.bearer, c.language, c.region = key, IsBearerKey(key), lang, region
	c.generation++
	c.cache = newRespCache(c.opts.cacheMaxEntries, c.opts.cacheMaxBytes)
}
