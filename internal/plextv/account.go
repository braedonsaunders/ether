package plextv

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// PIN is a plex.tv sign-in PIN (POST /api/v2/pins).
type PIN struct {
	ID               int64  `json:"id"`
	Code             string `json:"code"`
	ClientIdentifier string `json:"clientIdentifier"`
	Trusted          bool   `json:"trusted"`
	ExpiresIn        int    `json:"expiresIn"`
	ExpiresAt        time.Time
	// AuthToken is set once the user approved the PIN.
	AuthToken string `json:"authToken"`
}

type pinJSON struct {
	ID               int64    `json:"id"`
	Code             string   `json:"code"`
	ClientIdentifier string   `json:"clientIdentifier"`
	Trusted          bool     `json:"trusted"`
	ExpiresIn        int      `json:"expiresIn"`
	ExpiresAt        flexTime `json:"expiresAt"`
	AuthToken        *string  `json:"authToken"`
}

func decodePIN(b []byte) (*PIN, error) {
	var v pinJSON
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("plextv: decode pin: %w", err)
	}
	if v.ID == 0 || v.Code == "" {
		return nil, errors.New("plextv: pin response without id/code")
	}
	p := &PIN{ID: v.ID, Code: v.Code, ClientIdentifier: v.ClientIdentifier, Trusted: v.Trusted,
		ExpiresIn: v.ExpiresIn, ExpiresAt: v.ExpiresAt.Time}
	if v.AuthToken != nil {
		p.AuthToken = *v.AuthToken
	}
	if p.ExpiresAt.IsZero() && p.ExpiresIn > 0 {
		p.ExpiresAt = time.Now().Add(time.Duration(p.ExpiresIn) * time.Second)
	}
	return p, nil
}

// CreatePIN starts a sign-in: POST /api/v2/pins?strong=true. Strong PINs are
// long codes meant for the hosted auth page (AuthURL) rather than typing.
func (c *Client) CreatePIN(ctx context.Context) (*PIN, error) {
	if c.id.ClientIdentifier == "" {
		return nil, errors.New("plextv: client identifier required for PIN sign-in")
	}
	resp, err := c.do(ctx, request{method: http.MethodPost, path: "/api/v2/pins",
		query: url.Values{"strong": {"true"}}})
	if err != nil {
		return nil, err
	}
	return decodePIN(resp.body)
}

// CheckPIN polls a PIN: GET /api/v2/pins/{id}?code=. AuthToken is empty
// until the user approves it. It must be called with the same client
// identifier that created the PIN.
func (c *Client) CheckPIN(ctx context.Context, id int64, code string) (*PIN, error) {
	resp, err := c.do(ctx, request{method: http.MethodGet, path: "/api/v2/pins/" + strconv.FormatInt(id, 10),
		query: url.Values{"code": {code}}})
	if err != nil {
		return nil, err
	}
	return decodePIN(resp.body)
}

// ErrPINExpired is returned by WaitPIN when the PIN expired unapproved.
var ErrPINExpired = errors.New("plextv: sign-in PIN expired")

// WaitPIN polls until the PIN is approved, expires or ctx ends.
func (c *Client) WaitPIN(ctx context.Context, pin *PIN, interval time.Duration) (string, error) {
	if interval <= 0 {
		interval = time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		p, err := c.CheckPIN(ctx, pin.ID, pin.Code)
		switch {
		case err == nil && p.AuthToken != "":
			return p.AuthToken, nil
		case err != nil && errors.Is(err, core.ErrNotFound):
			return "", ErrPINExpired // plex.tv deletes expired PINs
		case err != nil && ctx.Err() != nil:
			return "", ctx.Err()
		case err != nil:
			c.log.Debug("pin poll failed", "err", err)
		}
		if !pin.ExpiresAt.IsZero() && time.Now().After(pin.ExpiresAt) {
			return "", ErrPINExpired
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-t.C:
		}
	}
}

// AuthURL is the app.plex.tv page where the owner approves pin. forwardURL,
// when set, is where plex.tv sends the browser afterwards (the wizard).
func (c *Client) AuthURL(pin *PIN, forwardURL string) string {
	q := url.Values{}
	q.Set("clientID", c.id.ClientIdentifier)
	q.Set("code", pin.Code)
	q.Set("context[device][product]", c.id.Product)
	if c.id.Version != "" {
		q.Set("context[device][version]", c.id.Version)
	}
	if c.id.Platform != "" {
		q.Set("context[device][platform]", c.id.Platform)
	}
	if c.id.DeviceName != "" {
		q.Set("context[device][deviceName]", c.id.DeviceName)
	}
	if forwardURL != "" {
		q.Set("forwardUrl", forwardURL)
	}
	return c.authApp + "#?" + q.Encode()
}

