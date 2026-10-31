package tmdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/retryafter"
)

// rateLimiter is a token bucket whose rate can change at run time (a
// cluster divides TMDB's per-key budget among live nodes). Callers reserve
// a token and sleep until it is due, so concurrent waiters are served in
// order without polling.
type rateLimiter struct {
	mu     sync.Mutex
	rate   float64 // tokens per second
	burst  float64
	tokens float64 // may go negative: tokens reserved by waiters
	last   time.Time
	stop   chan struct{}
}

func newRateLimiter(perSec float64, burst int) *rateLimiter {
	perSec, b := limiterParams(perSec, burst)
	return &rateLimiter{rate: perSec, burst: b, tokens: b, last: time.Now(), stop: make(chan struct{})}
}

func limiterParams(perSec float64, burst int) (float64, float64) {
	if burst <= 0 {
		burst = 1
	}
	if perSec <= 0 {
		perSec = 40
	}
	return perSec, float64(burst)
}

// refillLocked adds the tokens earned since the last refill.
func (r *rateLimiter) refillLocked(now time.Time) {
	if el := now.Sub(r.last).Seconds(); el > 0 {
		r.tokens = min(r.burst, r.tokens+el*r.rate)
	}
	r.last = now
}

// setRate changes the rate and burst; tokens earned so far are kept (up
// to the new burst).
func (r *rateLimiter) setRate(perSec float64, burst int) {
	perSec, b := limiterParams(perSec, burst)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refillLocked(time.Now())
	r.rate, r.burst = perSec, b
	r.tokens = min(r.tokens, b)
}

func (r *rateLimiter) current() (float64, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rate, int(r.burst)
}

