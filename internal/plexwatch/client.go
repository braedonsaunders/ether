package plexwatch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/egress"
	"github.com/braedonsaunders/ether/internal/plexguid"
	"github.com/braedonsaunders/ether/internal/retryafter"
)

const (
	plexOrigin     = "https://plex.tv"
	serversOrigin  = "https://servers.plex.tv"
	discoverOrigin = "https://discover.provider.plex.tv"
	authApp        = "https://app.plex.tv/auth"
	maxBody        = 1 << 20
	// discoverIdentifier names the Discover provider in action requests.
	discoverIdentifier = "tv.plex.provider.discover"
)

// ErrAccountMismatch means the Plex account that approved a PIN is not the
// viewer's account.
var ErrAccountMismatch = errors.New("plexwatch: plex account does not belong to this viewer")

// ErrRevoked means Plex refused the viewer's stored account token (HTTP 401
// or 403). The viewer must connect the account again.
var ErrRevoked = errors.New("plexwatch: account token rejected")

// StatusError is an HTTP result that is not success. Body is not retained.
type StatusError struct {
	Status     int
	RetryAfter time.Duration
	Op         string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("plexwatch: %s: http %d", e.Op, e.Status)
}

// Is maps definitive credential refusals onto ErrRevoked.
func (e *StatusError) Is(target error) bool {
	return target == ErrRevoked && (e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden)
}

// Client is the allowlisted Plex HTTP API used by the account path: PIN
// sign-in and identity on plex.tv, watch state on discover.provider.plex.tv.
// Every call carries the viewer's own account token in X-Plex-Token.
type Client struct {
	plex     *url.URL
	servers  *url.URL
	discover *url.URL
	clientID string
	http     *http.Client
	now      func() time.Time
}

func newClient(base, clientID string, hc *http.Client, now func() time.Time) (*Client, error) {
	if clientID == "" {
		clientID = plexguid.ClientIdentifier("", "")
	}
	if now == nil {
		now = time.Now
	}
	c := &Client{clientID: clientID, now: now}
	if base == "" {
		var err error
		if c.plex, err = url.Parse(plexOrigin); err != nil {
			return nil, err
		}
		if c.servers, err = url.Parse(serversOrigin); err != nil {
			return nil, err
		}
		if c.discover, err = url.Parse(discoverOrigin); err != nil {
			return nil, err
		}
	} else {
		u, err := url.Parse(base)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return nil, errors.New("plexwatch: invalid test origin")
		}
		c.plex, c.servers, c.discover = u, u, u
	}
	if hc == nil {
		hc = egress.Client(egress.CapDirect, 20*time.Second)
	}
	clone := *hc
	clone.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	c.http = &clone
	return c, nil
}

type pinStart struct {
	ID        int64
	Code      string
	ExpiresAt time.Time
}

func (c *Client) createPIN(ctx context.Context) (pinStart, error) {
	var out struct {
		ID        int64  `json:"id"`
		Code      string `json:"code"`
		ExpiresIn int    `json:"expiresIn"`
	}
	if err := c.call(ctx, http.MethodPost, c.plex, "/api/v2/pins", url.Values{"strong": {"true"}}, "", &out, http.StatusCreated, http.StatusOK); err != nil {
		return pinStart{}, err
	}
	if out.ID == 0 || out.Code == "" {
		return pinStart{}, errors.New("plexwatch: pin response incomplete")
	}
	exp := c.now().Add(15 * time.Minute)
	if out.ExpiresIn > 0 {
		exp = c.now().Add(time.Duration(out.ExpiresIn) * time.Second)
	}
	return pinStart{ID: out.ID, Code: out.Code, ExpiresAt: exp}, nil
}

func (c *Client) authURL(code string) string {
	q := url.Values{}
	q.Set("clientID", c.clientID)
	q.Set("code", code)
	q.Set("context[device][product]", "Ether")
	return authApp + "#?" + q.Encode()
}

func (c *Client) pollPIN(ctx context.Context, id int64, code string) (string, error) {
	var out struct {
		AuthToken *string `json:"authToken"`
	}
	if err := c.call(ctx, http.MethodGet, c.plex, "/api/v2/pins/"+strconv.FormatInt(id, 10), url.Values{"code": {code}}, "", &out, http.StatusOK); err != nil {
		return "", err
	}
	if out.AuthToken == nil {
		return "", nil
	}
	return *out.AuthToken, nil
}

type remoteAccount struct {
	ID         int64
	Username   string
	Restricted bool
}

func (c *Client) account(ctx context.Context, token string) (remoteAccount, error) {
	var out struct {
		ID         int64  `json:"id"`
		Username   string `json:"username"`
		Title      string `json:"title"`
		Restricted bool   `json:"restricted"`
	}
	if err := c.call(ctx, http.MethodGet, c.servers, "/api/v2/user", nil, token, &out, http.StatusOK); err != nil {
		return remoteAccount{}, err
	}
	if out.ID == 0 {
		return remoteAccount{}, errors.New("plexwatch: account response without id")
	}
	name := out.Username
	if name == "" {
		name = out.Title
	}
	return remoteAccount{ID: out.ID, Username: name, Restricted: out.Restricted}, nil
}

