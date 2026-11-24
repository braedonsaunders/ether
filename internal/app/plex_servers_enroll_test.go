package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/auth"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/plextv"
	"github.com/braedonsaunders/ether/internal/store"
)

// enrollRequest is one request the fake plex.tv received.
type enrollRequest struct {
	Method, Path, ClientID, Token, Product, Provides string
}

type enrollFakePIN struct {
	code, clientID, token string
}

// enrollFakePlex is a local plex.tv for PIN sign-in and account lookup. Its
// PIN responses never carry clientIdentifier, and a PIN answers only the
// client identifier that created it.
type enrollFakePlex struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	requests []enrollRequest
	pins     map[int64]*enrollFakePIN
	nextPIN  int64
	accounts map[string]int64 // account token -> plex id
	// noExpiry leaves expiresIn/expiresAt out of PIN responses.
	noExpiry bool
	// userStatus, when not 0, fails account lookups with that status.
	userStatus int
	// userGate, when set, holds account lookups until it is closed.
	userGate    chan struct{}
	userEntered chan struct{}
}

func newEnrollFakePlex(t *testing.T) *enrollFakePlex {
	t.Helper()
	f := &enrollFakePlex{t: t, pins: map[int64]*enrollFakePIN{}, accounts: map[string]int64{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *enrollFakePlex) serve(w http.ResponseWriter, r *http.Request) {
	clientID := r.Header.Get("X-Plex-Client-Identifier")
	f.mu.Lock()
	f.requests = append(f.requests, enrollRequest{Method: r.Method, Path: r.URL.Path, ClientID: clientID,
		Token: r.Header.Get("X-Plex-Token"), Product: r.Header.Get("X-Plex-Product"), Provides: r.Header.Get("X-Plex-Provides")})
	expiry := `,"expiresIn":1800`
	if f.noExpiry {
		expiry = ""
	}
	gate, entered, userStatus := f.userGate, f.userEntered, f.userStatus
	f.mu.Unlock()
	fail := func(status int, msg string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprintf(w, `{"errors":[{"code":1000,"message":%q}]}`, msg)
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/v2/pins":
		if clientID == "" {
			fail(http.StatusBadRequest, "client identifier required")
			return
		}
		f.mu.Lock()
		f.nextPIN++
		id := f.nextPIN
		p := &enrollFakePIN{code: "code" + strconv.FormatInt(id, 10), clientID: clientID}
		f.pins[id] = p
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"id":%d,"code":%q,"authToken":null%s}`, id, p.code, expiry)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v2/pins/"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/api/v2/pins/"), 10, 64)
		f.mu.Lock()
		p := f.pins[id]
		var code, owner, token string
		if p != nil {
			code, owner, token = p.code, p.clientID, p.token
		}
		f.mu.Unlock()
		if p == nil || code != r.URL.Query().Get("code") {
			fail(http.StatusNotFound, "Code not found or expired")
			return
		}
		if owner != clientID {
			fail(http.StatusBadRequest, "client identifier mismatch")
			return
		}
		tok := "null"
		if token != "" {
			tok = strconv.Quote(token)
		}
		fmt.Fprintf(w, `{"id":%d,"code":%q,"authToken":%s%s}`, id, code, tok, expiry)
	case r.Method == http.MethodGet && r.URL.Path == "/api/v2/user":
		if entered != nil {
			select {
			case entered <- struct{}{}:
			default:
			}
		}
		if gate != nil {
			<-gate
		}
		if userStatus != 0 {
			fail(userStatus, "account lookup refused")
			return
		}
		f.mu.Lock()
		acct, ok := f.accounts[r.Header.Get("X-Plex-Token")]
		f.mu.Unlock()
		if !ok {
			fail(http.StatusUnauthorized, "User could not be authenticated")
			return
		}
		fmt.Fprintf(w, `{"id":%d,"username":"user%d"}`, acct, acct)
	default:
		f.t.Errorf("unexpected fake plex.tv request %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}
}

// approve marks the PIN id approved by the account plexID.
func (f *enrollFakePlex) approve(id string, token string, plexID int64) {
	n, _ := strconv.ParseInt(id, 10, 64)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accounts[token] = plexID
	f.pins[n].token = token
}

func (f *enrollFakePlex) expirePIN(id string) {
	n, _ := strconv.ParseInt(id, 10, 64)
	f.mu.Lock()
	delete(f.pins, n)
	f.mu.Unlock()
}

func (f *enrollFakePlex) set(fn func()) {
	f.mu.Lock()
	fn()
	f.mu.Unlock()
}

// seen returns the requests whose path starts with prefix.
func (f *enrollFakePlex) seen(method, prefix string) []enrollRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []enrollRequest
	for _, r := range f.requests {
		if r.Method == method && strings.HasPrefix(r.Path, prefix) {
			out = append(out, r)
		}
	}
	return out
}

// onlyHost lets requests reach the fake plex.tv and refuses (and records)
// every other host, so no test request leaves the machine.
type onlyHost struct {
	host string
	mu   sync.Mutex
	// blocked lists the hosts refused.
	blocked []string
}

func (o *onlyHost) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != o.host {
		o.mu.Lock()
		o.blocked = append(o.blocked, r.URL.Host)
		o.mu.Unlock()
		return nil, fmt.Errorf("test: external request to %s blocked", r.URL.Host)
	}
	return http.DefaultTransport.RoundTrip(r)
}

type enrollEnv struct {
	app   *App
	fake  *enrollFakePlex
	admin plexExtrasAdmin
	dir   string
	// capacityChecks counts the capacity checks a new account asked for.
	capacityChecks *int
}

const (
	enrollMainClientID  = "main-setup-client-id"
	enrollMainMachineID = "main-machine-id"
)

func newEnrollEnv(t *testing.T) *enrollEnv {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	cfg, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Update(func(c *config.Config) error {
		c.Server.MachineIdentifier = enrollMainMachineID
		c.Plex.ClientIdentifier = enrollMainClientID
		c.Plex.Claimed, c.Plex.OwnerPlexID, c.Plex.OwnerUsername, c.Plex.Token = true, 1, "owner", "main-server-token"
		c.Plex.ExtraServersEnabled = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	users, err := auth.New(ctx, db, auth.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(users.Close)
	fake := newEnrollFakePlex(t)
	u, _ := url.Parse(fake.srv.URL)
	guard := &onlyHost{host: u.Host}
	hc := &http.Client{Transport: guard, Timeout: 10 * time.Second}
	t.Cleanup(func() {
		guard.mu.Lock()
		defer guard.mu.Unlock()
		if len(guard.blocked) != 0 {
			t.Errorf("external requests attempted: %v", guard.blocked)
		}
	})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	client, err := plextv.New(plextv.Options{BaseURL: fake.srv.URL, HTTPClient: hc, Identity: plextv.ServerIdentity(cfg.Get(), ""),
		Logger: log, RetryBackoff: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	registrar, err := plextv.NewRegistrar(plextv.RegistrarOptions{Config: cfg, Client: client, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	// plextvHTTP is what a started extra server would use: also guarded.
	a := &App{cfg: cfg, users: users, registrar: registrar, plextv: client, log: log, opts: Options{DataDir: dir, plextvHTTP: hc}}
	a.plexExtras = newPlexExtras(a)
	checks := 0
	a.plexExtras.afterAdd = func() { checks++ }
	return &enrollEnv{app: a, fake: fake, admin: newPlexExtrasAdmin(a), dir: dir, capacityChecks: &checks}
}

func (e *enrollEnv) start(t *testing.T) (id, setupID string) {
	t.Helper()
	id, code, authURL, err := e.admin.StartPIN(context.Background())
	if err != nil {
		t.Fatalf("StartPIN: %v", err)
	}
	_, frag, ok := strings.Cut(authURL, "#?")
	if !ok {
		t.Fatalf("auth URL %q has no fragment query", authURL)
	}
	q, err := url.ParseQuery(frag)
	if err != nil {
		t.Fatal(err)
	}
	if q.Get("code") != code {
		t.Fatalf("auth URL code = %q, want %q", q.Get("code"), code)
	}
	if got := q.Get("context[device][product]"); got != plextv.AppProduct {
		t.Fatalf("auth URL product = %q", got)
	}
	return id, q.Get("clientID")
}

func (e *enrollEnv) profiles() []config.PlexExtraServer { return e.app.cfg.Get().PlexExtraServers }

func (e *enrollEnv) pendingSizes() (pins, flows int) {
	e.admin.pending.mu.Lock()
	defer e.admin.pending.mu.Unlock()
	return len(e.admin.pending.pins), len(e.admin.pending.flows)
}

func TestExtraEnrollmentsUseTheirOwnPersistentSetupID(t *testing.T) {
	ctx := context.Background()
	e := newEnrollEnv(t)
	sharedBefore := e.app.registrar.Client().Identity()

	// Two sign-ins overlap: both are pending before either is approved.
	pinA, idA := e.start(t)
	pinB, idB := e.start(t)
	if idA == "" || idB == "" || idA == idB {
		t.Fatalf("setup ids not distinct: %q %q", idA, idB)
	}
	for _, id := range []string{idA, idB} {
		if id == enrollMainClientID || id == enrollMainMachineID {
			t.Fatalf("enrollment reused a main identifier: %q", id)
		}
	}
	created := e.fake.seen(http.MethodPost, "/api/v2/pins")
	if len(created) != 2 || created[0].ClientID != idA || created[1].ClientID != idB {
		t.Fatalf("PIN creation identifiers = %+v, want %q then %q", created, idA, idB)
	}
	if done, _, err := e.admin.PollPIN(ctx, pinA); done || err != nil {
		t.Fatalf("unapproved poll: done=%v err=%v", done, err)
	}

	// B finishes first, then A.
	e.fake.approve(pinB, "token-b", 202)
	e.fake.approve(pinA, "token-a", 201)
	for _, pin := range []string{pinB, pinA} {
		done, server, err := e.admin.PollPIN(ctx, pin)
		if err != nil || !done || server == nil {
			t.Fatalf("PollPIN(%s): done=%v err=%v", pin, done, err)
		}
	}

	// Each flow sent only its own identifier, with the setup product.
	want := map[string]string{"/api/v2/pins/" + pinA: idA, "/api/v2/pins/" + pinB: idB}
	for path, id := range want {
		polls := e.fake.seen(http.MethodGet, path)
		if len(polls) == 0 {
			t.Fatalf("no poll for %s", path)
		}
		for _, r := range polls {
			if r.Path == path && r.ClientID != id {
				t.Fatalf("poll %s sent identifier %q, want %q", path, r.ClientID, id)
			}
		}
	}
	lookups := e.fake.seen(http.MethodGet, "/api/v2/user")
	if len(lookups) != 2 {
		t.Fatalf("account lookups = %d, want 2", len(lookups))
	}
	for _, r := range lookups {
		switch {
		case r.Token == "token-a" && r.ClientID == idA:
		case r.Token == "token-b" && r.ClientID == idB:
		default:
			t.Fatalf("account lookup mixed identifier and token: %+v", r)
		}
	}
	e.fake.mu.Lock()
	for _, r := range e.fake.requests {
		if r.Product != plextv.AppProduct || r.Provides != "" {
			t.Errorf("enrollment request %s %s identified as %q provides %q", r.Method, r.Path, r.Product, r.Provides)
		}
	}
	e.fake.mu.Unlock()

	// The saved profiles keep the identifier their sign-in used.
	check := func(cfg config.Config) {
		t.Helper()
		if len(cfg.PlexExtraServers) != 2 {
			t.Fatalf("profiles = %d, want 2", len(cfg.PlexExtraServers))
		}
		byAccount := map[int64]config.PlexExtraServer{}
		for _, s := range cfg.PlexExtraServers {
			byAccount[s.Account.ID] = s
		}
		a, b := byAccount[201], byAccount[202]
		if a.ClientIdentifier != idA || b.ClientIdentifier != idB {
			t.Fatalf("saved setup ids = %q %q, want %q %q", a.ClientIdentifier, b.ClientIdentifier, idA, idB)
		}
		if a.Account.Token != "token-a" || b.Account.Token != "token-b" {
			t.Fatalf("saved account tokens = %q %q", a.Account.Token, b.Account.Token)
		}
		for _, s := range []config.PlexExtraServer{a, b} {
			if s.State != config.PlexServerStandby || s.Claimed || s.Token != "" {
				t.Fatalf("new profile is not a plain standby entry: %+v", s)
			}
			if len(s.MachineIdentifier) != 40 || s.MachineIdentifier == s.ClientIdentifier || s.MachineIdentifier == enrollMainMachineID {
				t.Fatalf("machine identifier %q is not a separate fresh id", s.MachineIdentifier)
			}
			// Server registration identifies by the machine identifier.
			reg := plextv.ServerIdentity(cfg.WithPlexServer(s), "")
			if reg.ClientIdentifier != s.MachineIdentifier || reg.Product != plextv.ServerProduct || reg.Provides != "server" {
				t.Fatalf("server registration identity = %+v", reg)
			}
		}
		if a.MachineIdentifier == b.MachineIdentifier {
			t.Fatal("profiles share a machine identifier")
		}
		if cfg.Plex.ClientIdentifier != enrollMainClientID || cfg.Server.MachineIdentifier != enrollMainMachineID ||
			cfg.Plex.Token != "main-server-token" || cfg.Plex.OwnerPlexID != 1 {
			t.Fatalf("main identity changed: %+v", cfg.Plex)
		}
	}
	check(e.app.cfg.Get())
	reloaded, err := config.Open(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	check(reloaded.Get())
	if !reflect.DeepEqual(reloaded.Get().PlexExtraServers, e.profiles()) {
		t.Fatal("profiles differ after reload")
	}

	// The shared client and the main sign-in identity are untouched.
	if got := e.app.registrar.Client().Identity(); got != sharedBefore {
		t.Fatalf("shared client identity changed: %+v", got)
	}
	if got := e.app.registrar.SignInClient().Identity().ClientIdentifier; got != enrollMainClientID {
		t.Fatalf("main sign-in identifier = %q", got)
	}
	if pins, flows := e.pendingSizes(); pins != 0 || flows != 0 {
		t.Fatalf("finished sign-ins retained: pins=%d flows=%d", pins, flows)
	}
	if *e.capacityChecks != 2 {
		t.Fatalf("capacity checks requested = %d, want 2", *e.capacityChecks)
	}
}

func TestMainSignInKeepsMainSetupID(t *testing.T) {
	ctx := context.Background()
	e := newEnrollEnv(t)
	// An extra enrollment in flight must not leak into the main flows.
	_, extraID := e.start(t)

	wizard := &plexAuth{app: e.app, pins: map[string]*plextv.PIN{}}
	_, _, wizardURL, err := wizard.StartPIN(ctx)
	if err != nil {
		t.Fatal(err)
	}
	login := &plexSignIn{app: e.app, pins: map[string]*plextv.PIN{}}
	_, _, loginURL, err := login.StartPIN(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{wizardURL, loginURL} {
		_, frag, _ := strings.Cut(raw, "#?")
		q, _ := url.ParseQuery(frag)
		if q.Get("clientID") != enrollMainClientID {
			t.Fatalf("main sign-in URL clientID = %q", q.Get("clientID"))
		}
	}
	created := e.fake.seen(http.MethodPost, "/api/v2/pins")
	if len(created) != 3 || created[0].ClientID != extraID || created[1].ClientID != enrollMainClientID || created[2].ClientID != enrollMainClientID {
		t.Fatalf("PIN creation identifiers = %+v", created)
	}
	if len(e.profiles()) != 0 {
		t.Fatal("main sign-in created a profile")
	}
}

func TestExtraEnrollmentConcurrentPollsStoreOneProfile(t *testing.T) {
	e := newEnrollEnv(t)
	pin, setupID := e.start(t)
	e.fake.approve(pin, "token-a", 201)
	gate, entered := make(chan struct{}), make(chan struct{}, 1)
	e.fake.set(func() { e.fake.userGate, e.fake.userEntered = gate, entered })

	const polls = 8
	type outcome struct {
		done   bool
		server any
		err    error
	}
	out := make([]outcome, polls)
	var wg sync.WaitGroup
	for i := range out {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			out[i].done, out[i].server, out[i].err = e.admin.PollPIN(ctx, pin)
		}()
	}
	// One poll is inside the account lookup; give the rest time to queue.
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("no account lookup started")
	}
	time.Sleep(100 * time.Millisecond)
	close(gate)
	wg.Wait()

	succeeded := 0
	for _, o := range out {
		switch {
		case o.err == nil && o.done:
			succeeded++
			if m, _ := o.server.(map[string]any); m["id"] != "p2" {
				t.Fatalf("poll reported server %v", o.server)
			}
		case errors.Is(o.err, errPlexEnrollmentUnknown):
			// It arrived after the sign-in finished.
		default:
			t.Fatalf("poll outcome done=%v err=%v", o.done, o.err)
		}
	}
	if succeeded == 0 {
		t.Fatal("no poll finished the sign-in")
	}
	got := e.profiles()
	if len(got) != 1 || got[0].ClientIdentifier != setupID || got[0].Account.ID != 201 {
		t.Fatalf("profiles after concurrent polls = %+v", got)
	}
	if n := len(e.fake.seen(http.MethodGet, "/api/v2/user")); n != 1 {
		t.Fatalf("account lookups = %d, want 1", n)
	}
	if *e.capacityChecks != 1 {
		t.Fatalf("capacity checks requested = %d, want 1", *e.capacityChecks)
	}
	// A later poll of the finished sign-in adds nothing.
	if done, _, err := e.admin.PollPIN(context.Background(), pin); done || !errors.Is(err, errPlexEnrollmentUnknown) {
		t.Fatalf("repeat poll: done=%v err=%v", done, err)
	}
	if len(e.profiles()) != 1 {
		t.Fatal("repeat poll added a profile")
	}
}

func TestExtraEnrollmentAccountLookupFailure(t *testing.T) {
	ctx := context.Background()
	e := newEnrollEnv(t)
	pin, setupID := e.start(t)
	e.fake.approve(pin, "token-a", 201)

	// A failing lookup stores nothing and keeps the sign-in for a retry.
	e.fake.set(func() { e.fake.userStatus = http.StatusServiceUnavailable })
	if done, _, err := e.admin.PollPIN(ctx, pin); done || err == nil {
		t.Fatalf("poll with failing lookup: done=%v err=%v", done, err)
	}
	if len(e.profiles()) != 0 {
		t.Fatal("failed lookup stored a profile")
	}
	if pins, flows := e.pendingSizes(); pins != 1 || flows != 1 {
		t.Fatalf("sign-in not retained for retry: pins=%d flows=%d", pins, flows)
	}
	e.fake.set(func() { e.fake.userStatus = 0 })
	if done, _, err := e.admin.PollPIN(ctx, pin); !done || err != nil {
		t.Fatalf("retry: done=%v err=%v", done, err)
	}
	got := e.profiles()
	if len(got) != 1 || got[0].ClientIdentifier != setupID {
		t.Fatalf("profile after retry = %+v", got)
	}
	for _, r := range e.fake.seen(http.MethodGet, "/api/v2/user") {
		if r.ClientID != setupID || r.Token != "token-a" {
			t.Fatalf("lookup sent %+v, want identifier %q", r, setupID)
		}
	}

	// A token plex.tv rejects ends the sign-in.
	pin2, _ := e.start(t)
	e.fake.approve(pin2, "token-b", 202)
	e.fake.set(func() { delete(e.fake.accounts, "token-b") })
	if done, _, err := e.admin.PollPIN(ctx, pin2); done || err == nil {
		t.Fatalf("rejected token: done=%v err=%v", done, err)
	}
	if pins, flows := e.pendingSizes(); pins != 0 || flows != 0 {
		t.Fatalf("rejected sign-in retained: pins=%d flows=%d", pins, flows)
	}
	if len(e.profiles()) != 1 {
		t.Fatal("rejected token changed the profiles")
	}
}

func TestExtraEnrollmentSaveFailureStoresNothing(t *testing.T) {
	ctx := context.Background()
	e := newEnrollEnv(t)
	pin, setupID := e.start(t)
	e.fake.approve(pin, "token-a", 201)

	// The config cannot be written while its directory is gone.
	if err := os.RemoveAll(e.dir); err != nil {
		t.Fatal(err)
	}
	if done, _, err := e.admin.PollPIN(ctx, pin); done || err == nil {
		t.Fatalf("poll with failing save: done=%v err=%v", done, err)
	}
	if len(e.profiles()) != 0 {
		t.Fatal("failed save left a profile in memory")
	}
	if *e.capacityChecks != 0 {
		t.Fatal("failed save asked for a capacity check")
	}
	if err := os.MkdirAll(e.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if done, _, err := e.admin.PollPIN(ctx, pin); !done || err != nil {
		t.Fatalf("retry after save failure: done=%v err=%v", done, err)
	}
	got := e.profiles()
	if len(got) != 1 || got[0].ClientIdentifier != setupID || got[0].ID != "p2" {
		t.Fatalf("profile after retry = %+v", got)
	}
}

func TestExtraEnrollmentRejectsAccountAlreadyInUse(t *testing.T) {
	ctx := context.Background()
	e := newEnrollEnv(t)

	first, _ := e.start(t)
	e.fake.approve(first, "token-a", 201)
	if done, _, err := e.admin.PollPIN(ctx, first); !done || err != nil {
		t.Fatalf("first profile: done=%v err=%v", done, err)
	}
	before := e.profiles()

	for name, plexID := range map[string]int64{"main owner": 1, "existing profile": 201} {
		pin, _ := e.start(t)
		e.fake.approve(pin, "token-dup-"+name, plexID)
		done, _, err := e.admin.PollPIN(ctx, pin)
		if done || !errors.Is(err, admin.ErrPlexAccountInUse) {
			t.Fatalf("%s: done=%v err=%v", name, done, err)
		}
		if pins, flows := e.pendingSizes(); pins != 0 || flows != 0 {
			t.Fatalf("%s: refused sign-in retained: pins=%d flows=%d", name, pins, flows)
		}
	}
	if !reflect.DeepEqual(e.profiles(), before) {
		t.Fatalf("existing profile changed: %+v", e.profiles())
	}
}

func TestExtraEnrollmentExpiryAndRetention(t *testing.T) {
	ctx := context.Background()
	e := newEnrollEnv(t)
	// Without an upstream expiry the sign-in still gets a deadline.
	e.fake.set(func() { e.fake.noExpiry = true })
	pin, _ := e.start(t)
	e.admin.pending.mu.Lock()
	flow := e.admin.pending.flows[pin]
	deadline := flow.pin.ExpiresAt
	e.admin.pending.mu.Unlock()
	if deadline.IsZero() || time.Until(deadline) > 10*time.Minute+time.Second {
		t.Fatalf("fallback expiry = %v", deadline)
	}

	// Past its deadline the sign-in is refused without asking plex.tv.
	e.fake.approve(pin, "token-a", 201)
	e.admin.pending.mu.Lock()
	flow.pin.ExpiresAt = time.Now().Add(-time.Second)
	e.admin.pending.mu.Unlock()
	polls := len(e.fake.seen(http.MethodGet, "/api/v2/pins/"))
	if done, _, err := e.admin.PollPIN(ctx, pin); done || !errors.Is(err, errPlexEnrollmentExpired) {
		t.Fatalf("expired poll: done=%v err=%v", done, err)
	}
	if len(e.fake.seen(http.MethodGet, "/api/v2/pins/")) != polls || len(e.profiles()) != 0 {
		t.Fatal("expired sign-in was polled or stored")
	}
	if pins, flows := e.pendingSizes(); pins != 0 || flows != 0 {
		t.Fatalf("expired sign-in retained: pins=%d flows=%d", pins, flows)
	}
	if done, _, err := e.admin.PollPIN(ctx, pin); done || !errors.Is(err, errPlexEnrollmentUnknown) {
		t.Fatalf("poll after expiry: done=%v err=%v", done, err)
	}

	// A PIN plex.tv deleted counts as expired.
	gone, _ := e.start(t)
	e.fake.expirePIN(gone)
	if done, _, err := e.admin.PollPIN(ctx, gone); done || !errors.Is(err, errPlexEnrollmentExpired) {
		t.Fatalf("deleted PIN: done=%v err=%v", done, err)
	}
	if pins, flows := e.pendingSizes(); pins != 0 || flows != 0 {
		t.Fatalf("deleted PIN retained: pins=%d flows=%d", pins, flows)
	}

	// An expired sign-in is pruned, PIN and identifier together, by the next.
	stale, _ := e.start(t)
	e.admin.pending.mu.Lock()
	e.admin.pending.flows[stale].pin.ExpiresAt = time.Now().Add(-time.Second)
	e.admin.pending.mu.Unlock()
	fresh, _ := e.start(t)
	e.admin.pending.mu.Lock()
	_, stalePIN := e.admin.pending.pins[stale]
	_, staleFlow := e.admin.pending.flows[stale]
	_, freshFlow := e.admin.pending.flows[fresh]
	e.admin.pending.mu.Unlock()
	if stalePIN || staleFlow || !freshFlow {
		t.Fatalf("pruning: stale pin=%v flow=%v, fresh flow=%v", stalePIN, staleFlow, freshFlow)
	}

	// Abandoned sign-ins are bounded; every one kept has its own identifier.
	var last string
	for i := 0; i < 300; i++ {
		last, _ = e.start(t)
	}
	e.admin.pending.mu.Lock()
	defer e.admin.pending.mu.Unlock()
	if len(e.admin.pending.pins) != 256 || len(e.admin.pending.flows) != 256 {
		t.Fatalf("retained pins=%d flows=%d, want 256", len(e.admin.pending.pins), len(e.admin.pending.flows))
	}
	ids := map[string]bool{}
	for id, f := range e.admin.pending.flows {
		if e.admin.pending.pins[id] != f.pin {
			t.Fatalf("sign-in %s lost its PIN", id)
		}
		if ids[f.clientID] {
			t.Fatalf("setup identifier %q reused", f.clientID)
		}
		ids[f.clientID] = true
	}
	if e.admin.pending.flows[last] == nil {
		t.Fatal("newest sign-in was evicted")
	}
}

func TestExtraEnrollmentNeverStartsOnItsOwn(t *testing.T) {
	ctx := context.Background()
	e := newEnrollEnv(t)
	pin, _ := e.start(t)
	e.fake.approve(pin, "token-a", 201)
	if done, _, err := e.admin.PollPIN(ctx, pin); !done || err != nil {
		t.Fatalf("PollPIN: done=%v err=%v", done, err)
	}
	seen := len(e.fake.seen(http.MethodGet, "/")) + len(e.fake.seen(http.MethodPost, "/"))
	// A new account waits for the owner: no background step claims or
	// publishes it, and plex.tv is not contacted again (the env blocks
	// other hosts). The main server being full changes nothing.
	e.app.plexExtras.noteSeats("", 1000)
	time.Sleep(50 * time.Millisecond)
	if got := len(e.fake.seen(http.MethodGet, "/")) + len(e.fake.seen(http.MethodPost, "/")); got != seen {
		t.Fatalf("a full main server made %d plex.tv requests", got-seen)
	}
	got := e.profiles()
	if len(got) != 1 || got[0].State != config.PlexServerStandby || got[0].Claimed {
		t.Fatalf("the new account started on its own: %+v", got)
	}
}

// Extra Plex servers are off by default: no sign-in, no start, and requests
// stay on the main server.
func TestExtraServersOffRefusesSignInAndStart(t *testing.T) {
	ctx := context.Background()
	e := newEnrollEnv(t)
	pin, _ := e.start(t)
	e.fake.approve(pin, "token-a", 201)
	if done, _, err := e.admin.PollPIN(ctx, pin); !done || err != nil {
		t.Fatalf("PollPIN: done=%v err=%v", done, err)
	}
	if err := e.app.cfg.Update(func(c *config.Config) error {
		c.Plex.ExtraServersEnabled = false
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := e.admin.StartPIN(ctx); !errors.Is(err, errPlexExtrasOff) {
		t.Fatalf("StartPIN while off: %v", err)
	}
	id := e.profiles()[0].ID
	if err := e.admin.Start(ctx, id); !errors.Is(err, errPlexExtrasOff) {
		t.Fatalf("Start while off: %v", err)
	}
	if got := e.profiles(); len(got) != 1 || got[0].State != config.PlexServerStandby {
		t.Fatalf("profile changed while off: %+v", got)
	}
	cfg := e.app.cfg.Get()
	if !e.app.plexExtras.takesInvites(cfg, "") || e.app.plexExtras.takesInvites(cfg, id) {
		t.Fatal("an extra server takes invites while the feature is off")
	}
	if config.DefaultMaxExtraPlexServers != 2 || (config.Config{}).PlexMaxExtraServers() != 2 {
		t.Fatal("the default limit is not 2")
	}
}

// The limit holds for a new sign-in and for sign-ins already in flight.
func TestExtraServersLimit(t *testing.T) {
	ctx := context.Background()
	e := newEnrollEnv(t)
	if err := e.app.cfg.Update(func(c *config.Config) error {
		c.Plex.MaxExtraServers = 1
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Two sign-ins start while there is still room for one.
	pinA, _ := e.start(t)
	pinB, _ := e.start(t)
	e.fake.approve(pinA, "token-a", 201)
	e.fake.approve(pinB, "token-b", 202)
	if done, _, err := e.admin.PollPIN(ctx, pinA); !done || err != nil {
		t.Fatalf("PollPIN A: done=%v err=%v", done, err)
	}
	var limit errPlexExtrasLimit
	if _, _, err := e.admin.PollPIN(ctx, pinB); !errors.As(err, &limit) || limit.max != 1 {
		t.Fatalf("PollPIN B over the limit: %v", err)
	}
	if got := e.profiles(); len(got) != 1 || got[0].Account.ID != 201 {
		t.Fatalf("profiles = %+v, want only account 201", got)
	}
	if _, _, _, err := e.admin.StartPIN(ctx); !errors.As(err, &limit) {
		t.Fatalf("StartPIN at the limit: %v", err)
	}
}