func (r *rateLimiter) wait(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	r.refillLocked(time.Now())
	r.tokens--
	if r.tokens >= 0 {
		r.mu.Unlock()
		return nil
	}
	delay := time.Duration(-r.tokens / r.rate * float64(time.Second))
	r.mu.Unlock()
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		r.mu.Lock()
		r.tokens++ // give the reservation back
		r.mu.Unlock()
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Close releases the client (the rate limiter has no goroutine; Close is
// kept for callers that own the client).
func (c *Client) Close() {
	if c.lim != nil {
		select {
		case <-c.lim.stop:
		default:
			close(c.lim.stop)
		}
	}
}

func (c *Client) buildURL(path string, q url.Values, cfg clientSettings) string {
	// Work on a copy: the caller's values may be reused (a coalesced
	// waiter retries with them) and must never accumulate the api_key.
	if q == nil {
		q = url.Values{}
	} else {
		q = cloneValues(q)
	}
	if q.Get("language") == "" && cfg.language != "" {
		q.Set("language", cfg.language)
	}
	if !cfg.bearer && cfg.key != "" {
		q.Set("api_key", cfg.key)
	}
	u := c.opts.baseURL + path
	if enc := q.Encode(); enc != "" {
		u += "?" + enc
	}
	return u
}

// getJSON performs a cached, coalesced GET and decodes into out.
func (c *Client) getJSON(ctx context.Context, path string, q url.Values, out any) error {
	body, err := c.get(ctx, path, q)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("tmdb: decode %s: %w", path, err)
	}
	return nil
}

func (c *Client) get(ctx context.Context, path string, q url.Values) ([]byte, error) {
	cfg := c.settings()
	if cfg.key == "" {
		return nil, ErrNoKey
	}
	full := c.buildURL(path, q, cfg)
	cacheKey := strconv.FormatUint(cfg.generation, 10) + ":" + full
	// Cache hit (fresh).
	c.mu.Lock()
	if e, ok := c.cache.get(cacheKey); ok && time.Now().Before(e.expires) {
		body := append([]byte(nil), e.body...)
		c.mu.Unlock()
		return body, nil
	}
	c.mu.Unlock()
	// The request runs on the leading caller's context; if that caller gives
	// up, a waiter retries with its own.
	body, err := c.flights.Do(ctx, cacheKey, func(ctx context.Context) ([]byte, error) {
		c.mu.Lock()
		e, cached := c.cache.get(cacheKey)
		epoch := c.cacheEpoch
		if cached && time.Now().Before(e.expires) {
			// Filled by a request that finished as this one started.
			c.mu.Unlock()
			return e.body, nil
		}
		// Keep the stale body with its ETag: the entry may be evicted while
		// the revalidation is in flight, and a 304 must still have a body to
		// serve. Cached bodies are never mutated, so sharing the slice is safe.
		var etag string
		var staleBody []byte
		if cached {
			etag, staleBody = e.etag, e.body
		}
		c.mu.Unlock()

		body, newETag, notModified, err := c.doWithRetry(ctx, full, etag, cfg)
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		if notModified {
			body = staleBody
		}
		if c.generation == cfg.generation && c.cacheEpoch == epoch {
			if notModified {
				c.cache.put(cacheKey, cacheEntry{body: staleBody, etag: etag, expires: time.Now().Add(c.opts.cacheTTL)}, time.Now())
			} else {
				c.cache.put(cacheKey, cacheEntry{body: append([]byte(nil), body...), etag: newETag, expires: time.Now().Add(c.opts.cacheTTL)}, time.Now())
			}
		}
		return body, nil
	})
	if err != nil {
		return nil, err
	}
	return append([]byte(nil), body...), nil
}

func (c *Client) doWithRetry(ctx context.Context, full, etag string, cfg clientSettings) (body []byte, newETag string, notModified bool, err error) {
	if c.brk.open(time.Now()) {
		return nil, "", false, ErrUnavailable
	}
	body, newETag, notModified, err = c.doAttempts(ctx, full, etag, cfg)
	switch {
	case err != nil && ctx.Err() != nil:
		// The caller gave up; that says nothing about TMDB.
	case err != nil && !errors.Is(err, core.ErrNotFound) && retryableFailure(err):
		var he *HTTPError
		if errors.As(err, &he) {
			// Transport failures were counted per attempt; an exhausted
			// 429/5xx ladder counts once.
			c.brk.failure(time.Now())
		}
	default:
		// A definite answer (data, 304, 404, 401) proves TMDB is reachable.
		c.brk.success()
	}
	return body, newETag, notModified, err
}

// retryableFailure reports transport errors and exhausted 429/5xx answers.
func retryableFailure(err error) bool {
	var he *HTTPError
	if errors.As(err, &he) {
		return he.StatusCode == http.StatusTooManyRequests || he.StatusCode >= 500
	}
	return true
}

func (c *Client) doAttempts(ctx context.Context, full, etag string, cfg clientSettings) (body []byte, newETag string, notModified bool, err error) {
	var lastErr error
	backoff := 500 * time.Millisecond
	for attempt := 0; attempt <= c.opts.maxRetries; attempt++ {
		if err := c.lim.wait(ctx); err != nil {
			return nil, "", false, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
		if err != nil {
			return nil, "", false, err
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", c.opts.userAgent)
		if cfg.bearer {
			req.Header.Set("Authorization", "Bearer "+cfg.key)
		}
		if etag != "" {
			req.Header.Set("If-None-Match", etag)
		}
		resp, err := c.opts.httpClient.Do(req)
		if err != nil {
			lastErr = redactURLError(err)
			if ctx.Err() != nil {
				return nil, "", false, ctx.Err()
			}
			// Every unreachable attempt counts, so an outage opens the
			// breaker within the first request's retries.
			c.brk.failure(time.Now())
		} else {
			b, rerr := io.ReadAll(io.LimitReader(resp.Body, (32<<20)+1))
			resp.Body.Close()
			if rerr != nil {
				lastErr = rerr
			} else if resp.StatusCode == http.StatusNotModified {
				return nil, etag, true, nil
			} else if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				// Validate before caching: a gateway's malformed 200 response
				// must not keep failing metadata reads for the entire cache TTL.
				if len(b) > 32<<20 || !json.Valid(b) {
					return nil, "", false, errors.New("tmdb: invalid or oversized JSON response")
				}
				return b, resp.Header.Get("ETag"), false, nil
			} else {
				he := parseHTTPError(resp.StatusCode, b)
				if resp.StatusCode == http.StatusNotFound {
					return nil, "", false, fmt.Errorf("%w: %v", core.ErrNotFound, he)
				}
				if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
					lastErr = he
					wait := backoff
					if ra, _ := retryafter.Parse(resp.Header.Get("Retry-After"), time.Now(), 0); ra > 0 {
						wait = ra
					}
					if attempt < c.opts.maxRetries && !c.brk.open(time.Now()) {
						if err := sleepCtx(ctx, wait); err != nil {
							return nil, "", false, err
						}
						backoff *= 2
						continue
					}
					return nil, "", false, he
				}
				return nil, "", false, he
			}
		}
		if attempt < c.opts.maxRetries && !c.brk.open(time.Now()) {
			if err := sleepCtx(ctx, backoff); err != nil {
				return nil, "", false, err
			}
			backoff *= 2
			continue
		}
	}
	if lastErr != nil {
		return nil, "", false, lastErr
	}
	return nil, "", false, fmt.Errorf("tmdb: request failed")
}

// redactURLError strips the query (which carries a v3 api_key) from the
// URL net/http embeds in transport errors, so the key never reaches logs or
// API responses.
func redactURLError(err error) error {
	var ue *url.Error
	if !errors.As(err, &ue) {
		return err
	}
	if u, perr := url.Parse(ue.URL); perr == nil {
		if q := u.Query(); q.Has("api_key") {
			q.Set("api_key", "REDACTED")
			u.RawQuery = q.Encode()
		}
		return &url.Error{Op: ue.Op, URL: u.String(), Err: ue.Err}
	}
	return &url.Error{Op: ue.Op, URL: "tmdb", Err: ue.Err}
}

func parseHTTPError(code int, b []byte) *HTTPError {
	var payload struct {
		StatusMessage string `json:"status_message"`
	}
	_ = json.Unmarshal(b, &payload)
	body := strings.TrimSpace(string(b))
	if len(body) > 512 {
		body = body[:512]
	}
	return &HTTPError{StatusCode: code, StatusMsg: payload.StatusMessage, Body: body}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func values(pairs ...string) url.Values {
	q := url.Values{}
	for i := 0; i+1 < len(pairs); i += 2 {
		if pairs[i+1] != "" {
			q.Set(pairs[i], pairs[i+1])
		}
	}
	return q
}
func pageQ(q url.Values, page int) url.Values {
	if q == nil {
		q = url.Values{}
	}
	if page > 0 {
		q.Set("page", strconv.Itoa(page))
	}
	return q
}

// getFreshJSON contacts TMDB and replaces the ordinary cached response.
func (c *Client) getFreshJSON(ctx context.Context, path string, q url.Values, out any) error {
	cfg := c.settings()
	if cfg.key == "" {
		return ErrNoKey
	}
	full := c.buildURL(path, q, cfg)
	body, etag, _, err := c.doWithRetry(ctx, full, "", cfg)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("tmdb: decode %s: %w", path, err)
	}
	c.mu.Lock()
	if c.generation == cfg.generation {
		c.cacheEpoch++
		key := strconv.FormatUint(cfg.generation, 10) + ":" + full
		c.cache.put(key, cacheEntry{body: append([]byte(nil), body...), etag: etag, expires: time.Now().Add(c.opts.cacheTTL)}, time.Now())
	}
	c.mu.Unlock()
	return nil
}