// Account is a plex.tv user (GET /api/v2/user).
type Account struct {
	ID           int64         `json:"id"`
	UUID         string        `json:"uuid"`
	Username     string        `json:"username"`
	Title        string        `json:"title"`
	FriendlyName string        `json:"friendlyName"`
	Email        string        `json:"email"`
	Thumb        string        `json:"thumb"`
	AuthToken    string        `json:"authToken"`
	Home         bool          `json:"home"`
	HomeAdmin    bool          `json:"homeAdmin"`
	Restricted   bool          `json:"restricted"`
	Subscription Subscription  `json:"subscription"`
	Track        TrackDefaults `json:"profile,omitempty"`
}

// Subscription is the account's Plex Pass state.
type Subscription struct {
	Active   bool     `json:"active"`
	Status   string   `json:"status"`
	Plan     string   `json:"plan"`
	Features []string `json:"features"`
}

// DisplayName prefers the friendly name, then title, then username.
func (a *Account) DisplayName() string {
	for _, s := range []string{a.FriendlyName, a.Title, a.Username} {
		if s != "" {
			return s
		}
	}
	return a.Email
}

// User returns the account a token belongs to. A revoked or unknown token
// yields an error matching core.ErrUnauthorized.
func (c *Client) User(ctx context.Context, token string) (*Account, error) {
	if token == "" {
		return nil, &APIError{Method: http.MethodGet, Path: "/api/v2/user", Status: http.StatusUnauthorized, Message: "no token"}
	}
	resp, err := c.do(ctx, request{method: http.MethodGet, host: c.serversHost(), path: "/api/v2/user",
		query: url.Values{"includeSubscriptions": {"1"}, "includeProviders": {"1"}}, token: token})
	if err != nil {
		return nil, err
	}
	var a Account
	if bytes.HasPrefix(bytes.TrimSpace(resp.body), []byte("<")) {
		n, err := parseXMLNode(resp.body)
		if err != nil {
			return nil, fmt.Errorf("plextv: decode user: %w", err)
		}
		a.ID, a.UUID, a.Username, a.Title = n.intAttr("id"), n.attr("uuid"), n.attr("username"), n.attr("title")
		a.FriendlyName, a.Email, a.Thumb = n.attr("friendlyName"), n.attr("email"), n.attr("thumb")
		a.AuthToken = n.attr("authToken")
		a.Home, a.HomeAdmin, a.Restricted = n.boolAttr("home"), n.boolAttr("homeAdmin"), n.boolAttr("restricted")
		if sub := n.find("subscription"); sub != nil {
			a.Subscription = Subscription{Active: sub.boolAttr("active") || strings.EqualFold(sub.attr("status"), "active"), Status: sub.attr("status"), Plan: sub.attr("plan")}
			for _, f := range sub.findAll("feature") {
				if id := f.attr("id"); id != "" {
					a.Subscription.Features = append(a.Subscription.Features, id)
				}
			}
		}
		if profile := n.find("profile"); profile != nil {
			a.Track = profileDefaults(profile)
		} else if n.attr("defaultAudioLanguage") != "" || n.attr("defaultSubtitleLanguage") != "" || n.attr("autoSelectAudio") != "" || n.attr("autoSelectSubtitle") != "" {
			a.Track = profileDefaults(n)
		}
	} else if err := json.Unmarshal(resp.body, &a); err != nil {
		return nil, fmt.Errorf("plextv: decode user: %w", err)
	} else if parsed := trackDefaultsFromJSON(resp.body); parsed.HasProfile {
		a.Track = parsed
	} else if a.Track.AudioLanguage != "" || a.Track.SubtitleLanguage != "" || a.Track.AutoSelectAudio || a.Track.SubtitleMode != 0 {
		// Keep the first decode when the profile object used a shape
		// trackDefaultsFromJSON does not accept.
		a.Track.HasProfile = true
	}
	if a.ID == 0 {
		return nil, errors.New("plextv: user response without id")
	}
	return &a, nil
}

