package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/backup"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/plextv"
	"github.com/braedonsaunders/ether/internal/startup"
)

// recovery is the API a failed start offers on a standalone server, so the
// owner can restore a backup (or undo the last restore) from the status
// page when the database will not open. Nothing that can check a console
// session is running, so the owner proves who they are with:
//
//   - their local owner sign-in, checked against the mirror kept in the
//     data directory (ownerrecovery.go) or, without one, the database
//     opened read-only;
//   - a Plex sign-in, accepted only for the account that owns the server;
//   - a one-time recovery code, written to recovery-code.txt in the data
//     directory and to the log while the start is failing (whoever can
//     read those can replace the data directory anyway). Requests that
//     came through a reverse proxy cannot use it.
//
// Each yields a short-lived recovery token that only these endpoints
// accept. Before setup completes the local network has access, as
// everywhere else. Settings that cannot be read count as a finished setup.
//
// One recovery serves the whole of Serve: sign-ins survive retries, the
// retry waits while the owner is using the page or a restore is being
// prepared, and a restore cannot be requested once a start has begun.
//
// Mounted at startup.RecoveryPrefix (LAN-only, like the console):
//
//	GET  access                 which sign-ins are available; granted
//	POST local                  {username, password} → {token}
//	POST code                   {code} → {token}
//	POST pin, GET pin/{handle}  Plex sign-in → {token}
//	GET  backups                bundles kept here, and the last restore
//	POST restore/snapshot/{n}   restore one and start again
//	POST restore                an uploaded encrypted bundle (password in
//	                            X-Ether-Backup-Password, base64)
//	POST restore/undo           put back what the last restore replaced
type recovery struct {
	dataDir string
	front   *frontDoor
	log     *slog.Logger
	now     func() time.Time
	// plexFor builds the plex.tv sign-in for the settings (a test seam).
	plexFor func(nodeFacts) recoveryPlex

	mu     sync.Mutex
	pins   map[string]*plextv.PIN
	tokens map[string]time.Time
	// limits holds recent sign-in attempts per client and per username,
	// failures every failed sign-in from anyone (see admit); verifying
	// bounds concurrent Argon2id checks.
	limits    map[string][]time.Time
	failures  []time.Time
	verifying chan struct{}
	code      string
	// attempting is set while a start runs and ready once one succeeded;
	// busy counts restores being prepared; activeUntil holds the retry
	// while a signed-in owner uses the page.
	attempting  bool
	ready       bool
	busy        int
	activeUntil time.Time
}

// recoveryPlex is the plex.tv sign-in recovery uses (a *plextv.Client).
type recoveryPlex interface {
	CreatePIN(ctx context.Context) (*plextv.PIN, error)
	AuthURL(pin *plextv.PIN, forwardURL string) string
	CheckPIN(ctx context.Context, id int64, code string) (*plextv.PIN, error)
	User(ctx context.Context, token string) (*plextv.Account, error)
}

const (
	recoveryTokenHeader = "X-Ether-Recovery-Token"
	recoveryTokenTTL    = 15 * time.Minute
	// recoveryCodeFile holds the one-time recovery code while a start fails.
	recoveryCodeFile = "recovery-code.txt"
	// recoveryHold is how long the page's last use keeps a retry waiting.
	recoveryHold = 2 * time.Minute
	// Sign-in attempts within limitWindow: limitPerIP per client (an IPv4
	// address or an IPv6 /64), limitPerName per username, and
	// limitFailures failures from all clients together. At most limitKeys
	// clients and names are tracked, and maxVerifies Argon2id checks (about
	// 19 MB each) run at once.
	limitWindow   = 15 * time.Minute
	limitPerIP    = 20
	limitPerName  = 5
	limitFailures = 100
	limitKeys     = 4096
	maxVerifies   = 2
	maxUsername   = 128
)