// userState is one title's account state on Discover. Watched is
// viewCount>0: unscrobble resets viewCount but keeps lastViewedAt, so
// lastViewedAt alone never means watched.
type userState struct {
	Key      string
	Type     string
	Watched  bool
	ViewedAt int64 // lastViewedAt, unix seconds
	Offset   int64 // viewOffset, milliseconds; 0 when not in progress
	// ViewState is "progress" while in progress and "ignore" once removed
	// from Continue Watching (the next timeline write clears it).
	ViewState  string
	Rating     int // 1–10, 0 when unrated
	ViewedLeaf int // shows only
}

// viewIgnore is the viewState of a title removed from Continue Watching.
const viewIgnore = "ignore"

// removeFromContinueWatching hides key from the account's Continue
// Watching (viewState "ignore"). It keeps viewOffset.
func (c *Client) removeFromContinueWatching(ctx context.Context, token, key string) error {
	return c.action(ctx, token, "/actions/removeFromContinueWatching", key, nil)
}

// consent reads the account's "Sync My Watch State and Ratings" setting.
// Plex does not enforce it on the Discover routes; Ether does.
func (c *Client) consent(ctx context.Context, token string) (bool, error) {
	var out struct {
		Consent *bool `json:"consent"`
	}
	if err := c.call(ctx, http.MethodGet, c.plex, "/api/v2/user/view_state_sync", nil, token, &out, http.StatusOK); err != nil {
		return false, err
	}
	if out.Consent == nil {
		return false, errors.New("plexwatch: view_state_sync response without consent")
	}
	return *out.Consent, nil
}

// timelineResult is what Plex holds after a timeline write. ViewCountKnown
// is false when the answer did not say.
type timelineResult struct {
	ViewCount      int
	ViewCountKnown bool
	Offset         int64
}

// timeline records a stopped playback position (milliseconds) for key.
// Discover takes it only as a GET; Plex never marks a title watched from a
// timeline, so completion is still sent as a scrobble.
func (c *Client) timeline(ctx context.Context, token, key string, offsetMS, durationMS int64) (timelineResult, error) {
	if !validHexKey(key) {
		return timelineResult{}, errors.New("plexwatch: invalid plex metadata key")
	}
	if offsetMS <= 0 || durationMS <= 0 {
		return timelineResult{}, errors.New("plexwatch: timeline needs a position and a duration")
	}
	q := url.Values{
		"ratingKey": {key},
		"key":       {"/library/metadata/" + key},
		"state":     {"stopped"},
		"time":      {strconv.FormatInt(offsetMS, 10)},
		"duration":  {strconv.FormatInt(durationMS, 10)},
	}
	var out struct {
		Container struct {
			ViewCount  *int  `json:"viewCount"`
			ViewOffset int64 `json:"viewOffset"`
		} `json:"MediaContainer"`
	}
	if err := c.call(ctx, http.MethodGet, c.discover, "/timeline", q, token, &out, http.StatusOK); err != nil {
		return timelineResult{}, err
	}
	res := timelineResult{Offset: out.Container.ViewOffset}
	if out.Container.ViewCount != nil {
		res.ViewCount, res.ViewCountKnown = *out.Container.ViewCount, true
	}
	return res, nil
}

// maxStateBatch caps the keys in one batched userState read.
const maxStateBatch = 50

// userStates reads the account state for up to maxStateBatch Plex metadata
// keys in one request. Keys missing from the answer have no known state and
// are absent from the map; callers must not read absence as "unwatched".
func (c *Client) userStates(ctx context.Context, token string, keys []string) (map[string]userState, error) {
	if len(keys) == 0 {
		return map[string]userState{}, nil
	}
	if len(keys) > maxStateBatch {
		return nil, errors.New("plexwatch: userState batch too large")
	}
	for _, k := range keys {
		if !validHexKey(k) {
			return nil, errors.New("plexwatch: invalid plex metadata key")
		}
	}
	var out struct {
		Container struct {
			UserState json.RawMessage `json:"UserState"`
		} `json:"MediaContainer"`
	}
	path := "/library/metadata/" + strings.Join(keys, ",") + "/userState"
	if err := c.call(ctx, http.MethodGet, c.discover, path, nil, token, &out, http.StatusOK); err != nil {
		return nil, err
	}
	type wire struct {
		RatingKey       string  `json:"ratingKey"`
		Type            string  `json:"type"`
		ViewCount       int     `json:"viewCount"`
		ViewOffset      float64 `json:"viewOffset"`
		ViewState       string  `json:"viewState"`
		LastViewedAt    int64   `json:"lastViewedAt"`
		UserRating      float64 `json:"userRating"`
		ViewedLeafCount int     `json:"viewedLeafCount"`
	}
	var items []wire
	raw := bytes.TrimSpace(out.Container.UserState)
	switch {
	case len(raw) == 0 || bytes.Equal(raw, []byte("null")):
	case raw[0] == '[':
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, errors.New("plexwatch: decode userState")
		}
	default:
		var one wire
		if err := json.Unmarshal(raw, &one); err != nil {
			return nil, errors.New("plexwatch: decode userState")
		}
		items = []wire{one}
	}
	want := make(map[string]bool, len(keys))
	for _, k := range keys {
		want[k] = true
	}
	found := make(map[string]userState, len(items))
	for _, it := range items {
		if !want[it.RatingKey] {
			continue
		}
		rating, _ := plexRatingFromRemote(it.UserRating)
		offset := int64(0)
		if it.ViewOffset > 0 && it.ViewOffset < 1<<53 {
			offset = int64(it.ViewOffset)
		}
		found[it.RatingKey] = userState{
			Key: it.RatingKey, Type: it.Type, Watched: it.ViewCount > 0,
			ViewedAt: it.LastViewedAt, Offset: offset, ViewState: it.ViewState, Rating: rating, ViewedLeaf: it.ViewedLeafCount,
		}
	}
	return found, nil
}

