package admin_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/auth"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/oidcsignin"
	"github.com/braedonsaunders/ether/internal/plextv"
	"github.com/braedonsaunders/ether/internal/store"
)

// stubOIDC stands in for the provider round trip, which oidcsignin tests
// against a real (fake) provider. Here a started flow is approved by
// calling finish with the identity the provider vouched for.
type stubOIDC struct {
	mu       sync.Mutex
	n        int
	flows    map[string]*stubFlow
	redirect string
}

type stubFlow struct {
	state    string
	binding  string
	failed   bool
	purpose  oidcsignin.Purpose
	viewerID int64
	revision uint64
	done     bool
	who      oidcsignin.Identity
}

func (s *stubOIDC) Check(context.Context, oidcsignin.Settings) error { return nil }
func (s *stubOIDC) Start(_ context.Context, settings oidcsignin.Settings, redirectURL string, purpose oidcsignin.Purpose, viewerID int64) (oidcsignin.Started, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	handle := fmt.Sprintf("handle-%d", s.n)
	f := &stubFlow{state: fmt.Sprintf("state-%d-0123456789abcdef", s.n), binding: fmt.Sprintf("binding-%d", s.n), purpose: purpose, viewerID: viewerID, revision: settings.Revision}
	s.flows[handle] = f
	s.redirect = redirectURL
	return oidcsignin.Started{Handle: handle, AuthURL: "https://auth.example.test/authorize?state=" + f.state, State: f.state, Binding: f.binding}, nil
}

// Callback approves the flow as subject "callback" when the binding matches.
func (s *stubOIDC) Callback(_ context.Context, state, binding, _, _ string, revision uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range s.flows {
		if f.state == state && !f.done && f.revision == revision {
			f.done = true
			if binding != f.binding {
				f.failed = true
				return oidcsignin.ErrRejected
			}
			f.who = oidcsignin.Identity{Subject: "callback", Display: "callback"}
			return nil
		}
	}
	return oidcsignin.ErrExpired
}
func (s *stubOIDC) Take(handle, binding string, revision uint64, purpose oidcsignin.Purpose, viewerID int64) (oidcsignin.Identity, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.flows[handle]
	if !ok || f.purpose != purpose || f.viewerID != viewerID || f.revision != revision {
		return oidcsignin.Identity{}, false, oidcsignin.ErrExpired
	}
	if binding != f.binding {
		return oidcsignin.Identity{}, false, oidcsignin.ErrRejected
	}
	if !f.done {
		return oidcsignin.Identity{}, false, nil
	}
	delete(s.flows, handle)
	if f.failed {
		return oidcsignin.Identity{}, true, oidcsignin.ErrRejected
	}
	return f.who, true, nil
}
func (s *stubOIDC) finish(handle, subject, display string) {
	s.finishAs(handle, oidcsignin.Identity{Subject: subject, Display: display})
}

func (s *stubOIDC) finishAs(handle string, who oidcsignin.Identity) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.flows[handle]
	f.done = true
	f.who = who
}

type oidcHarness struct {
	t       *testing.T
	h       http.Handler
	stub    *stubOIDC
	cfg     *config.Store
	local   *identity.Local
	db      *sql.DB
	users   *auth.Service
	browser *cookiejar.Jar
}