func newRecovery(dataDir string, front *frontDoor, log *slog.Logger) *recovery {
	return &recovery{
		dataDir: dataDir, front: front, log: log, now: time.Now,
		plexFor: func(f nodeFacts) recoveryPlex {
			if f.ClientIdentifier == "" {
				return nil
			}
			cfg := config.Config{Server: config.Server{FriendlyName: f.Server.FriendlyName}}
			cfg.Plex.ClientIdentifier = f.ClientIdentifier
			c, err := plextv.New(plextv.Options{Identity: plextv.AppIdentity(cfg, ""), Logger: log})
			if err != nil {
				return nil
			}
			return c
		},
		pins: map[string]*plextv.PIN{}, tokens: map[string]time.Time{}, limits: map[string][]time.Time{},
		verifying: make(chan struct{}, maxVerifies),
	}
}

func (rc *recovery) facts() nodeFacts { return readNodeFacts(rc.dataDir, os.Getenv) }

// ---- Coupling with Serve's retries ---------------------------------------------

// beginAttempt waits until no restore is being prepared, then marks a
// start as running: restores requested from now on are refused.
func (rc *recovery) beginAttempt(ctx context.Context) error {
	for {
		rc.mu.Lock()
		if rc.busy == 0 {
			rc.attempting = true
			rc.mu.Unlock()
			return nil
		}
		rc.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// endAttempt records how a start ended. Once one succeeded, restores are
// refused even from a request that reached the page before (sticky until
// a later start fails).
func (rc *recovery) endAttempt(started bool) {
	rc.mu.Lock()
	rc.attempting, rc.ready = false, started
	rc.mu.Unlock()
}

// touch records that a signed-in owner is using the page.
func (rc *recovery) touch() {
	rc.mu.Lock()
	rc.activeUntil = rc.now().Add(recoveryHold)
	rc.mu.Unlock()
}

func (rc *recovery) holding() bool {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.busy > 0 || rc.now().Before(rc.activeUntil)
}

// recoveryPoll is how often a held retry checks whether the page is idle.
var recoveryPoll = 5 * time.Second

// wait waits out a failed start's backoff, longer while the owner is using
// the page, and returns early when a restore was requested. It reports
// false when ctx ended.
func (rc *recovery) wait(ctx context.Context, d time.Duration) bool {
	// A wake-up left over from an earlier wait must not skip this one.
	select {
	case <-rc.front.retry:
	default:
	}
	t := time.NewTimer(d)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-rc.front.retry:
			return true
		case <-t.C:
			if !rc.holding() {
				return true
			}
			rc.front.tracker.Postpone(recoveryPoll)
			t.Reset(recoveryPoll)
		}
	}
}

// prepare makes sure a recovery code exists while a start is failing.
func (rc *recovery) prepare() {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.code == "" {
		rc.newCodeLocked()
	}
}

// clear forgets the code once the server is ready.
func (rc *recovery) clear() {
	rc.mu.Lock()
	rc.code = ""
	rc.mu.Unlock()
	_ = os.Remove(filepath.Join(rc.dataDir, recoveryCodeFile))
}

// codeAlphabet avoids look-alike characters (0/O, 1/I/L).
const codeAlphabet = "23456789ABCDEFGHJKMNPQRSTUVWXYZ"

func (rc *recovery) newCodeLocked() {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	var sb strings.Builder
	for i, c := range b {
		if i > 0 && i%4 == 0 {
			sb.WriteByte('-')
		}
		sb.WriteByte(codeAlphabet[int(c)%len(codeAlphabet)])
	}
	rc.code = sb.String()
	path := filepath.Join(rc.dataDir, recoveryCodeFile)
	body := "Ether recovery code (valid once, while Ether cannot start):\n" + rc.code + "\n"
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(body), 0o600); err == nil {
		_ = os.Rename(tmp, path)
	}
	rc.log.Warn("To restore a backup from the startup page, enter this one-time recovery code", "recovery_code", rc.code, "file", path)
}

// ---- Access -------------------------------------------------------------------