// scrobble marks key watched, unscrobble unwatched. Both are idempotent.
func (c *Client) scrobble(ctx context.Context, token, key string, watched bool) error {
	action := "/actions/unscrobble"
	if watched {
		action = "/actions/scrobble"
	}
	return c.action(ctx, token, action, key, nil)
}

// rate sets key's rating on Plex's 1–10 scale; 0 clears it (rating=-1).
func (c *Client) rate(ctx context.Context, token, key string, rating int) error {
	if rating < 0 || rating > 10 {
		return errors.New("plexwatch: rating out of range")
	}
	v := "-1"
	if rating > 0 {
		v = strconv.Itoa(rating)
	}
	return c.action(ctx, token, "/actions/rate", key, url.Values{"rating": {v}})
}

func (c *Client) action(ctx context.Context, token, path, key string, extra url.Values) error {
	if !validHexKey(key) {
		return errors.New("plexwatch: invalid plex metadata key")
	}
	q := url.Values{"identifier": {discoverIdentifier}, "key": {key}}
	for k, v := range extra {
		q[k] = v
	}
	return c.call(ctx, http.MethodPut, c.discover, path, q, token, nil, http.StatusOK, http.StatusNoContent)
}

func (c *Client) call(ctx context.Context, method string, base *url.URL, path string, query url.Values, token string, dest any, ok ...int) error {
	u := *base
	u.Path = strings.TrimRight(base.Path, "/") + path
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, method, u.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Plex-Product", "Ether")
	req.Header.Set("X-Plex-Client-Identifier", c.clientID)
	if token != "" {
		req.Header.Set("X-Plex-Token", token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("plexwatch: %s %s: %w", method, u.Host, redactURL(err))
	}
	defer resp.Body.Close()
	slurp, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return fmt.Errorf("plexwatch: read %s", u.Host)
	}
	good := false
	for _, want := range ok {
		if resp.StatusCode == want {
			good = true
		}
	}
	if !good {
		delay, _ := retryafter.Parse(resp.Header.Get("Retry-After"), c.now(), 6*time.Hour)
		return &StatusError{Status: resp.StatusCode, RetryAfter: delay, Op: method + " " + u.Host}
	}
	if dest != nil && len(bytes.TrimSpace(slurp)) > 0 {
		if err := json.Unmarshal(slurp, dest); err != nil {
			return fmt.Errorf("plexwatch: decode %s", u.Host)
		}
	}
	return nil
}

func redactURL(err error) error {
	var ue *url.Error
	if !errors.As(err, &ue) {
		return err
	}
	safe := "[redacted URL]"
	if u, perr := url.Parse(ue.URL); perr == nil {
		u.RawQuery, u.Fragment, u.User = "", "", nil
		safe = u.String()
	}
	return &url.Error{Op: ue.Op, URL: safe, Err: ue.Err}
}

// validHexKey accepts the hex id inside a verified plex:// GUID.
func validHexKey(k string) bool {
	if len(k) == 0 || len(k) > 64 {
		return false
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// plexRating maps Ether's 0–10 rating onto Plex's whole 1–10 scale (five
// stars in half-star steps). Zero means "no rating" and clears it on Plex.
// Any positive rating rounds to the nearest half star, never below one.
func plexRating(local float64) int {
	if math.IsNaN(local) || local <= 0 {
		return 0
	}
	n := int(math.Round(local))
	if n < 1 {
		n = 1
	}
	if n > 10 {
		n = 10
	}
	return n
}

// plexRatingFromRemote reads Plex's userRating. ok is false for no rating.
func plexRatingFromRemote(v float64) (int, bool) {
	if math.IsNaN(v) || v <= 0 {
		return 0, false
	}
	return plexRating(v), true
}
