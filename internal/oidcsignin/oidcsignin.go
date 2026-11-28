// Package oidcsignin signs household members in through an OpenID Connect
// provider such as Pocket ID, Authelia, Authentik or Keycloak.
//
// It runs the authorization code flow with PKCE, a state and a nonce, and
// verifies the returned ID token against the provider's published keys. It
// only proves who the provider says the person is: mapping that identity to a
// viewer and issuing Ether credentials stay with the identity and admin
// packages.
//
// A flow has three random values. The state travels through the provider and
// back to the callback. The binding is kept by the starting browser (the
// caller stores it in a cookie) and must come back with the callback, so a
// sign-in link forwarded to someone else's browser fails there instead of
// signing that person in for the sender. The handle is returned only to the
// browser that started the flow, which polls with it; the result is released
// only to the holder of the handle.
package oidcsignin

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/braedonsaunders/ether/internal/httpclient"
)

// Provider is the identity_links provider name for OIDC identities.
const Provider = "oidc"

// FlowTTL bounds how long a person has to finish signing in at the provider.
const FlowTTL = 10 * time.Minute

// maxFlows caps pending flows so unauthenticated starts cannot grow memory
// without bound. Starts are also rate limited by the caller.
const maxFlows = 1024

// discoveryTTL is how long a provider's discovery document is reused. Signing
// keys refresh on their own when an unknown key ID appears.
const discoveryTTL = time.Hour

var (
	// ErrNotConfigured means OIDC sign-in is off or incomplete.
	ErrNotConfigured = errors.New("oidcsignin: not configured")
	// ErrExpired means the handle or state is unknown, used or expired.
	ErrExpired = errors.New("oidcsignin: sign-in expired")
	// ErrBusy means too many sign-ins are pending.
	ErrBusy = errors.New("oidcsignin: too many pending sign-ins")
	// ErrRejected means the provider refused the sign-in or its answer did
	// not verify.
	ErrRejected = errors.New("oidcsignin: sign-in rejected")
)

// Settings are the owner's provider settings.
type Settings struct {
	Issuer       string
	ClientID     string
	ClientSecret string
}

func (s Settings) normalized() Settings {
	return Settings{
		Issuer:       strings.TrimSpace(s.Issuer),
		ClientID:     strings.TrimSpace(s.ClientID),
		ClientSecret: strings.TrimSpace(s.ClientSecret),
	}
}

// Usable reports whether s names a provider and a client.
func (s Settings) Usable() bool {
	s = s.normalized()
	return s.Issuer != "" && s.ClientID != ""
}

// Identity is the verified result of a sign-in.
type Identity struct {
	// Subject is unique per provider and person: the issuer and the
	// provider's subject claim. It never changes when the person renames
	// themselves at the provider.
	Subject string
	// Display is a readable name for sign-in method lists.
	Display string
}

// SubjectFor joins an issuer and a subject claim into an identity subject.
func SubjectFor(issuer, sub string) string { return issuer + "|" + sub }

// Purpose says what a finished flow may be used for. A handle started for
// one purpose cannot be redeemed for another.
type Purpose string

const (
	PurposeOwner  Purpose = "owner"
	PurposePortal Purpose = "viewer-portal"
	PurposeLink   Purpose = "link"
)

type flow struct {
	settings    Settings
	redirectURL string
	verifier    string
	nonce       string
	state       string
	binding     string
	handle      string
	purpose     Purpose
	viewerID    int64
	expires     time.Time
	done        bool
	identity    Identity
	err         error
}

type cachedProvider struct {
	issuer  string
	p       *oidc.Provider
	fetched time.Time
}

// Client runs sign-in flows. The zero value is not usable; call New.
type Client struct {
	// HTTP is used for discovery, keys and the token exchange.
	HTTP *http.Client
	now  func() time.Time

	mu        sync.Mutex
	byState   map[string]*flow
	byHandle  map[string]*flow
	providers map[string]cachedProvider
}

// userAgent sets Ether's outbound User-Agent on provider requests.
type userAgent struct{ next http.RoundTripper }