func (rc *recovery) handler() http.Handler {
	mux := http.NewServeMux()
	prefix := strings.TrimSuffix(startup.RecoveryPrefix, "/")
	mux.HandleFunc("GET "+prefix+"/access", rc.access)
	mux.HandleFunc("POST "+prefix+"/local", rc.mutating(rc.localSignIn))
	mux.HandleFunc("POST "+prefix+"/code", rc.mutating(rc.codeSignIn))
	mux.HandleFunc("POST "+prefix+"/pin", rc.mutating(rc.startPIN))
	mux.HandleFunc("GET "+prefix+"/pin/{handle}", rc.pollPIN)
	mux.HandleFunc("GET "+prefix+"/backups", rc.authed(rc.backups))
	mux.HandleFunc("POST "+prefix+"/restore/snapshot/{name}", rc.mutating(rc.authed(rc.restoreSnapshot)))
	mux.HandleFunc("POST "+prefix+"/restore/undo", rc.mutating(rc.authed(rc.restoreUndo)))
	mux.HandleFunc("POST "+prefix+"/restore", rc.authed(rc.restoreUpload))
	return mux
}

func recoveryJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func recoveryError(w http.ResponseWriter, code int, kind, msg string) {
	recoveryJSON(w, code, map[string]any{"error": map[string]string{"code": kind, "message": msg}})
}

// mutating requires the console's CSRF header (and, with a body, JSON).
func (rc *recovery) mutating(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ct := strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0])
		if r.Header.Get("X-Ether-Request") != "1" || (r.ContentLength > 0 && !strings.EqualFold(ct, "application/json")) {
			recoveryError(w, http.StatusForbidden, "csrf", "mutating requests require the X-Ether-Request: 1 header and a JSON content type")
			return
		}
		next(w, r)
	}
}

// open reports whether r may restore without signing in: only on the
// local network before a setup known to be unfinished, and never once a
// local owner exists (as the console stops tokenless access then).
func (rc *recovery) open(r *http.Request, f nodeFacts) bool {
	if !f.Readable || f.SetupComplete {
		return false
	}
	if _, ok := readOwnerMirror(rc.dataDir); ok {
		return false
	}
	return admin.SetupOpen(false, r, identity.ProxyPrefixes(f.Server.TrustedProxyCIDRs)...)
}

// authed admits a valid recovery token, or an open setup.
func (rc *recovery) authed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f := rc.facts()
		if !f.Standalone() {
			recoveryError(w, http.StatusConflict, "guided_restore", "this server's database cannot be restored from here")
			return
		}
		if rc.validToken(r.Header.Get(recoveryTokenHeader)) || rc.open(r, f) {
			rc.touch()
			next(w, r)
			return
		}
		recoveryError(w, http.StatusUnauthorized, "unauthorized", "sign in as the owner to restore")
	}
}

