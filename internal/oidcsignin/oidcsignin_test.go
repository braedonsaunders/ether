package oidcsignin

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// fakeProvider is a minimal OpenID provider: discovery, keys and a token
// endpoint that enforces PKCE and client authentication.
type fakeProvider struct {
	t      *testing.T
	srv    *httptest.Server
	key    *rsa.PrivateKey
	secret string

	// issuerSuffix is appended to the server URL to form the issuer, for
	// providers whose issuer ends in a slash or a path.
	issuerSuffix string

	mu    sync.Mutex
	codes map[string]pendingCode
	// claims overrides fields of the next ID token.
	claims func(map[string]any)
}

type pendingCode struct {
	challenge, redirect, nonce string
}

func newFakeProvider(t *testing.T) *fakeProvider {
	return newFakeProviderAt(t, "")
}

func newFakeProviderAt(t *testing.T, suffix string) *fakeProvider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &fakeProvider{t: t, key: key, secret: "s3cret", codes: map[string]pendingCode{}, issuerSuffix: suffix}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+strings.TrimSuffix(suffix, "/")+"/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                p.issuer(),
			"authorization_endpoint":                p.srv.URL + "/authorize",
			"token_endpoint":                        p.srv.URL + "/token",
			"jwks_uri":                              p.srv.URL + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("POST /token", p.token)
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

// approve stands in for the person signing in at the provider: it reads
// the authorization request and returns a code for it.
func (p *fakeProvider) approve(t *testing.T, authURL string) (state, code string) {
	t.Helper()
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
		t.Fatalf("authorization request lacks PKCE: %s", authURL)
	}
	if q.Get("nonce") == "" || q.Get("state") == "" || q.Get("client_id") != "ether" {
		t.Fatalf("authorization request lacks nonce, state or client: %s", authURL)
	}
	code = "code-" + q.Get("state")[:8]
	p.mu.Lock()
	p.codes[code] = pendingCode{challenge: q.Get("code_challenge"), redirect: q.Get("redirect_uri"), nonce: q.Get("nonce")}
	p.mu.Unlock()
	return q.Get("state"), code
}

func (p *fakeProvider) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", 400)
		return
	}
	id, secret, ok := r.BasicAuth()
	if !ok {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	if id != "ether" || secret != p.secret {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":"invalid_client"}`))
		return
	}
	p.mu.Lock()
	pending, ok := p.codes[r.PostForm.Get("code")]
	delete(p.codes, r.PostForm.Get("code"))
	p.mu.Unlock()
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != pending.challenge || r.PostForm.Get("redirect_uri") != pending.redirect {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
		return
	}
	claims := map[string]any{
		"iss":                p.issuer(),
		"sub":                "user-1",
		"aud":                "ether",
		"exp":                time.Now().Add(5 * time.Minute).Unix(),
		"iat":                time.Now().Unix(),
		"nonce":              pending.nonce,
		"preferred_username": "alex",
		"email":              "alex@example.test",
	}
	if p.claims != nil {
		p.claims(claims)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: p.key}, (&jose.SignerOptions{}).WithHeader("kid", "k1"))
	if err != nil {
		p.t.Error(err)
		return
	}
	raw, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		p.t.Error(err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "id_token": raw, "expires_in": 300})
}

func (p *fakeProvider) issuer() string { return p.srv.URL + p.issuerSuffix }

func (p *fakeProvider) settings() Settings {
	return Settings{Issuer: p.issuer(), ClientID: "ether", ClientSecret: p.secret}
}

const redirect = "https://ether.example.test/ether/api/oidc/callback"