func (u userAgent) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("User-Agent", httpclient.UserAgent())
	return u.next.RoundTrip(r)
}

// New returns a Client that uses httpClient (a 15-second client that sends
// Ether's User-Agent when nil).
func New(httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second, Transport: userAgent{next: http.DefaultTransport}}
	}
	return &Client{
		HTTP:      httpClient,
		now:       time.Now,
		byState:   map[string]*flow{},
		byHandle:  map[string]*flow{},
		providers: map[string]cachedProvider{},
	}
}

func (c *Client) ctx(ctx context.Context) context.Context {
	return oidc.ClientContext(ctx, c.HTTP)
}

// provider returns the discovered provider for issuer, reusing a recent one.
func (c *Client) provider(ctx context.Context, issuer string) (*oidc.Provider, error) {
	c.mu.Lock()
	cached, ok := c.providers[issuer]
	c.mu.Unlock()
	if ok && c.now().Sub(cached.fetched) < discoveryTTL {
		return cached.p, nil
	}
	p, err := oidc.NewProvider(c.ctx(ctx), issuer)
	if err != nil {
		return nil, fmt.Errorf("oidcsignin: discover %s: %w", issuer, err)
	}
	c.mu.Lock()
	c.providers[issuer] = cachedProvider{issuer: issuer, p: p, fetched: c.now()}
	c.mu.Unlock()
	return p, nil
}

// Check discovers the provider, which proves the issuer URL is right and
// reachable. It cannot prove the client ID or secret: only a real sign-in can.
func (c *Client) Check(ctx context.Context, s Settings) error {
	s = s.normalized()
	if !s.Usable() {
		return ErrNotConfigured
	}
	c.mu.Lock()
	delete(c.providers, s.Issuer)
	c.mu.Unlock()
	_, err := c.provider(ctx, s.Issuer)
	return err
}

func oauthConfig(p *oidc.Provider, s Settings, redirectURL string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     s.ClientID,
		ClientSecret: s.ClientSecret,
		Endpoint:     p.Endpoint(),
		RedirectURL:  redirectURL,
		Scopes:       []string{oidc.ScopeOpenID, "profile", "email"},
	}
}

func random() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// prune drops expired flows. c.mu must be held.
func (c *Client) prune(now time.Time) {
	for k, f := range c.byHandle {
		if now.After(f.expires) {
			delete(c.byHandle, k)
			delete(c.byState, f.state)
		}
	}
}

// Started is a new sign-in. AuthURL opens the provider. Handle goes to the
// starting browser's script; Binding goes to the same browser in a cookie
// that the callback request carries back with State.
type Started struct {
	Handle, AuthURL, State, Binding string
}

// Start begins a sign-in. redirectURL is this server's callback as the
// browser addresses it and must be registered with the provider. viewerID
// binds a PurposeLink flow to the signed-in viewer who started it.
func (c *Client) Start(ctx context.Context, s Settings, redirectURL string, purpose Purpose, viewerID int64) (Started, error) {
	s = s.normalized()
	if !s.Usable() {
		return Started{}, ErrNotConfigured
	}
	if purpose == PurposeLink && viewerID <= 0 {
		return Started{}, errors.New("oidcsignin: link requires a viewer")
	}
	u, err := url.Parse(redirectURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return Started{}, errors.New("oidcsignin: invalid redirect URL")
	}
	p, err := c.provider(ctx, s.Issuer)
	if err != nil {
		return Started{}, err
	}
	f := &flow{
		settings:    s,
		redirectURL: redirectURL,
		verifier:    oauth2.GenerateVerifier(),
		nonce:       random(),
		state:       random(),
		binding:     random(),
		handle:      random(),
		purpose:     purpose,
		viewerID:    viewerID,
		expires:     c.now().Add(FlowTTL),
	}
	c.mu.Lock()
	c.prune(c.now())
	if len(c.byHandle) >= maxFlows {
		c.mu.Unlock()
		return Started{}, ErrBusy
	}
	c.byState[f.state] = f
	c.byHandle[f.handle] = f
	c.mu.Unlock()
	authURL := oauthConfig(p, s, redirectURL).AuthCodeURL(f.state,
		oauth2.S256ChallengeOption(f.verifier),
		oidc.Nonce(f.nonce),
	)
	return Started{Handle: f.handle, AuthURL: authURL, State: f.state, Binding: f.binding}, nil
}