func (rc *recovery) validToken(token string) bool {
	if token == "" {
		return false
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	now := rc.now()
	ok := false
	for t, exp := range rc.tokens {
		if now.After(exp) {
			delete(rc.tokens, t)
			continue
		}
		if subtle.ConstantTimeCompare([]byte(t), []byte(token)) == 1 {
			ok = true
		}
	}
	return ok
}

func (rc *recovery) issueToken() string {
	token := "ether-recovery-" + rand.Text()
	rc.mu.Lock()
	rc.tokens[token] = rc.now().Add(recoveryTokenTTL)
	rc.activeUntil = rc.now().Add(recoveryHold)
	rc.mu.Unlock()
	return token
}

// admit records a sign-in attempt from client (and for username, when
// set) and reports whether it may proceed. The client's limit is checked
// first and nothing is recorded once it is exceeded; so is the limit on
// failures from everyone, and the number of tracked keys is capped.
func (rc *recovery) admit(client, username string) bool {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	now := rc.now()
	rc.pruneLocked(now)
	if len(rc.failures) >= limitFailures {
		return false
	}
	ipKey, nameKey := "ip:"+client, ""
	if username != "" {
		nameKey = "name:" + username
	}
	if len(rc.limits[ipKey]) >= limitPerIP {
		return false
	}
	newKeys := 0
	for _, k := range []string{ipKey, nameKey} {
		if _, ok := rc.limits[k]; k != "" && !ok {
			newKeys++
		}
	}
	if len(rc.limits)+newKeys > limitKeys {
		return false
	}
	rc.limits[ipKey] = append(rc.limits[ipKey], now)
	if nameKey == "" {
		return true
	}
	if len(rc.limits[nameKey]) >= limitPerName {
		return false
	}
	rc.limits[nameKey] = append(rc.limits[nameKey], now)
	return true
}

// failed records a failed sign-in for the limit on everyone.
func (rc *recovery) failed() {
	rc.mu.Lock()
	rc.failures = append(rc.failures, rc.now())
	rc.mu.Unlock()
}

// pruneLocked drops attempts older than the window, and keys left empty.
func (rc *recovery) pruneLocked(now time.Time) {
	fresh := func(ts []time.Time) []time.Time {
		i := 0
		for i < len(ts) && now.Sub(ts[i]) >= limitWindow {
			i++
		}
		return ts[i:]
	}
	for k, ts := range rc.limits {
		if ts = fresh(ts); len(ts) == 0 {
			delete(rc.limits, k)
		} else {
			rc.limits[k] = ts
		}
	}
	rc.failures = fresh(rc.failures)
}

// clientKey is who the limiter counts r against: the connecting peer,
// or the client a trusted proxy (TrustedProxyCIDRs only) names. An IPv6
// client counts as its /64, which one host can rotate through.
func (rc *recovery) clientKey(r *http.Request, f nodeFacts) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(strings.Trim(host, "[]"))
	if err != nil {
		return "unknown"
	}
	addr = addr.Unmap()
	if trusted := identity.ProxyPrefixes(f.Server.TrustedProxyCIDRs); len(trusted) > 0 {
		for _, p := range trusted {
			if p.Contains(addr) {
				if ip, _ := identity.ClientNet(r, trusted...); ip.IsValid() {
					addr = ip.Unmap()
				}
				break
			}
		}
	}
	if addr.Is6() {
		return netip.PrefixFrom(addr, 64).Masked().String()
	}
	return addr.String()
}

// verify runs fn as one of at most maxVerifies concurrent Argon2id
// checks; ran is false when that many are already running.
func (rc *recovery) verify(fn func() bool) (result, ran bool) {
	select {
	case rc.verifying <- struct{}{}:
	default:
		return false, false
	}
	defer func() { <-rc.verifying }()
	return fn(), true
}

// validUsername accepts what identity accepts for a local username.
func validUsername(s string) bool {
	return len(s) > 0 && len(s) <= maxUsername && strings.TrimSpace(s) == s && !strings.ContainsAny(s, "\x00\r\n")
}

// proxied reports whether a reverse proxy forwarded r.
func proxied(r *http.Request) bool {
	for _, h := range []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-Ip"} {
		if r.Header.Get(h) != "" {
			return true
		}
	}
	return false
}

// ownerCredential is the owner's local sign-in: the mirror, or the
// database read-only without one.
func (rc *recovery) ownerCredential(ctx context.Context) (identity.OwnerCredential, bool) {
	if cred, ok := readOwnerMirror(rc.dataDir); ok {
		return cred, true
	}
	cred, ok, err := ownerCredentialFromDB(ctx, rc.dataDir)
	return cred, ok && err == nil
}

func (rc *recovery) access(w http.ResponseWriter, r *http.Request) {
	f := rc.facts()
	if !f.Standalone() {
		recoveryJSON(w, http.StatusOK, map[string]bool{"available": false})
		return
	}
	_, local := rc.ownerCredential(r.Context())
	recoveryJSON(w, http.StatusOK, map[string]bool{
		"available": true,
		"granted":   rc.open(r, f),
		"local":     local,
		"plex":      f.OwnerPlexID != 0 && rc.plexFor(f) != nil,
		"code":      !proxied(r),
	})
}