func TestSignInVerifiesAndReleasesOnceToTheHandle(t *testing.T) {
	p := newFakeProvider(t)
	c := New(p.srv.Client())
	ctx := context.Background()
	st, err := c.Start(ctx, p.settings(), redirect, PurposePortal, 0)
	if err != nil {
		t.Fatal(err)
	}
	handle, authURL := st.Handle, st.AuthURL
	if _, done, err := c.Take(handle, st.Binding, 0, PurposePortal, 0); err != nil || done {
		t.Fatalf("pending take = done %v err %v", done, err)
	}
	state, code := p.approve(t, authURL)
	if state != st.State || state == handle || st.Binding == "" || st.Binding == handle || st.Binding == state {
		t.Fatal("state, handle and binding must be distinct and only the state may travel through the provider")
	}
	if err := c.Callback(ctx, state, st.Binding, code, "", 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Take(handle, st.Binding, 0, PurposeOwner, 0); !errors.Is(err, ErrExpired) {
		t.Fatalf("take for another purpose = %v, want ErrExpired", err)
	}
	if _, _, err := c.Take(handle, "", 0, PurposePortal, 0); !errors.Is(err, ErrRejected) {
		t.Fatalf("take without browser proof = %v, want ErrRejected", err)
	}
	id, done, err := c.Take(handle, st.Binding, 0, PurposePortal, 0)
	if err != nil || !done {
		t.Fatalf("take = done %v err %v", done, err)
	}
	if id.Subject != SubjectFor(p.srv.URL, "user-1") || id.Display != "alex" {
		t.Fatalf("identity = %+v", id)
	}
	if _, _, err := c.Take(handle, st.Binding, 0, PurposePortal, 0); !errors.Is(err, ErrExpired) {
		t.Fatalf("second take = %v, want ErrExpired", err)
	}
	if err := c.Callback(ctx, state, st.Binding, code, "", 0); !errors.Is(err, ErrExpired) {
		t.Fatalf("replayed callback = %v, want ErrExpired", err)
	}
}

func TestLinkFlowIsBoundToTheStartingViewer(t *testing.T) {
	p := newFakeProvider(t)
	c := New(p.srv.Client())
	ctx := context.Background()
	if _, err := c.Start(ctx, p.settings(), redirect, PurposeLink, 0); err == nil {
		t.Fatal("a link needs a viewer")
	}
	st, err := c.Start(ctx, p.settings(), redirect, PurposeLink, 7)
	if err != nil {
		t.Fatal(err)
	}
	handle := st.Handle
	state, code := p.approve(t, st.AuthURL)
	if err := c.Callback(ctx, state, st.Binding, code, "", 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Take(handle, st.Binding, 0, PurposeLink, 8); !errors.Is(err, ErrExpired) {
		t.Fatalf("take by another viewer = %v, want ErrExpired", err)
	}
	if _, done, err := c.Take(handle, st.Binding, 0, PurposeLink, 7); err != nil || !done {
		t.Fatalf("take by the starting viewer = done %v err %v", done, err)
	}
}

func TestRejectedAnswersEndTheFlow(t *testing.T) {
	cases := map[string]struct {
		claims        func(map[string]any)
		providerError string
		secret        string
		binding       *string
	}{
		"nonce":          {claims: func(c map[string]any) { c["nonce"] = "other" }},
		"audience":       {claims: func(c map[string]any) { c["aud"] = "someone-else" }},
		"issuer":         {claims: func(c map[string]any) { c["iss"] = "https://evil.example.test" }},
		"expired":        {claims: func(c map[string]any) { c["exp"] = time.Now().Add(-time.Hour).Unix() }},
		"provider error": {providerError: "access_denied"},
		"client secret":  {secret: "wrong"},
		// A sign-in link forwarded to another browser arrives without the
		// starting browser's binding cookie.
		"no binding":    {binding: new(string)},
		"wrong binding": {binding: func() *string { v := "0123456789abcdef"; return &v }()},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			p := newFakeProvider(t)
			p.claims = tc.claims
			c := New(p.srv.Client())
			s := p.settings()
			if tc.secret != "" {
				s.ClientSecret = tc.secret
			}
			st, err := c.Start(context.Background(), s, redirect, PurposePortal, 0)
			if err != nil {
				t.Fatal(err)
			}
			handle := st.Handle
			state, code := p.approve(t, st.AuthURL)
			binding := st.Binding
			if tc.binding != nil {
				binding = *tc.binding
			}
			if err := c.Callback(context.Background(), state, binding, code, tc.providerError, 0); !errors.Is(err, ErrRejected) {
				t.Fatalf("callback = %v, want ErrRejected", err)
			}
			if _, done, err := c.Take(handle, st.Binding, 0, PurposePortal, 0); !done || !errors.Is(err, ErrRejected) {
				t.Fatalf("take = done %v err %v, want rejected", done, err)
			}
		})
	}
}