// Callback finishes the flow named by state with the provider's answer.
// binding is the value the starting browser kept; a callback from any other
// browser is rejected. It returns nil when the person is now verified and
// waiting for the starting browser to collect the result. Every failure,
// including a provider error, ends the flow, so a state is used at most once.
func (c *Client) Callback(ctx context.Context, state, binding, code, providerError string) error {
	c.mu.Lock()
	f, ok := c.byState[state]
	if ok {
		// One callback per state, whatever happens next.
		delete(c.byState, state)
	}
	c.mu.Unlock()
	if !ok || c.now().After(f.expires) || f.done {
		return ErrExpired
	}
	var identity Identity
	var err error
	if subtle.ConstantTimeCompare([]byte(binding), []byte(f.binding)) != 1 {
		err = fmt.Errorf("%w: callback from another browser", ErrRejected)
	} else {
		identity, err = c.complete(ctx, f, code, providerError)
	}
	c.mu.Lock()
	f.done = true
	f.identity = identity
	f.err = err
	c.mu.Unlock()
	return err
}

func (c *Client) complete(ctx context.Context, f *flow, code, providerError string) (Identity, error) {
	if providerError != "" || code == "" {
		return Identity{}, ErrRejected
	}
	p, err := c.provider(ctx, f.settings.Issuer)
	if err != nil {
		return Identity{}, err
	}
	tok, err := oauthConfig(p, f.settings, f.redirectURL).Exchange(c.ctx(ctx), code, oauth2.VerifierOption(f.verifier))
	if err != nil {
		return Identity{}, fmt.Errorf("%w: token exchange: %v", ErrRejected, err)
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		return Identity{}, fmt.Errorf("%w: no ID token", ErrRejected)
	}
	idt, err := p.Verifier(&oidc.Config{ClientID: f.settings.ClientID, Now: c.now}).Verify(c.ctx(ctx), raw)
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %v", ErrRejected, err)
	}
	if idt.Nonce != f.nonce {
		return Identity{}, fmt.Errorf("%w: nonce mismatch", ErrRejected)
	}
	if strings.TrimSpace(idt.Subject) == "" {
		return Identity{}, fmt.Errorf("%w: no subject", ErrRejected)
	}
	var claims struct {
		PreferredUsername string `json:"preferred_username"`
		Email             string `json:"email"`
		Name              string `json:"name"`
	}
	_ = idt.Claims(&claims)
	display := idt.Subject
	for _, v := range []string{claims.PreferredUsername, claims.Email, claims.Name} {
		if v = strings.TrimSpace(v); v != "" {
			display = v
			break
		}
	}
	if len(display) > 128 {
		display = display[:128]
	}
	return Identity{Subject: SubjectFor(idt.Issuer, idt.Subject), Display: display}, nil
}

// Take returns the result of the flow started with handle once its callback
// has run. done is false while the person is still at the provider. A
// finished flow is released once, and only for the purpose (and, for links,
// the viewer) it was started with.
func (c *Client) Take(handle string, purpose Purpose, viewerID int64) (id Identity, done bool, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	f, ok := c.byHandle[handle]
	if !ok || f.purpose != purpose || (purpose == PurposeLink && f.viewerID != viewerID) {
		return Identity{}, false, ErrExpired
	}
	if !f.done {
		if c.now().After(f.expires) {
			delete(c.byHandle, handle)
			delete(c.byState, f.state)
			return Identity{}, false, ErrExpired
		}
		return Identity{}, false, nil
	}
	delete(c.byHandle, handle)
	if f.err != nil {
		return Identity{}, true, ErrRejected
	}
	return f.identity, true, nil
}