func (rc *recovery) localSignIn(w http.ResponseWriter, r *http.Request) {
	f := rc.facts()
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body) != nil {
		recoveryError(w, http.StatusBadRequest, "invalid", "invalid request")
		return
	}
	if !validUsername(body.Username) || len(body.Password) == 0 || len(body.Password) > 1024 {
		recoveryError(w, http.StatusBadRequest, "invalid", "enter a username and password")
		return
	}
	if !rc.admit("local:"+rc.clientKey(r, f), body.Username) {
		recoveryError(w, http.StatusTooManyRequests, "rate_limit", "too many attempts; try again later")
		return
	}
	cred, ok := rc.ownerCredential(r.Context())
	match, ran := rc.verify(func() bool { return ok && identity.VerifyOwnerCredential(cred, body.Username, body.Password) })
	if !ran {
		recoveryError(w, http.StatusTooManyRequests, "busy", "too many sign-ins at once; try again in a moment")
		return
	}
	if !match {
		rc.failed()
		recoveryError(w, http.StatusUnauthorized, "invalid_credentials", "that username and password don't match the owner's")
		return
	}
	rc.log.Info("recovery: owner signed in on the startup page", "method", "local")
	recoveryJSON(w, http.StatusOK, map[string]any{"done": true, "token": rc.issueToken()})
}

// normalizeCode accepts the code typed with or without dashes and spaces.
func normalizeCode(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '-' || r == ' ' {
			return -1
		}
		if r >= 'a' && r <= 'z' {
			return r - 'a' + 'A'
		}
		return r
	}, strings.TrimSpace(s))
}

