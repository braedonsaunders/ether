// Package simkl implements the optional Simkl integration.
package simkl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// AUTH V2 device flow: https://api.simkl.org/api-reference/oauth2-device
// Token semantics: https://api.simkl.org/api-reference/oauth2-tokens
// The device endpoint reference does not specify PKCE for device grants;
// PKCE is documented for the authorization-code grant.

type AuthClient struct {
	BaseURL, ClientID, ClientSecret string
	HTTP                            *http.Client
}
type DeviceCode struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}
type Token struct {
	ClientID     string `json:"-"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`
}

// APIError deliberately excludes response descriptions, which can contain secrets.
type APIError struct {
	Status     int
	Code       string
	RetryAfter string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("simkl: authorization failed (%d: %s)", e.Status, e.Code)
}

func (c AuthClient) post(ctx context.Context, path string, form url.Values, dst any) error {
	if c.ClientID == "" {
		return errors.New("simkl: client ID required")
	}
	form.Set("client_id", c.ClientID)
	if c.ClientSecret != "" {
		form.Set("client_secret", c.ClientSecret)
	}
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		base = "https://api.simkl.com"
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "Ether/1.0")
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	// Never forward form credentials across a redirect.
	safe := *hc
	safe.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := safe.Do(req)
	if err != nil {
		return fmt.Errorf("simkl: request failed: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var result struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &result)
		switch result.Error {
		case "authorization_pending", "slow_down", "expired_token", "invalid_grant", "invalid_client", "access_denied":
		default:
			result.Error = "unknown"
		}
		return &APIError{Status: resp.StatusCode, Code: result.Error, RetryAfter: resp.Header.Get("Retry-After")}
	}
	if dst == nil {
		return nil
	}
	return json.Unmarshal(data, dst)
}
func (c AuthClient) Start(ctx context.Context) (DeviceCode, error) {
	var d DeviceCode
	err := c.post(ctx, "/oauth2/device", url.Values{"scope": {"media:read media:write"}}, &d)
	if err == nil && (d.DeviceCode == "" || d.UserCode == "" || d.VerificationURI == "" || d.ExpiresIn <= 0 || d.Interval <= 0) {
		err = errors.New("simkl: invalid device response")
	}
	return d, err
}
func (c AuthClient) Poll(ctx context.Context, deviceCode string) (Token, error) {
	var t Token
	err := c.post(ctx, "/oauth2/token", url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}, "device_code": {deviceCode}}, &t)
	if err == nil {
		t.ClientID = c.ClientID
		err = validateToken(t)
	}
	return t, err
}
func (c AuthClient) Refresh(ctx context.Context, refresh string) (Token, error) {
	var t Token
	err := c.post(ctx, "/oauth2/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}}, &t)
	if err == nil {
		t.ClientID = c.ClientID
		err = validateToken(t)
	}
	return t, err
}
func validateToken(t Token) error {
	scopes := strings.Fields(t.Scope)
	read, write := false, false
	for _, s := range scopes {
		if s == "media:read" {
			read = true
		}
		if s == "media:write" {
			write = true
		}
	}
	if t.AccessToken == "" || t.RefreshToken == "" || t.ExpiresIn <= 0 || !read || !write {
		return errors.New("simkl: incomplete token or insufficient scope")
	}
	return nil
}
func (c AuthClient) Revoke(ctx context.Context, token string) error {
	return c.post(ctx, "/oauth2/revoke", url.Values{"token": {token}}, nil)
}