func TestFlowsExpire(t *testing.T) {
	p := newFakeProvider(t)
	c := New(p.srv.Client())
	now := time.Now()
	c.now = func() time.Time { return now }
	st, err := c.Start(context.Background(), p.settings(), redirect, PurposeOwner, 0)
	if err != nil {
		t.Fatal(err)
	}
	handle := st.Handle
	state, code := p.approve(t, st.AuthURL)
	now = now.Add(FlowTTL + time.Second)
	if err := c.Callback(context.Background(), state, st.Binding, code, "", 0); !errors.Is(err, ErrExpired) {
		t.Fatalf("late callback = %v, want ErrExpired", err)
	}
	if _, _, err := c.Take(handle, st.Binding, 0, PurposeOwner, 0); !errors.Is(err, ErrExpired) {
		t.Fatalf("late take = %v, want ErrExpired", err)
	}
}

func TestCompletedFlowsRequireCurrentRevisionAndDeadline(t *testing.T) {
	for _, name := range []string{"deadline", "revision"} {
		t.Run(name, func(t *testing.T) {
			p := newFakeProvider(t)
			c := New(p.srv.Client())
			now := time.Now()
			c.now = func() time.Time { return now }
			settings := p.settings()
			settings.Revision = 1
			st, err := c.Start(t.Context(), settings, redirect, PurposePortal, 0)
			if err != nil {
				t.Fatal(err)
			}
			state, code := p.approve(t, st.AuthURL)
			if err := c.Callback(t.Context(), state, st.Binding, code, "", settings.Revision); err != nil {
				t.Fatal(err)
			}
			if name == "deadline" {
				now = now.Add(FlowTTL)
			} else {
				settings.Revision++
			}
			if _, _, err := c.Take(st.Handle, st.Binding, settings.Revision, PurposePortal, 0); !errors.Is(err, ErrExpired) {
				t.Fatalf("take = %v, want ErrExpired", err)
			}
		})
	}
}

func TestProviderTransportValidation(t *testing.T) {
	for _, endpoint := range []string{"authorization_endpoint", "token_endpoint", "jwks_uri", "userinfo_endpoint"} {
		t.Run(endpoint, func(t *testing.T) {
			var issuer string
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				metadata := map[string]string{
					"issuer":                 issuer,
					"authorization_endpoint": issuer + "/authorize",
					"token_endpoint":         issuer + "/token",
					"jwks_uri":               issuer + "/jwks",
					"userinfo_endpoint":      issuer + "/userinfo",
				}
				metadata[endpoint] = "http://127.0.0.1:12345/" + endpoint
				_ = json.NewEncoder(w).Encode(metadata)
			}))
			t.Cleanup(server.Close)
			issuer = server.URL
			if err := New(server.Client()).Check(t.Context(), Settings{Issuer: issuer, ClientID: "ether"}); err == nil {
				t.Fatalf("https issuer accepted a plaintext %s", endpoint)
			}
		})
	}
	for _, tc := range []struct {
		name, method, from, to string
		allowed                bool
	}{
		{"same origin", "POST", "https://auth.example.test/token", "https://auth.example.test/v2/token", true},
		{"downgrade", "GET", "https://auth.example.test/jwks", "http://127.0.0.1:12345/jwks", false},
		{"cross-origin token exchange", "POST", "https://auth.example.test/token", "https://other.example.test/token", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			origin := httptest.NewRequest(tc.method, tc.from, nil)
			next := httptest.NewRequest(tc.method, tc.to, nil)
			err := New(nil).HTTP.CheckRedirect(next, []*http.Request{origin})
			if (err == nil) != tc.allowed {
				t.Fatalf("redirect permission = %v", err)
			}
		})
	}
}

func TestStartNeedsSettingsAndAReachableIssuer(t *testing.T) {
	c := New(nil)
	if _, err := c.Start(context.Background(), Settings{ClientID: "ether"}, redirect, PurposeOwner, 0); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("start without issuer = %v", err)
	}
	p := newFakeProvider(t)
	c = New(p.srv.Client())
	if _, err := c.Start(context.Background(), p.settings(), "javascript:alert(1)", PurposeOwner, 0); err == nil {
		t.Fatal("start accepted a non-http redirect")
	}
	if err := c.Check(context.Background(), p.settings()); err != nil {
		t.Fatalf("check = %v", err)
	}
	p.srv.Close()
	if err := c.Check(context.Background(), p.settings()); err == nil {
		t.Fatal("check passed against a stopped provider")
	}
}