func (rc *recovery) codeSignIn(w http.ResponseWriter, r *http.Request) {
	f := rc.facts()
	if proxied(r) {
		recoveryError(w, http.StatusForbidden, "proxied", "open this page directly on the local network, not through a proxy, to use the recovery code")
		return
	}
	var body struct {
		Code string `json:"code"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&body) != nil {
		recoveryError(w, http.StatusBadRequest, "invalid", "invalid request")
		return
	}
	if !rc.admit("code:"+rc.clientKey(r, f), "") {
		recoveryError(w, http.StatusTooManyRequests, "rate_limit", "too many attempts; try again later")
		return
	}
	rc.mu.Lock()
	want := normalizeCode(rc.code)
	ok := want != "" && subtle.ConstantTimeCompare([]byte(want), []byte(normalizeCode(body.Code))) == 1
	if ok {
		// One use: the next owner who needs it reads the new one.
		rc.newCodeLocked()
	}
	rc.mu.Unlock()
	if !ok {
		rc.failed()
		recoveryError(w, http.StatusUnauthorized, "invalid_code", "that recovery code is not right")
		return
	}
	rc.log.Info("recovery: owner signed in on the startup page", "method", "code")
	recoveryJSON(w, http.StatusOK, map[string]any{"done": true, "token": rc.issueToken()})
}

func (rc *recovery) startPIN(w http.ResponseWriter, r *http.Request) {
	f := rc.facts()
	plex := rc.plexFor(f)
	if plex == nil || f.OwnerPlexID == 0 {
		recoveryError(w, http.StatusServiceUnavailable, "signin_unavailable", "Plex sign-in is not available for this server")
		return
	}
	if !rc.admit("pin:"+rc.clientKey(r, f), "") {
		recoveryError(w, http.StatusTooManyRequests, "rate_limit", "too many attempts; try again later")
		return
	}
	pin, err := plex.CreatePIN(r.Context())
	if err != nil {
		recoveryError(w, http.StatusBadGateway, "plextv", "Could not reach plex.tv")
		return
	}
	handle := rand.Text()
	rc.mu.Lock()
	for h, p := range rc.pins {
		if !p.ExpiresAt.IsZero() && rc.now().After(p.ExpiresAt) {
			delete(rc.pins, h)
		}
	}
	rc.pins[handle] = pin
	rc.mu.Unlock()
	// plex.tv sends the browser back here after approval: only to a local
	// name or address, never to whatever Host a request claimed.
	forward := ""
	if localForwardHost(r.Host) {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		forward = (&url.URL{Scheme: scheme, Host: r.Host, Path: "/ether/"}).String()
	}
	recoveryJSON(w, http.StatusOK, map[string]string{"handle": handle, "code": pin.Code, "authUrl": plex.AuthURL(pin, forward)})
}

// pollPIN completes a Plex sign-in: only the account that owns this server
// receives a recovery token.
func (rc *recovery) pollPIN(w http.ResponseWriter, r *http.Request) {
	f := rc.facts()
	handle := r.PathValue("handle")
	rc.mu.Lock()
	pin := rc.pins[handle]
	rc.mu.Unlock()
	plex := rc.plexFor(f)
	if pin == nil || plex == nil {
		recoveryError(w, http.StatusNotFound, "expired", "This sign-in expired. Start again.")
		return
	}
	got, err := plex.CheckPIN(r.Context(), pin.ID, pin.Code)
	if err != nil {
		recoveryError(w, http.StatusBadGateway, "plextv", "Could not check the sign-in with plex.tv")
		return
	}
	if got.AuthToken == "" {
		recoveryJSON(w, http.StatusOK, map[string]any{"done": false})
		return
	}
	rc.mu.Lock()
	delete(rc.pins, handle)
	rc.mu.Unlock()
	acct, err := plex.User(r.Context(), got.AuthToken)
	if err != nil || f.OwnerPlexID == 0 || acct.ID != f.OwnerPlexID {
		rc.failed()
		recoveryError(w, http.StatusForbidden, "not_owner", "That Plex account doesn't own this server. Sign in as the owner.")
		return
	}
	rc.log.Info("recovery: owner signed in on the startup page", "method", "plex", "account", strconv.FormatInt(acct.ID, 10))
	recoveryJSON(w, http.StatusOK, map[string]any{"done": true, "token": rc.issueToken()})
}

// ---- Restores -------------------------------------------------------------------

func (rc *recovery) backups(w http.ResponseWriter, r *http.Request) {
	snaps, err := backup.LocalSnapshots(rc.dataDir)
	if err != nil {
		recoveryError(w, http.StatusInternalServerError, "backups", "the backups folder cannot be read")
		return
	}
	out := map[string]any{"snapshots": snaps}
	if rep, _ := backup.ReadReport(rc.dataDir); rep != nil && rep.Status == "restored" && rep.Rollback != "" {
		out["undo"] = map[string]any{"source": rep.Source, "restoredAt": rep.RestoredAt}
	}
	recoveryJSON(w, http.StatusOK, out)
}

// begin admits one restore at a time, and none once a start has begun
// or succeeded.
func (rc *recovery) begin(w http.ResponseWriter) bool {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	switch {
	case rc.ready:
		recoveryError(w, http.StatusConflict, "running", "Ether is running; restore from the console")
		return false
	case rc.attempting:
		recoveryError(w, http.StatusConflict, "starting", "Ether is starting again; try in a moment")
		return false
	case rc.busy > 0:
		recoveryError(w, http.StatusConflict, "busy", "another restore is in progress")
		return false
	}
	rc.busy++
	return true
}

func (rc *recovery) leave() {
	rc.mu.Lock()
	rc.busy--
	rc.mu.Unlock()
}

func (rc *recovery) restoreSnapshot(w http.ResponseWriter, r *http.Request) {
	if !rc.begin(w) {
		return
	}
	defer rc.leave()
	st, err := backup.StageLocal(r.Context(), rc.dataDir, r.PathValue("name"))
	if errors.Is(err, os.ErrNotExist) {
		recoveryError(w, http.StatusNotFound, "not_found", "no such backup")
		return
	}
	rc.restore(w, st, err)
}

func (rc *recovery) restoreUndo(w http.ResponseWriter, r *http.Request) {
	if !rc.begin(w) {
		return
	}
	defer rc.leave()
	if err := backup.RequestUndo(rc.dataDir); err != nil {
		rc.requestError(w, err)
		return
	}
	rc.log.Info("recovery: undoing the last restore from the startup page")
	recoveryJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
	rc.front.retryNow()
}

func (rc *recovery) restoreUpload(w http.ResponseWriter, r *http.Request) {
	// Same rules as the console upload: the header and a same-origin
	// octet-stream body (which a cross-site form cannot send).
	ct := strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0])
	if r.Header.Get("X-Ether-Request") != "1" || !strings.EqualFold(ct, "application/octet-stream") || !sameOriginRequest(r) {
		recoveryError(w, http.StatusForbidden, "csrf", "upload the backup from the Ether console")
		return
	}
	raw, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(r.Header.Get("X-Ether-Backup-Password")))
	if len(raw) == 0 {
		recoveryError(w, http.StatusBadRequest, "password_required", "enter the password the backup was downloaded with")
		return
	}
	if !rc.begin(w) {
		return
	}
	defer rc.leave()
	body := http.MaxBytesReader(w, r.Body, 9<<30)
	head := make([]byte, 8)
	n, _ := io.ReadFull(body, head)
	if !backup.IsEncrypted(head[:n]) {
		recoveryError(w, http.StatusBadRequest, "encrypted_only", "restore an encrypted backup downloaded from Ether here; older unencrypted bundles need the running console")
		return
	}
	st, err := backup.StageUpload(r.Context(), rc.dataDir, io.MultiReader(bytes.NewReader(head[:n]), body), string(raw), uploadFileName(r))
	if errors.Is(err, backup.ErrBadPassword) {
		recoveryError(w, http.StatusBadRequest, "bad_password", err.Error())
		return
	}
	rc.restore(w, st, err)
}

// restore requests the staged restore and starts the server again at once.
func (rc *recovery) restore(w http.ResponseWriter, st backup.Staged, err error) {
	if err == nil {
		err = backup.PrepareStaged(rc.dataDir, st.ID, rc.facts().Server)
	}
	if err == nil {
		err = backup.RequestStaged(rc.dataDir, st.ID)
	}
	if err != nil {
		if st.ID != "" {
			_ = backup.DiscardStaged(rc.dataDir, st.ID)
		}
		rc.requestError(w, err)
		return
	}
	rc.log.Info("recovery: restoring a backup from the startup page", "source", st.Source)
	recoveryJSON(w, http.StatusAccepted, st)
	rc.front.retryNow()
}

func (rc *recovery) requestError(w http.ResponseWriter, err error) {
	if errors.Is(err, backup.ErrRestoreInProgress) {
		recoveryError(w, http.StatusConflict, "restore_in_progress", err.Error())
		return
	}
	recoveryError(w, http.StatusBadRequest, "invalid_backup", err.Error())
}

// localForwardHost reports whether host (a Host header) is an IP literal
// on loopback or a private network: the only place plex.tv may send the
// browser back to after a recovery sign-in.
func localForwardHost(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	addr, err := netip.ParseAddr(strings.Trim(host, "[]"))
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	return addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast()
}

func sameOriginRequest(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && u.Host != "" && strings.EqualFold(u.Host, r.Host)
}

// uploadFileName is the uploaded file's name from X-Ether-Backup-Name.
func uploadFileName(r *http.Request) string {
	name := strings.TrimSpace(r.Header.Get("X-Ether-Backup-Name"))
	if dec, err := url.PathUnescape(name); err == nil {
		name = dec
	}
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	if len(name) > 200 || strings.ContainsAny(name, "\x00\r\n") {
		return ""
	}
	return name
}