func newOIDCHarness(t *testing.T) *oidcHarness {
	t.Helper()
	db := store.OpenTest(t)
	ctx := context.Background()
	users, err := auth.New(ctx, db, auth.Options{})
	if err != nil {
		t.Fatal(err)
	}
	local, _ := identity.NewLocal(db)
	grants, _ := identity.NewGrants(db, users, time.Hour, nil)
	limiter, _ := identity.NewLoginLimiter(db, time.Minute, 100, 20, nil)
	poll, _ := identity.NewPollLimiter(db, time.Minute, 1000, 200, nil)
	cfg := newStore(t, func(c *config.Config) {
		c.OIDC = config.OIDC{Enabled: true, DisplayName: "Pocket ID", Issuer: "https://auth.example.test", ClientID: "ether", ClientSecret: "s3cret"}
	})
	stub := &stubOIDC{flows: map[string]*stubFlow{}}
	h := admin.NewHandler(admin.Deps{Config: cfg, LocalIdentity: local, IdentityGrants: grants, IdentityLimiter: limiter, PollLimiter: poll, RefreshViewers: users.RefreshViewers, OIDC: stub, LocalOwnerConfigured: func(ctx context.Context) bool { x, err := users.Owner(ctx); return err == nil && x.PlexID == 0 }})
	browser, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	x := &oidcHarness{t: t, h: h, stub: stub, cfg: cfg, local: local, db: db, users: users, browser: browser}
	if res := serve(h, req("POST", "/ether/api/setup/local", `{"username":"alice","password":"secret-pass1"}`)); res.Code != 200 {
		t.Fatalf("setup: %d %s", res.Code, res.Body)
	}
	if err := cfg.Update(func(c *config.Config) error { c.SetupComplete = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := local.CreateViewer(ctx, "bob", "secret-pass2"); err != nil {
		t.Fatal(err)
	}
	if err := users.RefreshViewers(ctx); err != nil {
		t.Fatal(err)
	}
	return x
}

const oidcBrowserOrigin = "https://192.168.1.10:32400"

func (x *oidcHarness) exchange(method, path, token, body string) *httptest.ResponseRecorder {
	if body == "" && method == "POST" {
		body = "{}"
	}
	r := req(method, oidcBrowserOrigin+path, body)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	for _, cookie := range x.browser.Cookies(r.URL) {
		r.AddCookie(cookie)
	}
	res := serve(x.h, r)
	x.browser.SetCookies(r.URL, res.Result().Cookies())
	return res
}

func (x *oidcHarness) call(method, path, token, body string) (int, map[string]any) {
	x.t.Helper()
	res := x.exchange(method, path, token, body)
	out := map[string]any{}
	if strings.HasPrefix(res.Header().Get("Content-Type"), "application/json") {
		decode(x.t, res, &out)
	}
	return res.Code, out
}

func oidcPollBody(handle, state string) string {
	return fmt.Sprintf(`{"handle":%q,"state":%q}`, handle, state)
}

func (x *oidcHarness) poll(start, token string, flow map[string]any) (int, map[string]any) {
	return x.call("POST", start+"/poll", token, oidcPollBody(flow["handle"].(string), flow["state"].(string)))
}

func (x *oidcHarness) portalLogin(user, pass string) string {
	x.t.Helper()
	code, out := x.call("POST", "/ether/api/me/login", "", fmt.Sprintf(`{"username":%q,"password":%q}`, user, pass))
	if code != 200 {
		x.t.Fatalf("portal login %s: %d %v", user, code, out)
	}
	return out["token"].(string)
}

// link runs the portal link flow for the signed-in viewer behind token.
func (x *oidcHarness) link(token, subject string) (int, map[string]any) {
	x.t.Helper()
	const start = "/ether/api/me/link/oidc"
	code, out := x.call("POST", start, token, "")
	if code != 200 {
		x.t.Fatalf("link start: %d %v", code, out)
	}
	handle := out["handle"].(string)
	if status, result := x.poll(start, token, out); status != 200 || result["done"] != false {
		x.t.Fatalf("pending link poll: %d %v", status, result)
	}
	x.stub.finish(handle, subject, subject+"-display")
	return x.poll(start, token, out)
}

// signIn runs a sign-in flow (owner console or portal) for subject.
func (x *oidcHarness) signIn(start, subject string) (int, map[string]any) {
	x.t.Helper()
	code, out := x.call("POST", start, "", "")
	if code != 200 {
		x.t.Fatalf("start %s: %d %v", start, code, out)
	}
	handle := out["handle"].(string)
	x.stub.finish(handle, subject, subject)
	return x.poll(start, "", out)
}

func TestOIDCSignInRequiresALinkAndOwnershipForTheConsole(t *testing.T) {
	x := newOIDCHarness(t)

	code, boot := x.call("GET", "/ether/api/bootstrap", "", "")
	methods := fmt.Sprint(boot["signInMethods"])
	if code != 200 || !strings.Contains(methods, "oidc") || boot["oidcName"] != "Pocket ID" {
		t.Fatalf("bootstrap = %d %v", code, boot)
	}

	// An unlinked provider account signs in nowhere.
	if code, out := x.signIn("/ether/api/me/oidc", "https://auth.example.test|alice"); code != 403 || out["error"] == nil {
		t.Fatalf("unlinked portal sign-in = %d %v", code, out)
	}
	if code, _ := x.signIn("/ether/api/auth/oidc", "https://auth.example.test|alice"); code != 403 {
		t.Fatalf("unlinked console sign-in = %d", code)
	}

	alice := x.portalLogin("alice", "secret-pass1")
	if code, out := x.link(alice, "https://auth.example.test|alice"); code != 200 || out["done"] != true {
		t.Fatalf("link alice = %d %v", code, out)
	}
	if !strings.HasSuffix(x.stub.redirect, "/ether/api/oidc/callback") {
		t.Fatalf("redirect URL = %q", x.stub.redirect)
	}
	bob := x.portalLogin("bob", "secret-pass2")
	// Bob cannot take over Alice's provider account.
	if code, out := x.link(bob, "https://auth.example.test|alice"); code != 409 {
		t.Fatalf("conflicting link = %d %v", code, out)
	}
	if code, out := x.link(bob, "https://auth.example.test|bob"); code != 200 {
		t.Fatalf("link bob = %d %v", code, out)
	}

	// The owner's provider account opens the console.
	code, out := x.signIn("/ether/api/auth/oidc", "https://auth.example.test|alice")
	if code != 200 || out["done"] != true {
		t.Fatalf("owner console sign-in = %d %v", code, out)
	}
	console := out["token"].(string)
	if code, _ := x.call("GET", "/ether/api/config", console, ""); code != 200 {
		t.Fatalf("console session rejected: %d", code)
	}
	// The console grant is not a portal grant.
	if code, _ := x.call("GET", "/ether/api/me", console, ""); code != 401 {
		t.Fatalf("console grant on portal = %d", code)
	}

	// A viewer's provider account opens the portal, never the console.
	if code, out := x.signIn("/ether/api/auth/oidc", "https://auth.example.test|bob"); code != 403 || out["error"] == nil {
		t.Fatalf("viewer console sign-in = %d %v", code, out)
	}
	code, out = x.signIn("/ether/api/me/oidc", "https://auth.example.test|bob")
	if code != 200 || out["done"] != true {
		t.Fatalf("viewer portal sign-in = %d %v", code, out)
	}
	portal := out["token"].(string)
	if code, me := x.call("GET", "/ether/api/me", portal, ""); code != 200 || me["username"] != "bob" {
		t.Fatalf("portal session = %d %v", code, me)
	}
	if code, _ := x.call("GET", "/ether/api/config", portal, ""); code != 401 {
		t.Fatalf("portal grant on console = %d", code)
	}

	// A handle is redeemed once and only on the route it was started for.
	code, out = x.call("POST", "/ether/api/me/oidc", "", "")
	handle := out["handle"].(string)
	x.stub.finish(handle, "https://auth.example.test|bob", "bob")
	if code, _ := x.poll("/ether/api/auth/oidc", "", out); code != 404 {
		t.Fatalf("portal handle on console = %d", code)
	}
	if code, _ := x.poll("/ether/api/me/oidc", "", out); code != 200 {
		t.Fatalf("portal handle = %d", code)
	}
	if code, _ := x.poll("/ether/api/me/oidc", "", out); code != 404 {
		t.Fatalf("redeemed handle = %d", code)
	}
}

func TestOIDCUnlinkAndSwitchOff(t *testing.T) {
	x := newOIDCHarness(t)
	bob := x.portalLogin("bob", "secret-pass2")
	if code, out := x.link(bob, "https://auth.example.test|bob"); code != 200 {
		t.Fatalf("link = %d %v", code, out)
	}
	code, signedIn := x.signIn("/ether/api/me/oidc", "https://auth.example.test|bob")
	if code != 200 {
		t.Fatalf("initial OIDC sign-in = %d %v", code, signedIn)
	}
	oidcToken := signedIn["token"].(string)
	code, out := x.call("GET", "/ether/api/me/identities", bob, "")
	if code != 200 {
		t.Fatalf("identities = %d", code)
	}
	var oidcID float64
	for _, item := range out["identities"].([]any) {
		m := item.(map[string]any)
		if m["provider"] == "oidc" {
			oidcID = m["id"].(float64)
			if m["display"] != "https://auth.example.test|bob-display" {
				t.Fatalf("display = %v", m["display"])
			}
		}
		if m["provider"] == "local" {
			// Unlink removes single sign-on links only.
			if code, _ := x.call("POST", "/ether/api/me/identities/unlink", bob, fmt.Sprintf(`{"id":%v}`, m["id"])); code != 404 {
				t.Fatalf("unlink local = %d", code)
			}
		}
	}
	if oidcID == 0 {
		t.Fatalf("no oidc identity in %v", out)
	}
	alice := x.portalLogin("alice", "secret-pass1")
	if code, _ := x.call("POST", "/ether/api/me/identities/unlink", alice, fmt.Sprintf(`{"id":%v}`, oidcID)); code != 404 {
		t.Fatalf("unlink someone else's identity = %d", code)
	}
	if code, _ := x.call("POST", "/ether/api/me/identities/unlink", bob, fmt.Sprintf(`{"id":%v}`, oidcID)); code != 200 {
		t.Fatalf("unlink = %d", code)
	}
	if code, _ := x.signIn("/ether/api/me/oidc", "https://auth.example.test|bob"); code != 403 {
		t.Fatalf("sign-in after unlink = %d", code)
	}
	if code, _ := x.call("GET", "/ether/api/me", oidcToken, ""); code != 401 {
		t.Fatalf("OIDC session after unlink = %d", code)
	}
	if code, _ := x.call("GET", "/ether/api/me", bob, ""); code != 200 {
		t.Fatalf("password session after unlink = %d", code)
	}

	// Switched off, nothing is advertised or started, and the secret stays redacted.
	if err := x.cfg.Update(func(c *config.Config) error { c.OIDC.Enabled = false; return nil }); err != nil {
		t.Fatal(err)
	}
	_, boot := x.call("GET", "/ether/api/bootstrap", "", "")
	if strings.Contains(fmt.Sprint(boot["signInMethods"]), "oidc") {
		t.Fatalf("disabled OIDC advertised: %v", boot)
	}
	if code, _ := x.call("POST", "/ether/api/me/oidc", "", ""); code != 503 {
		t.Fatalf("disabled start = %d", code)
	}
	code, owner := x.signInLocalOwner()
	if code != 200 {
		t.Fatal("owner login failed")
	}
	_, section := x.call("GET", "/ether/api/config/oidc", owner, "")
	if strings.Contains(fmt.Sprint(section), "s3cret") {
		t.Fatalf("client secret leaked: %v", section)
	}
}

func TestOIDCOwnerConfirmsLinkAfterPlexOwnershipTransfer(t *testing.T) {
	x := newOIDCHarness(t)
	ctx := t.Context()
	if err := x.users.SetOwner(ctx, plextv.Account{ID: 101, Username: "alice"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := x.db.ExecContext(ctx, `UPDATE auth_users SET plex_id=202 WHERE username='bob'`); err != nil {
		t.Fatal(err)
	}
	x.setOIDC(func(c *config.OIDC) { c.AutoLinkUsername = true })
	who := oidcsignin.Identity{Subject: "https://auth.example.test|bob", Username: "bob"}
	code, out := x.portalAs(who)
	if code != 200 {
		t.Fatalf("auto-link = %d %v", code, out)
	}
	priorGrant := out["token"].(string)
	if err := x.users.SetOwner(ctx, plextv.Account{ID: 202, Username: "bob"}, ""); err != nil {
		t.Fatal(err)
	}
	for _, surface := range []string{"/ether/api/me/oidc", "/ether/api/auth/oidc"} {
		if code, _ := x.signIn(surface, who.Subject); code != 403 {
			t.Fatalf("sign-in with an unconfirmed link at %s = %d", surface, code)
		}
	}
	if code, _ := x.call("GET", "/ether/api/me", priorGrant, ""); code != 401 {
		t.Fatalf("pre-transfer OIDC grant = %d", code)
	}
	bob := x.portalLogin("bob", "secret-pass2")
	if code, out := x.link(bob, who.Subject); code != 200 {
		t.Fatalf("owner confirmation = %d %v", code, out)
	}
	if code, _ := x.signIn("/ether/api/auth/oidc", who.Subject); code != 200 {
		t.Fatalf("owner sign-in after confirming the link = %d", code)
	}
	if code, _ := x.call("GET", "/ether/api/me", priorGrant, ""); code != 401 {
		t.Fatalf("pre-transfer OIDC grant after confirmation = %d", code)
	}
}

func TestOIDCConcurrentAutoLinkKeepsOneIdentity(t *testing.T) {
	x := newOIDCHarness(t)
	const issuer = "https://auth.example.test"
	match := identity.OIDCMatch{Issuer: issuer, Username: "bob"}
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, sub := range []string{"first", "second"} {
		go func() {
			<-start
			_, err := x.local.AutoLinkOIDC(t.Context(), match, oidcsignin.SubjectFor(issuer, sub), sub)
			results <- err
		}()
	}
	close(start)
	winners := 0
	for range 2 {
		if err := <-results; err == nil {
			winners++
		} else if !errors.Is(err, identity.ErrNoMatch) {
			t.Errorf("automatic link = %v", err)
		}
	}
	if winners != 1 {
		t.Fatalf("%d sign-ins linked to the viewer, want 1", winners)
	}
}

func TestOIDCProviderConfigurationInvalidatesPendingAndCompletedFlows(t *testing.T) {
	x := newOIDCHarness(t)
	const start = "/ether/api/me/oidc"
	if code, _ := x.link(x.portalLogin("bob", "secret-pass2"), "https://auth.example.test|bob"); code != 200 {
		t.Fatalf("link = %d", code)
	}
	if code, flow := x.call("POST", start, "", ""); code != 200 {
		t.Fatalf("start = %d %v", code, flow)
	} else {
		x.stub.finish(flow["handle"].(string), "https://auth.example.test|bob", "bob")
		x.setOIDC(func(c *config.OIDC) { c.Enabled = false })
		x.setOIDC(func(c *config.OIDC) { c.Enabled = true })
		if code, out := x.poll(start, "", flow); code != 404 || out["token"] != nil {
			t.Fatalf("completed flow after toggling sign-in = %d %v", code, out)
		}
	}
	code, flow := x.call("POST", start, "", "")
	if code != 200 {
		t.Fatalf("start = %d %v", code, flow)
	}
	x.setOIDC(func(c *config.OIDC) { c.Issuer = "https://other.example.test" })
	callback := "/ether/api/oidc/callback?code=c&state=" + flow["state"].(string)
	if res := x.exchange("GET", callback, "", ""); res.Code != 400 || !strings.Contains(res.Body.String(), "Sign-in expired") {
		t.Fatalf("callback after issuer change = %d %s", res.Code, res.Body)
	}
}

func (x *oidcHarness) signInLocalOwner() (int, string) {
	code, out := x.call("POST", "/ether/api/auth/local", "", `{"username":"alice","password":"secret-pass1"}`)
	token, _ := out["token"].(string)
	return code, token
}

func TestOIDCIssuerValidation(t *testing.T) {
	for issuer, ok := range map[string]bool{
		"https://auth.example.test":      true,
		"http://127.0.0.1:1411":          true,
		"http://localhost:1411":          true,
		"http://auth.example.test":       false,
		"auth.example.test":              false,
		"https://auth.example.test/?x=1": false,
	} {
		errs := admin.ValidateConfig(config.Config{OIDC: config.OIDC{Enabled: true, Issuer: issuer, ClientID: "ether"}})
		found := false
		for _, e := range errs {
			if e.Field == "oidc.issuer" {
				found = true
			}
		}
		if found == ok {
			t.Errorf("issuer %q: validation errors %v", issuer, errs)
		}
	}
	errs := admin.ValidateConfig(config.Config{OIDC: config.OIDC{Enabled: true, Issuer: "https://auth.example.test"}})
	found := false
	for _, e := range errs {
		found = found || e.Field == "oidc.clientId"
	}
	if !found {
		t.Errorf("missing client ID accepted: %v", errs)
	}
}

func TestOIDCCallbackRequiresTheStartingBrowser(t *testing.T) {
	x := newOIDCHarness(t)
	const startPath = "/ether/api/me/oidc"
	if res := serve(x.h, req("POST", startPath, "{}")); res.Code != 403 {
		t.Fatalf("insecure start = %d %s", res.Code, res.Body)
	}
	start := x.exchange("POST", startPath, "", "{}")
	if start.Code != 200 {
		t.Fatalf("start = %d %s", start.Code, start.Body)
	}
	var started struct {
		Handle  string `json:"handle"`
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	decode(t, start, &started)
	var binding *http.Cookie
	for _, c := range start.Result().Cookies() {
		if strings.HasPrefix(c.Name, "__Host-ether_oidc_") {
			binding = c
		}
	}
	if binding == nil || !binding.Secure || !binding.HttpOnly || binding.SameSite != http.SameSiteLaxMode ||
		binding.Path != "/" || binding.Domain != "" || binding.MaxAge <= 0 || binding.MaxAge > 600 {
		t.Fatalf("binding cookie = %+v", binding)
	}
	u, err := url.Parse(started.AuthURL)
	if err != nil || started.State == "" || u.Query().Get("state") != started.State {
		t.Fatalf("start state does not match the authorization URL: %v", err)
	}

	// The same link opened in a browser without the cookie fails, and the
	// flow ends: the starting browser cannot collect a sign-in either.
	forwarded := serve(x.h, req("GET", oidcBrowserOrigin+"/ether/api/oidc/callback?code=c&state="+started.State, ""))
	if forwarded.Code != 400 || !strings.Contains(forwarded.Body.String(), "Sign-in failed") {
		t.Fatalf("forwarded callback = %d %s", forwarded.Code, forwarded.Body)
	}
	if got := forwarded.Header().Get("Content-Security-Policy"); !strings.Contains(got, "default-src 'none'") {
		t.Fatalf("callback CSP = %q", got)
	}
	if code, _ := x.call("POST", startPath+"/poll", "", oidcPollBody(started.Handle, started.State)); code != 401 {
		t.Fatalf("poll after forwarded callback = %d", code)
	}

	// A successful callback leaves the cookie for the original browser's poll.
	start = x.exchange("POST", startPath, "", "{}")
	decode(t, start, &started)
	res := x.exchange("GET", "/ether/api/oidc/callback?code=c&state="+started.State, "", "")
	if res.Code != 200 || !strings.Contains(res.Body.String(), "Signed in") {
		t.Fatalf("callback = %d %s", res.Code, res.Body)
	}
	poll := oidcPollBody(started.Handle, started.State)
	unbound := serve(x.h, req("POST", oidcBrowserOrigin+startPath+"/poll", poll))
	if unbound.Code != 401 {
		t.Fatalf("poll without the original browser = %d", unbound.Code)
	}
	// The stub vouches for subject "callback", which nobody linked.
	res = x.exchange("POST", startPath+"/poll", "", poll)
	if res.Code != 403 {
		t.Fatalf("poll after callback = %d %s", res.Code, res.Body)
	}
	cleared := false
	for _, c := range res.Result().Cookies() {
		cleared = cleared || (strings.HasPrefix(c.Name, "__Host-ether_oidc_") && c.MaxAge < 0)
	}
	if !cleared {
		t.Fatal("poll left the binding cookie in place")
	}
}

// portalAs runs a portal sign-in for a provider account with profile claims.
func (x *oidcHarness) portalAs(who oidcsignin.Identity) (int, map[string]any) {
	x.t.Helper()
	const start = "/ether/api/me/oidc"
	code, out := x.call("POST", start, "", "")
	if code != 200 {
		x.t.Fatalf("start: %d %v", code, out)
	}
	handle := out["handle"].(string)
	x.stub.finishAs(handle, who)
	return x.poll(start, "", out)
}

func (x *oidcHarness) setOIDC(fn func(*config.OIDC)) {
	x.t.Helper()
	if err := x.cfg.Update(func(c *config.Config) error { fn(&c.OIDC); return nil }); err != nil {
		x.t.Fatal(err)
	}
}

func (x *oidcHarness) me(token string) string {
	x.t.Helper()
	code, me := x.call("GET", "/ether/api/me", token, "")
	if code != 200 {
		x.t.Fatalf("me = %d %v", code, me)
	}
	return fmt.Sprint(me["username"])
}

const iss = "https://auth.example.test"

func TestOIDCAutoLinkByEmail(t *testing.T) {
	x := newOIDCHarness(t)
	if _, err := x.db.Exec(`UPDATE auth_users SET email='Bob@Example.test' WHERE username='bob'`); err != nil {
		t.Fatal(err)
	}
	if _, err := x.db.Exec(`UPDATE auth_users SET email='alice@example.test' WHERE username='alice'`); err != nil {
		t.Fatal(err)
	}
	unverified := oidcsignin.Identity{Subject: iss + "|b1", Display: "bob", Email: "bob@example.test"}
	verified := unverified
	verified.EmailVerified = true

	// Off by default.
	if code, _ := x.portalAs(verified); code != 403 {
		t.Fatalf("auto-link while off = %d", code)
	}
	x.setOIDC(func(c *config.OIDC) { c.AutoLinkEmail = config.OIDCAutoLinkVerified })
	if code, _ := x.portalAs(unverified); code != 403 {
		t.Fatalf("unverified e-mail in verified mode = %d", code)
	}
	code, out := x.portalAs(verified)
	if code != 200 || x.me(out["token"].(string)) != "bob" {
		t.Fatalf("verified e-mail = %d %v", code, out)
	}
	// Now linked: the next sign-in needs no match.
	x.setOIDC(func(c *config.OIDC) { c.AutoLinkEmail = "" })
	if code, _ := x.portalAs(unverified); code != 200 {
		t.Fatalf("linked sign-in = %d", code)
	}
	// A second provider account with the same e-mail cannot take Bob over.
	x.setOIDC(func(c *config.OIDC) { c.AutoLinkEmail = config.OIDCAutoLinkAny })
	if code, _ := x.portalAs(oidcsignin.Identity{Subject: iss + "|b2", Email: "bob@example.test"}); code != 403 {
		t.Fatalf("second account took over a linked viewer = %d", code)
	}
	// The owner is never matched, even by an unverified e-mail in "any" mode.
	if code, _ := x.portalAs(oidcsignin.Identity{Subject: iss + "|a1", Email: "alice@example.test"}); code != 403 {
		t.Fatalf("owner auto-linked = %d", code)
	}
	// Auto-linking never runs from the console.
	code, out = x.call("POST", "/ether/api/auth/oidc", "", "")
	x.stub.finishAs(out["handle"].(string), oidcsignin.Identity{Subject: iss + "|a1", Email: "alice@example.test", EmailVerified: true})
	if code, _ := x.poll("/ether/api/auth/oidc", "", out); code != 403 {
		t.Fatalf("console auto-link = %d", code)
	}
}

func TestOIDCAutoLinkByUsernameAndGroup(t *testing.T) {
	x := newOIDCHarness(t)
	x.setOIDC(func(c *config.OIDC) { c.AutoLinkUsername = true; c.RequiredGroup = "ether" })
	bob := oidcsignin.Identity{Subject: iss + "|b1", Username: "BOB"}
	if code, _ := x.portalAs(bob); code != 403 {
		t.Fatalf("auto-link outside the required group = %d", code)
	}
	bob.Groups = []string{"family", "ether"}
	code, out := x.portalAs(bob)
	if code != 200 || x.me(out["token"].(string)) != "bob" {
		t.Fatalf("username auto-link = %d %v", code, out)
	}
	if code, _ := x.portalAs(oidcsignin.Identity{Subject: iss + "|a1", Username: "alice", Groups: []string{"ether"}}); code != 403 {
		t.Fatalf("owner auto-linked by username = %d", code)
	}
}

func TestOIDCAutoProvision(t *testing.T) {
	x := newOIDCHarness(t)
	carol := oidcsignin.Identity{Subject: iss + "|c1", Display: "carol", Username: "Carol", Name: "Carol C", Email: "carol@example.test"}
	if code, _ := x.portalAs(carol); code != 403 {
		t.Fatalf("provisioned while off = %d", code)
	}
	x.setOIDC(func(c *config.OIDC) { c.AutoProvision = true; c.AutoLinkUsername = true })
	code, out := x.portalAs(carol)
	if code != 200 || x.me(out["token"].(string)) != "carol" {
		t.Fatalf("provision = %d %v", code, out)
	}
	// The same account signs in to the same viewer again.
	code, out = x.portalAs(carol)
	if code != 200 || x.me(out["token"].(string)) != "carol" {
		t.Fatalf("second sign-in = %d %v", code, out)
	}
	// A different account named like an existing viewer it can't take over
	// (Bob matches by username, so it links; Alice is the owner, so a new
	// viewer is created instead with a unique name).
	code, out = x.portalAs(oidcsignin.Identity{Subject: iss + "|a2", Username: "alice"})
	if code != 200 {
		t.Fatalf("provision beside the owner = %d %v", code, out)
	}
	if name := x.me(out["token"].(string)); name != "alice2" {
		t.Fatalf("provisioned name = %q, want alice2", name)
	}
	if code, _ := x.call("GET", "/ether/api/config", out["token"].(string), ""); code != 401 {
		t.Fatalf("provisioned viewer reached the console = %d", code)
	}
	var stored string
	if err := x.db.QueryRow(`SELECT email FROM auth_users WHERE username='carol'`).Scan(&stored); err != nil || stored != "" {
		t.Fatalf("unverified e-mail stored: %q %v", stored, err)
	}
	// Created viewers don't need first-run sign-in setup.
	_, me := x.call("GET", "/ether/api/me", out["token"].(string), "")
	if me["needsSignInSetup"] != false {
		t.Fatalf("needsSignInSetup = %v", me["needsSignInSetup"])
	}
}

func TestOIDCAutoLinkEmailValidation(t *testing.T) {
	errs := admin.ValidateConfig(config.Config{OIDC: config.OIDC{AutoLinkEmail: "sometimes"}})
	found := false
	for _, e := range errs {
		found = found || e.Field == "oidc.autoLinkEmail"
	}
	if !found {
		t.Fatalf("invalid autoLinkEmail accepted: %v", errs)
	}
}