func TestProfileClaims(t *testing.T) {
	for name, verified := range map[string]any{"bool": true, "string": "true", "false": false} {
		t.Run(name, func(t *testing.T) {
			p := newFakeProvider(t)
			p.claims = func(c map[string]any) {
				c["email_verified"] = verified
				c["name"] = "Alex Example"
				c["groups"] = []string{"ether", "admins"}
			}
			c := New(p.srv.Client())
			s := p.settings()
			s.Scopes = []string{"groups", " ", "openid"}
			st, err := c.Start(context.Background(), s, redirect, PurposePortal, 0)
			if err != nil {
				t.Fatal(err)
			}
			u, _ := url.Parse(st.AuthURL)
			if scope := u.Query().Get("scope"); scope != "openid profile email groups" {
				t.Fatalf("scope = %q", scope)
			}
			state, code := p.approve(t, st.AuthURL)
			if err := c.Callback(context.Background(), state, st.Binding, code, "", 0); err != nil {
				t.Fatal(err)
			}
			id, _, err := c.Take(st.Handle, st.Binding, 0, PurposePortal, 0)
			if err != nil {
				t.Fatal(err)
			}
			want := name != "false"
			if id.Email != "alex@example.test" || id.EmailVerified != want || id.Username != "alex" ||
				id.Name != "Alex Example" || len(id.Groups) != 2 || id.Groups[0] != "ether" {
				t.Fatalf("identity = %+v", id)
			}
		})
	}
}

// Issuers are used exactly as configured: a trailing slash (Auth0, some
// Keycloak and Entra setups) or a path stays part of the identity.
func TestIssuerKeptExactly(t *testing.T) {
	for _, suffix := range []string{"/", "/realms/home", "/application/o/ether/"} {
		t.Run(suffix, func(t *testing.T) {
			p := newFakeProviderAt(t, suffix)
			c := New(p.srv.Client())
			st, err := c.Start(context.Background(), p.settings(), redirect, PurposePortal, 0)
			if err != nil {
				t.Fatal(err)
			}
			state, code := p.approve(t, st.AuthURL)
			if err := c.Callback(context.Background(), state, st.Binding, code, "", 0); err != nil {
				t.Fatal(err)
			}
			id, _, err := c.Take(st.Handle, st.Binding, 0, PurposePortal, 0)
			if err != nil || id.Subject != SubjectFor(p.issuer(), "user-1") {
				t.Fatalf("identity = %+v %v", id, err)
			}
			// The same provider without its trailing slash is a different issuer.
			if strings.HasSuffix(suffix, "/") {
				s := p.settings()
				s.Issuer = strings.TrimSuffix(s.Issuer, "/")
				if _, err := c.Start(context.Background(), s, redirect, PurposePortal, 0); err == nil {
					t.Fatal("an issuer that differs from discovery was accepted")
				}
			}
		})
	}
}

func TestGroupsClaimNameAndShape(t *testing.T) {
	p := newFakeProvider(t)
	p.claims = func(c map[string]any) { c["roles"] = "ether" }
	c := New(p.srv.Client())
	s := p.settings()
	s.GroupsClaim = "roles"
	st, err := c.Start(context.Background(), s, redirect, PurposePortal, 0)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(st.AuthURL)
	if scope := u.Query().Get("scope"); scope != "openid profile email" {
		t.Fatalf("scope = %q, want no extra scopes unless configured", scope)
	}
	state, code := p.approve(t, st.AuthURL)
	if err := c.Callback(context.Background(), state, st.Binding, code, "", 0); err != nil {
		t.Fatal(err)
	}
	id, _, _ := c.Take(st.Handle, st.Binding, 0, PurposePortal, 0)
	if len(id.Groups) != 1 || id.Groups[0] != "ether" {
		t.Fatalf("groups = %v", id.Groups)
	}
}
