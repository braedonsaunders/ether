package plexwatch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/egress"
	"github.com/braedonsaunders/ether/internal/retryafter"
)

const (
	plexOrigin     = "https://plex.tv"
	serversOrigin  = "https://servers.plex.tv"
	scrobbleOrigin = "https://scrobbles.plex.tv"
	authApp        = "https://app.plex.tv/auth"
	maxBody        = 1 << 20
)

// ErrAccountMismatch means the Plex account that approved a PIN is not the
// viewer's account.
var ErrAccountMismatch = errors.New("plexwatch: plex account does not belong to this viewer")

// ErrNotEligible means Plex refused watch-state sync for this account token.
var ErrNotEligible = errors.New("plexwatch: account has not allowed watch-state sync")

// ErrRevoked means the stored account token was rejected.
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

func (e *StatusError) Is(target error) bool {
	switch target {
	case ErrRevoked:
		return e.Status == http.StatusUnauthorized
	case ErrNotEligible:
		return e.Status == http.StatusForbidden
	default:
		return false
	}
}

// Client is the allowlisted Plex HTTP API used by this package.
type Client struct {
	plex     *url.URL
	servers  *url.URL
	scrobble *url.URL
	clientID string
	http     *http.Client
	now      func() time.Time
}

func newClient(base, clientID string, hc *http.Client, now func() time.Time) (*Client, error) {
	if clientID == "" {
		clientID = "ether-plex-watch"
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
		if c.scrobble, err = url.Parse(scrobbleOrigin); err != nil {
			return nil, err
		}
	} else {
		u, err := url.Parse(base)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return nil, errors.New("plexwatch: invalid test origin")
		}
		c.plex, c.servers, c.scrobble = u, u, u
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

func (c *Client) allowed(u *url.URL) bool {
	if u.Scheme != c.plex.Scheme {
		return false
	}
	return u.Host == c.plex.Host || u.Host == c.servers.Host || u.Host == c.scrobble.Host
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
	if _, err := c.call(ctx, http.MethodPost, c.plex, "/api/v2/pins?strong=true", "", nil, &out, http.StatusCreated, http.StatusOK); err != nil {
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
	path := "/api/v2/pins/" + strconv.FormatInt(id, 10) + "?code=" + url.QueryEscape(code)
	if _, err := c.call(ctx, http.MethodGet, c.plex, path, "", nil, &out, http.StatusOK); err != nil {
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
		AuthToken  string `json:"authToken"`
	}
	if _, err := c.call(ctx, http.MethodGet, c.servers, "/api/v2/user", token, nil, &out, http.StatusOK); err != nil {
		return remoteAccount{}, err
	}
	out.AuthToken = ""
	if out.ID == 0 {
		return remoteAccount{}, errors.New("plexwatch: account response without id")
	}
	name := out.Username
	if name == "" {
		name = out.Title
	}
	return remoteAccount{ID: out.ID, Username: name, Restricted: out.Restricted}, nil
}

// eligibility is the read-only view_state_sync check. allowed is true only
// for HTTP 200. HTTP 403 is ErrNotEligible. Other statuses are returned.
func (c *Client) eligibility(ctx context.Context, token string, plexID int64) error {
	path := "/api/v2/server/users/" + strconv.FormatInt(plexID, 10) + "/view_state_sync"
	_, err := c.call(ctx, http.MethodGet, c.plex, path, token, nil, nil, http.StatusOK)
	return err
}

type postItem struct {
	GUID      string `json:"guid"`
	ViewedAt  int64  `json:"viewedAt,omitempty"`
	ViewState string `json:"viewState,omitempty"`
	RatedAt   int64  `json:"ratedAt,omitempty"`
	Rating    *int   `json:"rating,omitempty"`
	IsBulk    bool   `json:"isBulk,omitempty"`
}

func (c *Client) postState(ctx context.Context, token string, plexID int64, item postItem) error {
	body, err := json.Marshal(struct {
		Items []postItem `json:"items"`
		User  int64      `json:"user"`
	}{Items: []postItem{item}, User: plexID})
	if err != nil {
		return err
	}
	_, err = c.call(ctx, http.MethodPost, c.scrobble, "/state", token, body, nil, http.StatusCreated, http.StatusOK)
	return err
}

type remotePage struct {
	Items       []remoteItem
	EndCursor   string
	HasNextPage bool
}

type remoteItem struct {
	GUID      string `json:"guid"`
	ViewedAt  int64  `json:"viewedAt"`
	ViewState string `json:"viewState"`
	RatedAt   int64  `json:"ratedAt"`
	Rating    *int   `json:"rating"`
}

func (c *Client) fetchState(ctx context.Context, token string, plexID int64, cursor string) (remotePage, error) {
	q := url.Values{}
	q.Set("count", "100")
	if cursor != "" {
		q.Set("after", cursor)
	}
	var out struct {
		Items    []remoteItem `json:"items"`
		PageInfo struct {
			EndCursor   string `json:"endCursor"`
			HasNextPage bool   `json:"hasNextPage"`
		} `json:"pageInfo"`
	}
	path := "/state/" + strconv.FormatInt(plexID, 10) + "?" + q.Encode()
	if _, err := c.call(ctx, http.MethodGet, c.scrobble, path, token, nil, &out, http.StatusOK); err != nil {
		return remotePage{}, err
	}
	return remotePage{Items: out.Items, EndCursor: out.PageInfo.EndCursor, HasNextPage: out.PageInfo.HasNextPage}, nil
}

func (c *Client) call(ctx context.Context, method string, base *url.URL, pathAndQuery, token string, body []byte, dest any, ok ...int) (int, error) {
	u, err := url.Parse(base.String())
	if err != nil {
		return 0, err
	}
	if i := strings.IndexByte(pathAndQuery, '?'); i >= 0 {
		u.Path = pathAndQuery[:i]
		u.RawQuery = pathAndQuery[i+1:]
	} else {
		u.Path = pathAndQuery
	}
	if !c.allowed(u) {
		return 0, errors.New("plexwatch: host is not allowlisted")
	}
	if strings.Contains(u.Path, "/services") || strings.Contains(u.Path, "/api/home/") {
		return 0, errors.New("plexwatch: that credential route is not used")
	}
	if strings.HasSuffix(u.Path, "/view_state_sync") && method != http.MethodGet {
		return 0, errors.New("plexwatch: watch-state consent is read-only")
	}
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rdr)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Plex-Product", "Ether")
	req.Header.Set("X-Plex-Client-Identifier", c.clientID)
	if token != "" {
		req.Header.Set("X-Plex-Token", token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("plexwatch: %s %s: %w", method, u.Host, redactURL(err))
	}
	defer resp.Body.Close()
	slurp, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return resp.StatusCode, fmt.Errorf("plexwatch: read %s", u.Host)
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return resp.StatusCode, &StatusError{Status: resp.StatusCode, Op: method + " " + u.Host}
	}
	good := false
	for _, want := range ok {
		if resp.StatusCode == want {
			good = true
		}
	}
	if !good {
		delay, _ := retryafter.Parse(resp.Header.Get("Retry-After"), c.now(), 15*time.Minute)
		return resp.StatusCode, &StatusError{Status: resp.StatusCode, RetryAfter: delay, Op: method + " " + u.Host}
	}
	if dest != nil && len(bytes.TrimSpace(slurp)) > 0 {
		if err := json.Unmarshal(slurp, dest); err != nil {
			return resp.StatusCode, fmt.Errorf("plexwatch: decode %s", u.Host)
		}
	}
	return resp.StatusCode, nil
}

func redactURL(err error) error {
	var ue *url.Error
	if !errors.As(err, &ue) {
		return err
	}
	safe := ue.URL
	if u, perr := url.Parse(ue.URL); perr == nil {
		u.RawQuery, u.Fragment, u.User = "", "", nil
		safe = u.String()
	}
	return &url.Error{Op: ue.Op, URL: safe, Err: ue.Err}
}

// starRating is a whole star on Plex's 10-point scale. Captured payloads
// used 8 and 10. The support article's example is "4 stars" (8). 2, 4 and 6
// are the same scale. Clears and every other value are not sent.
func starRating(v float64) (int, bool) {
	switch v {
	case 2, 4, 6, 8, 10:
		return int(v), true
	default:
		return 0, false
	}
}