// ClaimToken fetches a short-lived claim token ("claim-xxxx") for the
// signed-in user: GET /api/claim/token.json. This is what plex.tv/claim
// shows and what Plex Web hands a fresh server during setup.
func (c *Client) ClaimToken(ctx context.Context, userToken string) (string, error) {
	resp, err := c.do(ctx, request{method: http.MethodGet, path: "/api/claim/token.json", token: userToken})
	if err != nil {
		return "", err
	}
	var v struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(resp.body, &v); err != nil || !strings.HasPrefix(v.Token, "claim-") {
		return "", fmt.Errorf("plextv: unexpected claim token response")
	}
	return v.Token, nil
}

var tokenElement = regexp.MustCompile(`(?s)<authentication-token>\s*([^<\s]+)\s*</authentication-token>`)

// ExchangeClaim turns a claim token into the server's own plex.tv token:
// POST /api/claim/exchange?token=claim-xxxx, sent with the server identity
// (X-Plex-Provides: server, client identifier = machine identifier). This is
// exactly what PMS (and the official Docker image's first-run script) do;
// the response is a <user> document with an <authentication-token> element.
func (c *Client) ExchangeClaim(ctx context.Context, claimToken string) (string, error) {
	if c.id.Provides != "server" {
		return "", errors.New("plextv: claim exchange needs the server identity")
	}
	resp, err := c.do(ctx, request{method: http.MethodPost, path: "/api/claim/exchange",
		query: url.Values{"token": {claimToken}}, accept: "application/xml"})
	if err != nil {
		return "", err
	}
	if tok := extractAuthToken(resp.body); tok != "" {
		return tok, nil
	}
	return "", errors.New("plextv: claim exchange response without authentication token")
}

func extractAuthToken(body []byte) string {
	if m := tokenElement.FindSubmatch(body); m != nil {
		return string(m[1])
	}
	trim := strings.TrimSpace(string(body))
	if strings.HasPrefix(trim, "{") {
		var v struct {
			AuthToken           string `json:"authToken"`
			AuthenticationToken string `json:"authenticationToken"`
			User                struct {
				AuthToken string `json:"authToken"`
			} `json:"user"`
		}
		if json.Unmarshal(body, &v) == nil {
			for _, s := range []string{v.AuthToken, v.AuthenticationToken, v.User.AuthToken} {
				if s != "" {
					return s
				}
			}
		}
		return ""
	}
	if n, err := parseXMLNode(body); err == nil {
		if u := n.find("user"); u != nil {
			return u.attr("authenticationToken", "authToken", "authentication-token")
		}
		return n.attr("authenticationToken", "authToken")
	}
	return ""
}

// ErrClaimExchangeFailed matches any *ClaimExchangeError via errors.Is.
var ErrClaimExchangeFailed = errors.New("plextv: claim exchange failed")

// ClaimExchangeError reports that plex.tv refused or failed the claim-token
// exchange that mints a server-specific token.
type ClaimExchangeError struct {
	Err error
}

func (e *ClaimExchangeError) Error() string {
	return fmt.Sprintf("plextv: claim exchange failed: %v", e.Err)
}

func (e *ClaimExchangeError) Unwrap() error {
	return e.Err
}

func (e *ClaimExchangeError) Is(target error) bool {
	return target == ErrClaimExchangeFailed
}

// ClaimResult is the outcome of claiming the server.
type ClaimResult struct {
	ServerToken string
	Owner       Account
}

// Claim claims this server for the user behind userToken, the way a fresh
// PMS install is claimed: fetch a claim token for the user, exchange it
// under the server identity for the server's own token, then confirm the
// owner.
//
// c must carry the server identity.
func (c *Client) Claim(ctx context.Context, userToken string) (*ClaimResult, error) {
	owner, err := c.User(ctx, userToken)
	if err != nil {
		return nil, fmt.Errorf("plextv: claim: verify user: %w", err)
	}
	claim, err := c.ClaimToken(ctx, userToken)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &ClaimExchangeError{Err: err}
	}
	serverToken, err := c.ExchangeClaim(ctx, claim)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &ClaimExchangeError{Err: err}
	}
	acct, verr := c.User(ctx, serverToken)
	if verr == nil && acct.ID != owner.ID {
		return nil, fmt.Errorf("plextv: claim: server token belongs to account %d, not %d", acct.ID, owner.ID)
	}
	if verr != nil {
		c.log.Warn("could not verify server token owner", "err", verr)
	}
	return &ClaimResult{ServerToken: serverToken, Owner: *owner}, nil
}
