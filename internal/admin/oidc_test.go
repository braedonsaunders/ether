package admin_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/auth"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/oidcsignin"
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
	done     bool
	who      oidcsignin.Identity
}

func (s *stubOIDC) Check(context.Context, oidcsignin.Settings) error { return nil }
func (s *stubOIDC) Start(_ context.Context, _ oidcsignin.Settings, redirectURL string, purpose oidcsignin.Purpose, viewerID int64) (oidcsignin.Started, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	handle := fmt.Sprintf("handle-%d", s.n)
	f := &stubFlow{state: fmt.Sprintf("state-%d-0123456789abcdef", s.n), binding: fmt.Sprintf("binding-%d", s.n), purpose: purpose, viewerID: viewerID}
	s.flows[handle] = f
	s.redirect = redirectURL
	return oidcsignin.Started{Handle: handle, AuthURL: "https://auth.example.test/authorize?state=" + f.state, State: f.state, Binding: f.binding}, nil
}

// Callback approves the flow as subject "callback" when the binding matches.
func (s *stubOIDC) Callback(_ context.Context, state, binding, _, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range s.flows {
		if f.state == state && !f.done {
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
func (s *stubOIDC) Take(handle string, purpose oidcsignin.Purpose, viewerID int64) (oidcsignin.Identity, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.flows[handle]
	if !ok || f.purpose != purpose || (purpose == oidcsignin.PurposeLink && f.viewerID != viewerID) {
		return oidcsignin.Identity{}, false, oidcsignin.ErrExpired
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
	t     *testing.T
	h     http.Handler
	stub  *stubOIDC
	cfg   *config.Store
	local *identity.Local
	db    *sql.DB
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
	poll, _ := identity.NewLoginLimiter(db, time.Minute, 1000, 200, nil)
	cfg := newStore(t, func(c *config.Config) {
		c.OIDC = config.OIDC{Enabled: true, DisplayName: "Pocket ID", Issuer: "https://auth.example.test", ClientID: "ether", ClientSecret: "s3cret"}
	})
	stub := &stubOIDC{flows: map[string]*stubFlow{}}
	h := admin.NewHandler(admin.Deps{Config: cfg, LocalIdentity: local, IdentityGrants: grants, IdentityLimiter: limiter, PollLimiter: poll, RefreshViewers: users.RefreshViewers, OIDC: stub, LocalOwnerConfigured: func(ctx context.Context) bool { x, err := users.Owner(ctx); return err == nil && x.PlexID == 0 }})
	x := &oidcHarness{t: t, h: h, stub: stub, cfg: cfg, local: local, db: db}
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

func (x *oidcHarness) call(method, path, token, body string) (int, map[string]any) {
	x.t.Helper()
	r := req(method, path, body)
	if body == "" && method == "POST" {
		r = req(method, path, "{}")
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	res := serve(x.h, r)
	out := map[string]any{}
	if strings.HasPrefix(res.Header().Get("Content-Type"), "application/json") {
		decode(x.t, res, &out)
	}
	return res.Code, out
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
	code, out := x.call("POST", "/ether/api/me/link/oidc", token, "")
	if code != 200 {
		x.t.Fatalf("link start: %d %v", code, out)
	}
	handle := out["handle"].(string)
	if code, out = x.call("GET", "/ether/api/me/link/oidc/"+handle, token, ""); code != 200 || out["done"] != false {
		x.t.Fatalf("pending link poll: %d %v", code, out)
	}
	x.stub.finish(handle, subject, subject+"-display")
	return x.call("GET", "/ether/api/me/link/oidc/"+handle, token, "")
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
	return x.call("GET", start+"/"+handle, "", "")
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
	if code, _ := x.call("GET", "/ether/api/auth/oidc/"+handle, "", ""); code != 404 {
		t.Fatalf("portal handle on console = %d", code)
	}
	if code, _ := x.call("GET", "/ether/api/me/oidc/"+handle, "", ""); code != 200 {
		t.Fatalf("portal handle = %d", code)
	}
	if code, _ := x.call("GET", "/ether/api/me/oidc/"+handle, "", ""); code != 404 {
		t.Fatalf("redeemed handle = %d", code)
	}
}

func TestOIDCUnlinkAndSwitchOff(t *testing.T) {
	x := newOIDCHarness(t)
	bob := x.portalLogin("bob", "secret-pass2")
	if code, out := x.link(bob, "https://auth.example.test|bob"); code != 200 {
		t.Fatalf("link = %d %v", code, out)
	}
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
	start := serve(x.h, req("POST", "/ether/api/me/oidc", "{}"))
	if start.Code != 200 {
		t.Fatalf("start = %d %s", start.Code, start.Body)
	}
	var started struct {
		Handle  string `json:"handle"`
		AuthURL string `json:"authUrl"`
	}
	decode(t, start, &started)
	var binding *http.Cookie
	for _, c := range start.Result().Cookies() {
		if strings.HasPrefix(c.Name, "ether_oidc_") {
			binding = c
		}
	}
	if binding == nil || !binding.HttpOnly || binding.SameSite != http.SameSiteLaxMode ||
		binding.Path != "/ether/api/oidc/callback" || binding.MaxAge <= 0 || binding.MaxAge > 600 {
		t.Fatalf("binding cookie = %+v", binding)
	}
	state := strings.TrimPrefix(started.AuthURL, "https://auth.example.test/authorize?state=")

	// The same link opened in a browser without the cookie fails, and the
	// flow ends: the starting browser cannot collect a sign-in either.
	forwarded := serve(x.h, req("GET", "/ether/api/oidc/callback?code=c&state="+state, ""))
	if forwarded.Code != 400 || !strings.Contains(forwarded.Body.String(), "Sign-in failed") {
		t.Fatalf("forwarded callback = %d %s", forwarded.Code, forwarded.Body)
	}
	if got := forwarded.Header().Get("Content-Security-Policy"); !strings.Contains(got, "default-src 'none'") {
		t.Fatalf("callback CSP = %q", got)
	}
	if code, _ := x.call("GET", "/ether/api/me/oidc/"+started.Handle, "", ""); code != 401 {
		t.Fatalf("poll after forwarded callback = %d", code)
	}

	// From the starting browser the callback succeeds and clears the cookie.
	start = serve(x.h, req("POST", "/ether/api/me/oidc", "{}"))
	decode(t, start, &started)
	state = strings.TrimPrefix(started.AuthURL, "https://auth.example.test/authorize?state=")
	cb := req("GET", "/ether/api/oidc/callback?code=c&state="+state, "")
	for _, c := range start.Result().Cookies() {
		cb.AddCookie(c)
	}
	res := serve(x.h, cb)
	if res.Code != 200 || !strings.Contains(res.Body.String(), "Signed in") {
		t.Fatalf("callback = %d %s", res.Code, res.Body)
	}
	cleared := false
	for _, c := range res.Result().Cookies() {
		cleared = cleared || (strings.HasPrefix(c.Name, "ether_oidc_") && c.MaxAge < 0)
	}
	if !cleared {
		t.Fatal("callback left the binding cookie in place")
	}
	// The stub vouches for subject "callback", which nobody linked.
	if code, out := x.call("GET", "/ether/api/me/oidc/"+started.Handle, "", ""); code != 403 || fmt.Sprint(out["error"]) == "" {
		t.Fatalf("poll after callback = %d %v", code, out)
	}
}

// portalAs runs a portal sign-in for a provider account with profile claims.
func (x *oidcHarness) portalAs(who oidcsignin.Identity) (int, map[string]any) {
	x.t.Helper()
	code, out := x.call("POST", "/ether/api/me/oidc", "", "")
	if code != 200 {
		x.t.Fatalf("start: %d %v", code, out)
	}
	handle := out["handle"].(string)
	x.stub.finishAs(handle, who)
	return x.call("GET", "/ether/api/me/oidc/"+handle, "", "")
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
	if code, _ := x.call("GET", "/ether/api/auth/oidc/"+out["handle"].(string), "", ""); code != 403 {
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
