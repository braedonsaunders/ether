// Package trakt is the optional per-user Trakt integration: OAuth device
// flow sign-in, scrobbling Ether watch events to Trakt, and pulling Trakt
// history / recommendations / watchlist back in as recommendation sources.
//
// Tokens are stored per user in SQLite (table trakt_tokens). Everything is
// inert unless config.Trakt.Enabled and a client id/secret are set — Trakt
// requires each install to register its own app credentials, so Ether ships
// none.
package trakt

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/braedonsaunders/ether/internal/httpclient"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/egress"
	"github.com/braedonsaunders/ether/internal/retryafter"
	"github.com/braedonsaunders/ether/internal/state"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/textclip"
)

func init() {
	store.Register("trakt", 1, `
CREATE TABLE trakt_tokens (
  user_id INTEGER PRIMARY KEY,
  access_token TEXT NOT NULL,
  refresh_token TEXT NOT NULL,
  expires_at INTEGER NOT NULL DEFAULT 0,
  username TEXT NOT NULL DEFAULT '',
  updated_at INTEGER NOT NULL DEFAULT 0
);`)
	store.Register("trakt", 4, `ALTER TABLE trakt_tokens ADD COLUMN link_id TEXT NOT NULL DEFAULT '';`)
}

const defaultBaseURL = "https://api.trakt.tv"

// PollStatus is the outcome of one device-token poll.
type PollStatus string

const (
	PollPending    PollStatus = "pending"      // user hasn't entered the code yet (400)
	PollSlowDown   PollStatus = "slow_down"    // poll slower (429)
	PollDenied     PollStatus = "denied"       // user denied (418)
	PollExpired    PollStatus = "expired"      // code expired (410)
	PollUsed       PollStatus = "already_used" // code already redeemed (409)
	PollInvalid    PollStatus = "invalid"      // unknown code (404)
	PollAuthorized PollStatus = "authorized"   // tokens stored
)

// DeviceCode is what the setup UI shows the user.
type DeviceCode struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURL string `json:"verification_url"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

// Tokens are a user's stored Trakt credentials.
type Tokens struct {
	UserID       int64
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
	Username     string
	linkID       string // identifies a connection across token rotations
	storedAccess string // database value used to fence refresh writes
}

// Options build a Client.
type Options struct {
	Config     config.Trakt
	DB         *sql.DB
	Catalog    core.Catalog // resolves RatingKeys <-> TMDB/IMDb ids; may be nil for auth-only use
	BaseURL    string       // override for tests
	HTTPClient *http.Client
	Logger     *slog.Logger
	Now        func() time.Time
	// MaxRetryAfter caps how long a 429 Retry-After wait may block (default 5s).
	MaxRetryAfter time.Duration
	// MinPostInterval spaces POST requests (Trakt allows ~1 authed POST per
	// second). Default 1s; negative disables pacing.
	MinPostInterval time.Duration
	// CacheTTL is how long pulled recommendations, watchlist and history are
	// served without a refresh (default 30m). Stale entries are still served
	// while a background refresh runs, so Home never waits on Trakt twice.
	CacheTTL time.Duration
	// QueueSize bounds the event queue; the oldest events are dropped when
	// Trakt is unreachable for long (default 1000).
	QueueSize int
	// NodeID names this node on import-queue rows it claims; a random id is
	// used when empty.
	NodeID string
	// OnTokenExpired is an optional non-blocking callback invoked when Trakt
	// rejects a user's refresh token and disconnects the account.
	OnTokenExpired func(userID int64, username, reason string)
	// Sealer protects the stored tokens (secrets.go); nil stores them as
	// given.
	Sealer config.FieldSealer
}

// ErrRevoked is returned when Trakt rejects a user's refresh token; the
// stored tokens are deleted and the user must reconnect.
var ErrRevoked = errors.New("trakt: authorization revoked; reconnect required")

// ErrNotConnected is returned by authenticated calls for users who have not
// linked Trakt.
var ErrNotConnected error = notConnectedError{}

type notConnectedError struct{}

func (notConnectedError) Error() string { return "trakt: user not connected" }

// NotConnected lets callers recognize the unlinked state without importing
// this package (watchlist would form an import cycle through state).
func (notConnectedError) NotConnected() bool { return true }

type refreshKey struct {
	userID int64
	linkID string
}

// Client talks to the Trakt API for all users.
type Client struct {
	cfg     config.Trakt
	db      *sql.DB
	sealer  config.FieldSealer
	catalog core.Catalog
	base    string
	http    *http.Client
	log     *slog.Logger
	now     func() time.Time
	maxWait time.Duration

	mu         sync.RWMutex // guards cfg
	rfrMu      sync.Mutex
	refreshing map[refreshKey]chan struct{} // one token refresh per connection
	evMu       sync.Mutex
	queue      []state.Event
	qsize      int
	notify     chan struct{}
	done       chan struct{}
	once       sync.Once

	postMu   sync.Mutex // paces POSTs
	lastPost time.Time
	minPost  time.Duration

	cmu             sync.Mutex
	cache           map[string]*cacheEntry
	cacheGeneration uint64 // invalidations fence off older in-flight pulls
	ttl             time.Duration

	scMu      sync.Mutex
	lastScro  map[scrobbleKey]scrobbleState
	stops     map[scrobbleKey]*pendingStop // final stops being sent or awaiting a retry
	retryWake chan struct{}

	devMu   sync.Mutex
	devices map[int64]*deviceFlow

	pollUnit       time.Duration // device poll interval unit (tests shrink it)
	claimID        string        // this node's name on claimed import-queue rows
	onTokenExpired func(userID int64, username, reason string)
}

type cacheEntry struct {
	val        any
	at         time.Time
	refreshing bool
}

// New builds a Client. Call Start to run the event-scrobble worker and
// SubscribeTo to feed it.
func New(opts Options) *Client {
	if opts.BaseURL == "" {
		opts.BaseURL = defaultBaseURL
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = egress.Client(egress.CapMetadata, 30*time.Second)
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.MaxRetryAfter <= 0 {
		opts.MaxRetryAfter = 5 * time.Second
	}
	switch {
	case opts.MinPostInterval == 0:
		opts.MinPostInterval = time.Second
	case opts.MinPostInterval < 0:
		opts.MinPostInterval = 0
	}
	if opts.CacheTTL <= 0 {
		opts.CacheTTL = 30 * time.Minute
	}
	if opts.QueueSize <= 0 {
		opts.QueueSize = 1000
	}
	if opts.NodeID == "" {
		var b [8]byte
		_, _ = rand.Read(b[:])
		opts.NodeID = hex.EncodeToString(b[:])
	}
	return &Client{
		cfg: opts.Config, db: opts.DB, sealer: opts.Sealer, catalog: opts.Catalog, base: strings.TrimSuffix(opts.BaseURL, "/"),
		http: opts.HTTPClient, log: opts.Logger, now: opts.Now, maxWait: opts.MaxRetryAfter,
		notify: make(chan struct{}, 1), done: make(chan struct{}), qsize: opts.QueueSize,
		minPost: opts.MinPostInterval, cache: map[string]*cacheEntry{}, ttl: opts.CacheTTL,
		refreshing: map[refreshKey]chan struct{}{}, retryWake: make(chan struct{}, 1),
		lastScro: map[scrobbleKey]scrobbleState{}, stops: map[scrobbleKey]*pendingStop{}, devices: map[int64]*deviceFlow{},
		pollUnit: time.Second, claimID: opts.NodeID, onTokenExpired: opts.OnTokenExpired,
	}
}

// pace blocks until a POST may be sent under MinPostInterval.
func (c *Client) pace(ctx context.Context) error {
	if c.minPost <= 0 {
		return nil
	}
	for {
		c.postMu.Lock()
		wait := time.Until(c.lastPost.Add(c.minPost))
		if wait <= 0 {
			c.lastPost = time.Now()
			c.postMu.Unlock()
			return ctx.Err()
		}
		c.postMu.Unlock()
		t := time.NewTimer(wait)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		}
	}
}

// UpdateConfig swaps the Trakt settings.
func (c *Client) UpdateConfig(cfg config.Trakt) {
	c.mu.Lock()
	c.cfg = cfg
	c.mu.Unlock()
}

// Enabled reports whether Trakt is configured. Only a client ID is needed:
// Trakt stopped issuing client secrets to new apps on 2026-10-01, and the
// device-code sign-in works without one. Apps that have a secret still send
// it.
func (c *Client) Enabled() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cfg.Enabled && c.cfg.ClientID != ""
}

// withSecret adds client_secret to a token request body when the app has
// one. Apps created since 2026-10-01 have none and must not send it.
func withSecret(body map[string]string, secret string) map[string]string {
	if secret != "" {
		body["client_secret"] = secret
	}
	return body
}

func (c *Client) creds() (id, secret string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cfg.ClientID, c.cfg.ClientSecret
}

// do performs one request, retrying once on 429 after Retry-After (capped).
// token, when non-empty, is sent as a Bearer token. The body is returned
// even on error statuses so callers can map Trakt's status-code protocol.
func (c *Client) do(ctx context.Context, method, path string, payload any, token string) (int, []byte, error) {
	var bodyBytes []byte
	if payload != nil {
		var err error
		if bodyBytes, err = json.Marshal(payload); err != nil {
			return 0, nil, err
		}
	}
	for attempt := 0; attempt < 2; attempt++ {
		if method == http.MethodPost {
			if err := c.pace(ctx); err != nil {
				return 0, nil, err
			}
		}
		var rdr io.Reader
		if bodyBytes != nil {
			rdr = bytes.NewReader(bodyBytes)
		}
		req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
		if err != nil {
			return 0, nil, err
		}
		id, _ := c.creds()
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("trakt-api-version", "2")
		req.Header.Set("trakt-api-key", id)
		req.Header.Set("User-Agent", httpclient.UserAgent())
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return 0, nil, err
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		retryAfter := resp.Header.Get("Retry-After")
		resp.Body.Close()
		if err != nil {
			return resp.StatusCode, nil, err
		}
		if resp.StatusCode == http.StatusTooManyRequests && path != "/oauth/device/token" && attempt == 0 && ctx.Err() == nil {
			wait, ok := retryafter.Parse(retryAfter, c.now(), c.maxWait)
			if !ok {
				wait = c.maxWait
			}
			c.log.Debug("trakt rate limited, retrying", "path", path, "wait", wait)
			t := time.NewTimer(wait)
			select {
			case <-t.C:
				continue
			case <-ctx.Done():
				t.Stop()
				return resp.StatusCode, b, ctx.Err()
			}
		}
		return resp.StatusCode, b, nil
	}
	return http.StatusTooManyRequests, nil, fmt.Errorf("trakt rate limited")
}

// ---- token storage -------------------------------------------------------

func (c *Client) saveTokens(ctx context.Context, t Tokens) error {
	if c.db == nil {
		return fmt.Errorf("trakt: no database configured")
	}
	access, refresh, err := c.sealTokens(t.UserID, t.AccessToken, t.RefreshToken)
	if err != nil {
		return err
	}
	var linkID [16]byte
	if _, err := rand.Read(linkID[:]); err != nil {
		return err
	}
	_, err = c.db.ExecContext(ctx, `INSERT INTO trakt_tokens(user_id, access_token, refresh_token, expires_at, username, updated_at, link_id)
		VALUES(?,?,?,?,?,?,?) ON CONFLICT(user_id) DO UPDATE SET access_token=excluded.access_token,
		  refresh_token=excluded.refresh_token, expires_at=excluded.expires_at, username=excluded.username, updated_at=excluded.updated_at, link_id=excluded.link_id`,
		t.UserID, access, refresh, t.ExpiresAt.Unix(), t.Username, c.now().Unix(), hex.EncodeToString(linkID[:]))
	return err
}

// Tokens returns the stored tokens for a user.
func (c *Client) Tokens(ctx context.Context, userID int64) (Tokens, bool, error) {
	if c.db == nil {
		return Tokens{}, false, fmt.Errorf("trakt: no database configured")
	}
	var t Tokens
	var exp int64
	err := c.db.QueryRowContext(ctx, `SELECT access_token, refresh_token, expires_at, username, link_id FROM trakt_tokens WHERE user_id=?`, userID).
		Scan(&t.storedAccess, &t.RefreshToken, &exp, &t.Username, &t.linkID)
	if errors.Is(err, sql.ErrNoRows) {
		return Tokens{}, false, nil
	}
	if err != nil {
		return Tokens{}, false, err
	}
	if t.AccessToken, t.RefreshToken, _, err = c.openTokens(userID, t.storedAccess, t.RefreshToken); err != nil {
		return Tokens{}, false, err
	}
	t.UserID = userID
	if exp > 0 {
		t.ExpiresAt = time.Unix(exp, 0).UTC()
	}
	return t, true, nil
}

// Connected reports whether a user has linked Trakt.
func (c *Client) Connected(ctx context.Context, userID int64) bool {
	if !c.Enabled() {
		return false
	}
	_, ok, err := c.Tokens(ctx, userID)
	return err == nil && ok
}

// Disconnect removes a user's stored tokens and clears any pending device code.
func (c *Client) Disconnect(ctx context.Context, userID int64) error {
	if c.db == nil {
		return fmt.Errorf("trakt: no database configured")
	}
	c.devMu.Lock()
	delete(c.devices, userID)
	c.devMu.Unlock()
	_, err := c.db.ExecContext(ctx, `DELETE FROM trakt_tokens WHERE user_id=?`, userID)
	c.cancelStops(userID, 0)
	c.invalidateUser(userID)
	return err
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	CreatedAt    int64  `json:"created_at"`
}

func (c *Client) storeTokenResponse(ctx context.Context, userID int64, tr tokenResponse, username string) error {
	if tr.AccessToken == "" {
		return fmt.Errorf("trakt: token response without access_token")
	}
	return c.saveTokens(ctx, Tokens{UserID: userID, AccessToken: tr.AccessToken, RefreshToken: tr.RefreshToken, ExpiresAt: c.tokenExpiry(tr), Username: username})
}

func (c *Client) tokenExpiry(tr tokenResponse) time.Time {
	if tr.CreatedAt > 0 && tr.ExpiresIn > 0 {
		return time.Unix(tr.CreatedAt+tr.ExpiresIn, 0).UTC()
	}
	return c.now().Add(time.Duration(tr.ExpiresIn) * time.Second).UTC()
}

// StartDevice begins the device-code flow for a user.
func (c *Client) StartDevice(ctx context.Context) (*DeviceCode, error) {
	if !c.Enabled() {
		return nil, fmt.Errorf("trakt not configured")
	}
	id, _ := c.creds()
	status, b, err := c.do(ctx, http.MethodPost, "/oauth/device/code", map[string]string{"client_id": id}, "")
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("trakt device code: HTTP %d: %s", status, truncate(string(b), 200))
	}
	var dc DeviceCode
	if err := json.Unmarshal(b, &dc); err != nil {
		return nil, fmt.Errorf("trakt device code decode: %w", err)
	}
	if dc.Interval <= 0 {
		dc.Interval = 5
	}
	return &dc, nil
}

// PollDevice makes one device-token poll. On PollAuthorized the tokens are
// stored for userID. The caller polls every DeviceCode.Interval seconds
// (backing off on PollSlowDown) until a terminal status.
func (c *Client) PollDevice(ctx context.Context, userID int64, deviceCode string) (PollStatus, error) {
	id, secret := c.creds()
	status, b, err := c.do(ctx, http.MethodPost, "/oauth/device/token",
		withSecret(map[string]string{"code": deviceCode, "client_id": id}, secret), "")
	if err != nil {
		return "", err
	}
	switch status {
	case http.StatusOK:
		var tr tokenResponse
		if err := json.Unmarshal(b, &tr); err != nil {
			return "", fmt.Errorf("trakt token decode: %w", err)
		}
		username := c.fetchUsername(ctx, tr.AccessToken)
		if err := c.storeTokenResponse(ctx, userID, tr, username); err != nil {
			return "", err
		}
		c.cancelStops(userID, 0)
		c.invalidateUser(userID)
		c.log.Info("trakt connected", "user", userID, "trakt_user", username)
		return PollAuthorized, nil
	case 400:
		return PollPending, nil
	case 404:
		return PollInvalid, nil
	case 409:
		return PollUsed, nil
	case 410:
		return PollExpired, nil
	case 418:
		return PollDenied, nil
	case 429:
		return PollSlowDown, nil
	default:
		return "", fmt.Errorf("trakt device token: HTTP %d: %s", status, truncate(string(b), 200))
	}
}

// WaitDevice polls the device token endpoint at the code's interval until
// the user authorises (tokens are stored for userID), denies, or the code
// expires. slow_down responses add 5s to the interval (RFC 8628). The
// returned status is terminal unless err is non-nil (ctx cancelled or a
// transport failure).
func (c *Client) WaitDevice(ctx context.Context, userID int64, dc *DeviceCode) (PollStatus, error) {
	if dc == nil || dc.DeviceCode == "" {
		return "", fmt.Errorf("trakt: no device code")
	}
	interval := time.Duration(max(dc.Interval, 1)) * c.pollUnit
	expires := time.Duration(dc.ExpiresIn) * c.pollUnit
	if dc.ExpiresIn <= 0 {
		expires = 600 * c.pollUnit
	}
	deadline := time.Now().Add(expires)
	for {
		t := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			t.Stop()
			return "", ctx.Err()
		case <-t.C:
		}
		st, err := c.PollDevice(ctx, userID, dc.DeviceCode)
		if err != nil {
			return "", err
		}
		switch st {
		case PollPending:
		case PollSlowDown:
			interval += 5 * c.pollUnit
		default:
			return st, nil
		}
		if time.Now().After(deadline) {
			return PollExpired, nil
		}
	}
}

func (c *Client) fetchUsername(ctx context.Context, token string) string {
	status, b, err := c.do(ctx, http.MethodGet, "/users/me", nil, token)
	if err != nil || status != http.StatusOK {
		return ""
	}
	var u struct {
		Username string `json:"username"`
	}
	if json.Unmarshal(b, &u) == nil {
		return u.Username
	}
	return ""
}

// Refresh exchanges a user's refresh token for a fresh token pair.
func (c *Client) Refresh(ctx context.Context, userID int64) error {
	t, err := c.tokensForLink(ctx, userID, nil)
	if err != nil {
		return err
	}
	_, err = c.refreshFrom(ctx, t)
	return err
}

func (c *Client) tokensForLink(ctx context.Context, userID int64, link *string) (Tokens, error) {
	t, ok, err := c.Tokens(ctx, userID)
	if err != nil {
		return Tokens{}, tokenLookupError{err}
	}
	if !ok || (link != nil && t.linkID != *link) {
		return Tokens{}, ErrNotConnected
	}
	return t, nil
}

type tokenLookupError struct{ error }

func (e tokenLookupError) Unwrap() error { return e.error }

// refreshFrom rotates a token for the same link. One refresh per link can
// run on this node. The conditional write also fences disconnects, relinks,
// and rotations on other nodes while the OAuth request is in flight.
func (c *Client) refreshFrom(ctx context.Context, stale Tokens) (Tokens, error) {
	userID := stale.UserID
	key := refreshKey{userID, stale.linkID}
	for {
		c.rfrMu.Lock()
		active := c.refreshing[key]
		if active == nil {
			active = make(chan struct{})
			c.refreshing[key] = active
			c.rfrMu.Unlock()
			defer func() {
				c.rfrMu.Lock()
				delete(c.refreshing, key)
				close(active)
				c.rfrMu.Unlock()
			}()
			break
		}
		c.rfrMu.Unlock()
		select {
		case <-active:
		case <-ctx.Done():
			return Tokens{}, ctx.Err()
		}
	}

	t, err := c.tokensForLink(ctx, userID, &stale.linkID)
	if err != nil || t.AccessToken != stale.AccessToken || t.RefreshToken != stale.RefreshToken {
		return t, err
	}
	if err := ctx.Err(); err != nil {
		return Tokens{}, err
	}
	// Once started, let the exchange commit its single-use token even if
	// the playback operation that needed it is cancelled.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	id, secret := c.creds()
	status, b, err := c.do(ctx, http.MethodPost, "/oauth/token", withSecret(map[string]string{
		"refresh_token": t.RefreshToken, "client_id": id,
		"grant_type": "refresh_token",
	}, secret), "")
	if err != nil && status == 0 {
		return Tokens{}, err
	}
	if status == http.StatusBadRequest || status == http.StatusUnauthorized {
		// Only invalid_grant (or an empty error body) means this viewer's
		// refresh token was revoked; client-level errors like invalid_client
		// must not disconnect every viewer.
		var oauthErr struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(b, &oauthErr)
		if oauthErr.Error != "" && oauthErr.Error != "invalid_grant" {
			return Tokens{}, fmt.Errorf("trakt refresh (%s): %w", oauthErr.Error, traktStatus(status))
		}
		res, err := c.db.ExecContext(ctx, `DELETE FROM trakt_tokens WHERE user_id=? AND link_id=? AND access_token=?`, userID, t.linkID, t.storedAccess)
		if err != nil {
			return Tokens{}, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return Tokens{}, err
		}
		if n == 0 {
			return c.tokensForLink(ctx, userID, &t.linkID)
		}
		c.invalidateUser(userID)
		c.log.Warn("trakt refresh token rejected; disconnected user", "user", userID, "status", status)
		if c.onTokenExpired != nil {
			c.onTokenExpired(userID, t.Username, fmt.Sprintf("HTTP %d", status))
		}
		return Tokens{}, ErrRevoked
	}
	if status != http.StatusOK {
		return Tokens{}, fmt.Errorf("trakt refresh: %w", traktStatus(status))
	}
	if err != nil {
		return Tokens{}, err
	}
	var tr tokenResponse
	if err := json.Unmarshal(b, &tr); err != nil {
		return Tokens{}, fmt.Errorf("trakt refresh decode: %w", err)
	}
	if tr.AccessToken == "" {
		return Tokens{}, fmt.Errorf("trakt: token response without access_token")
	}
	access, refresh, err := c.sealTokens(userID, tr.AccessToken, tr.RefreshToken)
	if err != nil {
		return Tokens{}, err
	}
	_, err = c.db.ExecContext(ctx, `UPDATE trakt_tokens SET access_token=?, refresh_token=?, expires_at=?, updated_at=?
		WHERE user_id=? AND link_id=? AND access_token=?`, access, refresh, c.tokenExpiry(tr).Unix(), c.now().Unix(), userID, t.linkID, t.storedAccess)
	if err != nil {
		return Tokens{}, err
	}
	return c.tokensForLink(ctx, userID, &t.linkID)
}

// authed does a request with the user's token, refreshing once on 401.
func (c *Client) authed(ctx context.Context, userID int64, method, path string, payload any) (int, []byte, error) {
	return c.authedForLink(ctx, userID, nil, method, path, payload)
}

func (c *Client) authedForLink(ctx context.Context, userID int64, link *string, method, path string, payload any) (int, []byte, error) {
	t, err := c.tokensForLink(ctx, userID, link)
	if err != nil {
		return 0, nil, err
	}
	if !t.ExpiresAt.IsZero() && !c.now().Add(time.Minute).Before(t.ExpiresAt) {
		t, err = c.refreshFrom(ctx, t)
		if err != nil {
			return 0, nil, err
		}
	}
	status, b, err := c.do(ctx, method, path, payload, t.AccessToken)
	if status == http.StatusUnauthorized && ctx.Err() == nil {
		t, err = c.refreshFrom(ctx, t)
		if err != nil {
			return 0, nil, err
		}
		return c.do(ctx, method, path, payload, t.AccessToken)
	}
	return status, b, err
}

func truncate(s string, n int) string {
	if len(s) > n {
		return textclip.Bytes(s, n) + "…"
	}
	return s
}

// ---- pull cache ------------------------------------------------------------

func cacheKey(kind string, userID int64, limit int) string {
	return fmt.Sprintf("%s:%d:%d", kind, userID, limit)
}

// invalidateUser drops a user's cached pulls (on connect, disconnect and
// watch events that change what Trakt would return).
func (c *Client) invalidateUser(userID int64) {
	suffix := fmt.Sprintf(":%d:", userID)
	c.cmu.Lock()
	c.cacheGeneration++
	for k := range c.cache {
		if strings.Contains(k, suffix) {
			delete(c.cache, k)
		}
	}
	c.cmu.Unlock()
}

// cachedCall serves a fresh cache entry, or a stale one while refreshing it
// in the background, or fetches synchronously on a miss.
func cachedCall[T any](c *Client, ctx context.Context, key string, fetch func(context.Context) (T, error)) (T, error) {
	return cachedCallTTL(c, ctx, key, c.ttl, fetch)
}

// cachedCallTTL is cachedCall with its own freshness window.
func cachedCallTTL[T any](c *Client, ctx context.Context, key string, ttl time.Duration, fetch func(context.Context) (T, error)) (T, error) {
	c.cmu.Lock()
	generation := c.cacheGeneration
	e, ok := c.cache[key]
	if ok && c.now().Sub(e.at) < ttl {
		c.cmu.Unlock()
		return e.val.(T), nil
	}
	if ok {
		if !e.refreshing {
			e.refreshing = true
			go func() {
				bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
				defer cancel()
				v, err := fetch(bg)
				c.cmu.Lock()
				defer c.cmu.Unlock()
				e.refreshing = false
				if generation != c.cacheGeneration || c.cache[key] != e {
					return
				}
				if err != nil {
					c.log.Debug("trakt background refresh failed", "key", key, "err", err)
					return
				}
				c.cache[key] = &cacheEntry{val: v, at: c.now()}
			}()
		}
		v := e.val.(T)
		c.cmu.Unlock()
		return v, nil
	}
	c.cmu.Unlock()
	v, err := fetch(ctx)
	if err != nil {
		return v, err
	}
	c.cmu.Lock()
	if generation == c.cacheGeneration {
		c.cache[key] = &cacheEntry{val: v, at: c.now()}
	}
	c.cmu.Unlock()
	return v, nil
}
