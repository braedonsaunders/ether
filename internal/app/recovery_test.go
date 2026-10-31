package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/backup"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/plextv"
	"github.com/braedonsaunders/ether/internal/startup"
	"github.com/braedonsaunders/ether/internal/store"
)

// ownedDataDir is a standalone server whose setup is finished, owned by
// Plex account 4242 and by the local owner "owner"/"first secret", with one
// backup kept.
func ownedDataDir(t *testing.T) (dataDir, bundle string, db *sql.DB) {
	t.Helper()
	dataDir = t.TempDir()
	cfg, err := config.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Update(func(c *config.Config) error {
		c.SetupComplete, c.Plex.OwnerPlexID, c.Plex.ClientIdentifier = true, 4242, "client-1"
		c.Plex.Token = "plex-server-token" // sealed with the credential key
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	bundle = writeBundle(t, dataDir)
	db, err = store.Open(context.Background(), dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	local, err := identity.NewLocal(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := local.CreateOwner(context.Background(), "owner", "first secret"); err != nil {
		t.Fatal(err)
	}
	return dataDir, bundle, db
}

func testRecovery(dataDir string) *recovery {
	rc := newRecovery(dataDir, newFrontDoor(startup.NewTracker("1", time.Now), quietLog()), quietLog())
	rc.plexFor = func(nodeFacts) recoveryPlex { return &fakePlexRecorder{fakePlex: fakePlex{id: 4242}} }
	return rc
}

type fakePlexRecorder struct {
	fakePlex
	forward string
}

func (f *fakePlexRecorder) AuthURL(_ *plextv.PIN, forward string) string {
	f.forward = forward
	return "https://app.plex.tv/auth#?forward=" + forward
}

func jsonBody(t *testing.T, w *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(w.Body.Bytes(), v); err != nil {
		t.Fatalf("decode %s: %v", w.Body, err)
	}
}

func signIn(t *testing.T, h http.Handler, path, body string, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", "http://192.168.1.10:32400"+startup.RecoveryPrefix+path, strings.NewReader(body))
	r.RemoteAddr = "192.168.1.50:5000"
	r.Header.Set("X-Ether-Request", "1")
	r.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// Finding 4: after setup no address gets tokenless restore, the server's
// own machine included (a same-host reverse proxy would admit anyone).
func TestRecoveryAfterSetupAlwaysNeedsASignIn(t *testing.T) {
	dataDir, _, _ := ownedDataDir(t)
	h := testRecovery(dataDir).handler()
	for _, c := range []struct{ host, remote string }{
		{"192.168.1.10:32400", "192.168.1.50:5000"},
		{"localhost:32400", "127.0.0.1:5000"},
		{"127.0.0.1:32400", "127.0.0.1:5000"}, // nginx proxy_pass to loopback
	} {
		r := httptest.NewRequest("GET", "http://"+c.host+startup.RecoveryPrefix+"backups", nil)
		r.RemoteAddr = c.remote
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s from %s: %d", c.host, c.remote, w.Code)
		}
	}
}

// Finding 4: the one-time recovery code, from the data directory or log.
func TestRecoveryCodeSignIn(t *testing.T) {
	dataDir, _, _ := ownedDataDir(t)
	rc := testRecovery(dataDir)
	h := rc.handler()
	rc.prepare()
	raw, err := os.ReadFile(filepath.Join(dataDir, recoveryCodeFile))
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(filepath.Join(dataDir, recoveryCodeFile)); st.Mode().Perm()&0o077 != 0 {
		t.Fatalf("code file mode %v", st.Mode())
	}
	code := rc.code
	if !strings.Contains(string(raw), code) || len(normalizeCode(code)) != 12 {
		t.Fatalf("code %q, file %q", code, raw)
	}
	if w := signIn(t, h, "code", `{"code":"AAAA-BBBB-CCCC"}`, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong code: %d", w.Code)
	}
	if w := signIn(t, h, "code", `{"code":"`+code+`"}`, map[string]string{"X-Forwarded-For": "203.0.113.9"}); w.Code != http.StatusForbidden {
		t.Fatalf("proxied code: %d", w.Code)
	}
	// Typed without dashes, in lower case.
	w := signIn(t, h, "code", `{"code":"`+strings.ToLower(normalizeCode(code))+`"}`, nil)
	var done struct{ Token string }
	jsonBody(t, w, &done)
	if w.Code != 200 || !strings.HasPrefix(done.Token, "ether-recovery-") {
		t.Fatalf("code sign-in: %d %s", w.Code, w.Body)
	}
	if w := recoveryCall(h, "GET", "backups", done.Token, nil); w.Code != 200 {
		t.Fatalf("backups with the code's token: %d", w.Code)
	}
	// One use: the code changed, and the old one no longer works.
	if rc.code == code {
		t.Fatal("code not rotated after use")
	}
	if w := signIn(t, h, "code", `{"code":"`+code+`"}`, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("reused code: %d", w.Code)
	}
	// Guessing is rate limited.
	var last int
	for range limitPerIP + 1 {
		last = signIn(t, h, "code", `{"code":"ZZZZ-ZZZZ-ZZZZ"}`, nil).Code
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("after many guesses: %d", last)
	}
	rc.clear()
	if _, err := os.Stat(filepath.Join(dataDir, recoveryCodeFile)); !os.IsNotExist(err) {
		t.Fatal("code file kept after the server started")
	}
}

func TestRecoveryPlexSignIn(t *testing.T) {
	dataDir, bundle, _ := ownedDataDir(t)
	rc := testRecovery(dataDir)
	plex := &fakePlexRecorder{fakePlex: fakePlex{id: 1}}
	rc.plexFor = func(nodeFacts) recoveryPlex { return plex }
	h := rc.handler()
	var pin struct{ Handle string }
	jsonBody(t, signIn(t, h, "pin", "", nil), &pin)
	if w := recoveryCall(h, "GET", "pin/"+pin.Handle, "", nil); w.Code != http.StatusForbidden {
		t.Fatalf("another account's sign-in: %d %s", w.Code, w.Body)
	}
	if plex.forward != "http://192.168.1.10:32400/ether/" {
		t.Fatalf("forward URL = %q", plex.forward)
	}
	// plex.tv never sends the owner to a name the request merely claimed.
	r := httptest.NewRequest("POST", "http://evil.example.com"+startup.RecoveryPrefix+"pin", nil)
	r.RemoteAddr, r.Header = "192.168.1.50:5000", http.Header{"X-Ether-Request": {"1"}}
	h.ServeHTTP(httptest.NewRecorder(), r)
	if plex.forward != "" {
		t.Fatalf("forward URL for a foreign Host = %q", plex.forward)
	}
	plex.id = 4242
	jsonBody(t, signIn(t, h, "pin", "", nil), &pin)
	var done struct{ Token string }
	w := recoveryCall(h, "GET", "pin/"+pin.Handle, "", nil)
	jsonBody(t, w, &done)
	if w.Code != 200 || done.Token == "" {
		t.Fatalf("owner sign-in: %d %s", w.Code, w.Body)
	}
	if w := recoveryCall(h, "POST", "restore/snapshot/"+bundle, done.Token, nil); w.Code != http.StatusAccepted {
		t.Fatalf("owner restore: %d %s", w.Code, w.Body)
	}
	// Starting PIN sign-ins is rate limited too.
	var last int
	for range limitPerIP + 1 {
		last = signIn(t, h, "pin", "", nil).Code
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("after many PINs: %d", last)
	}
}

// Owner request B: the local owner sign-in, from the mirror or (without
// it) the database read-only.
func TestRecoveryLocalSignIn(t *testing.T) {
	dataDir, _, db := ownedDataDir(t)
	h := testRecovery(dataDir).handler()
	local := func(user, pass string) int {
		return signIn(t, h, "local", `{"username":"`+user+`","password":"`+pass+`"}`, nil).Code
	}
	var access map[string]bool
	jsonBody(t, recoveryCall(h, "GET", "access", "", nil), &access)
	if !access["local"] || !access["plex"] || !access["code"] || access["granted"] {
		t.Fatalf("access = %v", access)
	}

	// No mirror yet: the database, read-only.
	if _, err := os.Stat(ownerMirrorPath(dataDir)); !os.IsNotExist(err) {
		t.Fatal("mirror exists before any sync")
	}
	if c := local("owner", "first secret"); c != 200 {
		t.Fatalf("database fallback: %d", c)
	}
	if c := local("owner", "wrong"); c != http.StatusUnauthorized {
		t.Fatalf("wrong password: %d", c)
	}

	// The mirror, written by the running server, owner-only.
	a := &App{db: db, opts: Options{DataDir: dataDir}, log: quietLog()}
	a.syncOwnerRecovery(context.Background())
	st, err := os.Stat(ownerMirrorPath(dataDir))
	if err != nil || st.Mode().Perm()&0o077 != 0 {
		t.Fatalf("mirror %v %v", st, err)
	}
	raw, _ := os.ReadFile(ownerMirrorPath(dataDir))
	if strings.Contains(string(raw), "first secret") || !strings.Contains(string(raw), "$argon2id$") {
		t.Fatalf("mirror content %s", raw)
	}
	// With the mirror the database is not needed: damage it.
	db.Close()
	if err := os.WriteFile(filepath.Join(dataDir, "ether.db"), []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(dataDir, "ether.db-wal"))
	os.Remove(filepath.Join(dataDir, "ether.db-shm"))
	if c := local("owner", "first secret"); c != 200 {
		t.Fatalf("mirror sign-in with a damaged database: %d", c)
	}

	// A wrong username or password never signs in, and repeated failures
	// for one name are rate limited even with the right password after.
	if c := local("Owner", "first secret"); c != http.StatusUnauthorized {
		t.Fatalf("wrong username: %d", c)
	}
	for range limitPerName {
		local("owner", "guess")
	}
	if c := local("owner", "first secret"); c != http.StatusTooManyRequests {
		t.Fatalf("after %d failures: %d", limitPerName, c)
	}
}

// Owner request B: a stale mirror (password changed on another node) keeps
// the old password until the next sync; the sync replaces it; removing the
// owner's local login removes the mirror.
func TestOwnerRecoveryMirrorFollowsTheDatabase(t *testing.T) {
	dataDir, _, db := ownedDataDir(t)
	a := &App{db: db, opts: Options{DataDir: dataDir}, log: quietLog()}
	a.syncOwnerRecovery(context.Background())
	cred, _, _ := identity.OwnerLocalCredential(context.Background(), db)
	local, _ := identity.NewLocal(db)
	if err := local.SetPassword(context.Background(), cred.ViewerID, "owner", "second secret"); err != nil {
		t.Fatal(err)
	}
	mirror, ok := readOwnerMirror(dataDir)
	if !ok || !identity.VerifyOwnerCredential(mirror, "owner", "first secret") || identity.VerifyOwnerCredential(mirror, "owner", "second secret") {
		t.Fatal("stale mirror should still hold the previous password")
	}
	a.syncOwnerRecovery(context.Background())
	mirror, _ = readOwnerMirror(dataDir)
	if identity.VerifyOwnerCredential(mirror, "owner", "first secret") || !identity.VerifyOwnerCredential(mirror, "owner", "second secret") {
		t.Fatal("sync did not refresh the mirror")
	}
	if _, err := db.Exec(`DELETE FROM identity_links WHERE provider='local'`); err != nil {
		t.Fatal(err)
	}
	a.syncOwnerRecovery(context.Background())
	if _, err := os.Stat(ownerMirrorPath(dataDir)); !os.IsNotExist(err) {
		t.Fatal("mirror kept after the owner's local login was removed")
	}
}

// Finding 3: settings that cannot be read fail closed.
func TestUnreadableSettingsFailClosed(t *testing.T) {
	// The credential key is gone: config.Read fails, but the plain facts
	// still say setup is complete, so the LAN gets no tokenless restore.
	dataDir, _, _ := ownedDataDir(t)
	if err := os.Remove(filepath.Join(dataDir, config.CredentialKeyFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Read(dataDir); err == nil {
		t.Fatal("fixture: settings still readable without the key")
	}
	if w := recoveryCall(testRecovery(dataDir).handler(), "GET", "backups", "", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("LAN backups without the key: %d", w.Code)
	}
	if f := readNodeFacts(dataDir, os.Getenv); !f.SetupComplete || !f.Standalone() {
		t.Fatalf("facts without the key = %+v", f)
	}

	// config.json itself is damaged: setup complete, not standalone (no
	// restore from here), and the port bound to loopback only.
	broken := t.TempDir()
	if err := os.WriteFile(filepath.Join(broken, "config.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := readNodeFacts(broken, os.Getenv)
	if f.Readable || !f.SetupComplete || f.Standalone() || f.Server.BindAddress != "127.0.0.1" {
		t.Fatalf("facts for damaged settings = %+v", f)
	}
	rc := testRecovery(broken)
	var access map[string]bool
	jsonBody(t, recoveryCall(rc.handler(), "GET", "access", "", nil), &access)
	if access["available"] {
		t.Fatalf("restore offered with damaged settings: %v", access)
	}
	if w := recoveryCall(rc.handler(), "GET", "backups", "", nil); w.Code != http.StatusConflict {
		t.Fatalf("backups with damaged settings: %d", w.Code)
	}
	// A new data directory is a fresh install: setup open, standalone.
	if f := readNodeFacts(t.TempDir(), os.Getenv); !f.Readable || f.SetupComplete || !f.Standalone() {
		t.Fatalf("facts for a new data directory = %+v", f)
	}
	// A cluster's sealed database URL still marks the node as clustered.
	pg := t.TempDir()
	os.WriteFile(filepath.Join(pg, "config.json"), []byte(`{"database":{"url":"ether-sealed:v2:abc"}}`), 0o600)
	if readNodeFacts(pg, os.Getenv).Standalone() {
		t.Fatal("a sealed PostgreSQL URL reported standalone")
	}
}

// Finding 5: one recovery follows Serve's retries.
func TestRecoveryFollowsTheRetryLoop(t *testing.T) {
	dataDir, bundle, _ := ownedDataDir(t)
	rc := testRecovery(dataDir)
	h := rc.handler()
	rc.prepare()
	var done struct{ Token string }
	jsonBody(t, signIn(t, h, "code", `{"code":"`+rc.code+`"}`, nil), &done)

	// While a start runs, a restore is refused (it would land after the
	// start applied pending restores).
	if err := rc.beginAttempt(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w := recoveryCall(h, "POST", "restore/snapshot/"+bundle, done.Token, nil); w.Code != http.StatusConflict {
		t.Fatalf("restore during a start: %d %s", w.Code, w.Body)
	}
	rc.endAttempt()
	// The sign-in survived the attempt.
	if w := recoveryCall(h, "GET", "backups", done.Token, nil); w.Code != 200 {
		t.Fatalf("token after a retry: %d", w.Code)
	}
	// A start waits for a restore being prepared.
	rc.mu.Lock()
	rc.busy = 1
	rc.mu.Unlock()
	started := make(chan struct{})
	go func() { _ = rc.beginAttempt(context.Background()); close(started) }()
	select {
	case <-started:
		t.Fatal("a start began while a restore was being prepared")
	case <-time.After(150 * time.Millisecond):
	}
	rc.leave()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("the start never began")
	}
	rc.endAttempt()

	// A stale wake-up does not skip the backoff.
	rc.mu.Lock()
	rc.activeUntil = time.Time{}
	rc.mu.Unlock()
	rc.front.retryNow()
	t0 := time.Now()
	if !rc.wait(context.Background(), 80*time.Millisecond) || time.Since(t0) < 70*time.Millisecond {
		t.Fatalf("wait returned after %v", time.Since(t0))
	}
	// The page in use holds the retry.
	old := recoveryPoll
	recoveryPoll = 10 * time.Millisecond
	defer func() { recoveryPoll = old }()
	rc.mu.Lock()
	rc.activeUntil = time.Now().Add(200 * time.Millisecond)
	rc.mu.Unlock()
	t0 = time.Now()
	rc.wait(context.Background(), 20*time.Millisecond)
	if time.Since(t0) < 180*time.Millisecond {
		t.Fatalf("retry did not wait for the page: %v", time.Since(t0))
	}
}

// Finding 6: no new restore over a restore being applied.
func TestRecoveryRefusesARestoreOverAnUnfinishedOne(t *testing.T) {
	dataDir, bundle, _ := ownedDataDir(t)
	if err := os.WriteFile(filepath.Join(dataDir, backup.PendingFile), []byte(`{"staged":"restored-x","rollback":"restore-1","applied":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	rc := testRecovery(dataDir)
	h := rc.handler()
	rc.prepare()
	var done struct{ Token string }
	jsonBody(t, signIn(t, h, "code", `{"code":"`+rc.code+`"}`, nil), &done)
	w := recoveryCall(h, "POST", "restore/snapshot/"+bundle, done.Token, nil)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "restore_in_progress") {
		t.Fatalf("restore over an unfinished one: %d %s", w.Code, w.Body)
	}
	if p, _ := backup.ReadPending(dataDir); p == nil || p.Rollback != "restore-1" {
		t.Fatalf("record overwritten: %+v", p)
	}
	if f := failureOf(startFail(startup.FailRestore, os.ErrInvalid), true); f.Recoverable {
		t.Fatal("a failed undo offered a restore from the page")
	}
}

// Finding 7: the page offers Undo for the last restore.
func TestRecoveryUndoesTheLastRestore(t *testing.T) {
	dataDir, _, _ := ownedDataDir(t)
	os.MkdirAll(filepath.Join(dataDir, backup.RollbackDir, "restore-1"), 0o700)
	os.WriteFile(filepath.Join(dataDir, backup.RollbackDir, "restore-1", "config.json"), []byte(`{"server":{}}`), 0o600)
	os.WriteFile(filepath.Join(dataDir, backup.RollbackDir, "restore-1", "ether.db"), []byte("before"), 0o600)
	if err := backup.WriteJSONDurable(filepath.Join(dataDir, backup.ReportFile), backup.Report{Status: "restored", Source: "ether-1.tar.gz", Rollback: "restore-1", RestoredAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	rc := testRecovery(dataDir)
	h := rc.handler()
	rc.prepare()
	var done struct{ Token string }
	jsonBody(t, signIn(t, h, "code", `{"code":"`+rc.code+`"}`, nil), &done)
	if w := recoveryCall(h, "GET", "backups", done.Token, nil); !strings.Contains(w.Body.String(), `"undo"`) {
		t.Fatalf("backups without undo: %s", w.Body)
	}
	if w := recoveryCall(h, "POST", "restore/undo", done.Token, nil); w.Code != http.StatusAccepted {
		t.Fatalf("undo: %d %s", w.Code, w.Body)
	}
	if p, _ := backup.ReadPending(dataDir); p == nil || p.Kind != "undo" {
		t.Fatalf("pending = %+v", p)
	}
}

// Finding 7: a restored server that cannot start puts the original data
// back, and the next start runs on it.
func TestNewRollsBackARestoreThatCannotStart(t *testing.T) {
	dataDir := t.TempDir()
	cfg, err := config.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Update(func(c *config.Config) error {
		c.Transcode.FFmpegPath = "/not-installed/ffmpeg"
		c.Server.FriendlyName = "Original"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	a, err := New(context.Background(), Options{DataDir: dataDir, Logger: quietLog()})
	if err != nil {
		t.Fatal(err)
	}
	a.Close()
	blocker := filepath.Join(t.TempDir(), "file")
	os.WriteFile(blocker, nil, 0o600)
	name := writeBundle(t, dataDir, func(c *config.Config) { c.Transcode.TempDir = filepath.Join(blocker, "x") })
	st, err := backup.StageLocal(context.Background(), dataDir, name)
	if err != nil {
		t.Fatal(err)
	}
	if err := backup.PrepareStaged(dataDir, st.ID, config.Server{}); err != nil {
		t.Fatal(err)
	}
	if err := backup.RequestStaged(dataDir, st.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := New(context.Background(), Options{DataDir: dataDir, Logger: quietLog()}); err == nil || !strings.Contains(err.Error(), "original data was put back") {
		t.Fatalf("New on a broken restore = %v", err)
	}
	rep, _ := backup.ReadReport(dataDir)
	if rep == nil || rep.Status != "failed" || !rep.RolledBack {
		t.Fatalf("report = %+v", rep)
	}
	a, err = New(context.Background(), Options{DataDir: dataDir, Logger: quietLog()})
	if err != nil {
		t.Fatalf("start on the original data: %v", err)
	}
	defer a.Close()
	if got := a.cfg.Get().Server.FriendlyName; got != "Original" {
		t.Fatalf("server name after rollback = %q", got)
	}
}

// A pending restore is never applied on a node that shares PostgreSQL.
func TestPendingRestoreIsNotAppliedOnACluster(t *testing.T) {
	dataDir, bundle, _ := ownedDataDir(t)
	st, err := backup.StageLocal(context.Background(), dataDir, bundle)
	if err != nil {
		t.Fatal(err)
	}
	if err := backup.RequestStaged(dataDir, st.ID); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(dataDir, "config.json"))
	t.Setenv(config.DatabaseURLEnv, "postgres://db.lan/ether")
	a := &App{opts: Options{DataDir: dataDir}, log: quietLog(), tracker: startup.NewTracker("1", time.Now)}
	if err := a.applyPendingRestore(); err != nil {
		t.Fatal(err)
	}
	if p, _ := backup.ReadPending(dataDir); p != nil {
		t.Fatal("restore left pending on a cluster node")
	}
	if after, _ := os.ReadFile(filepath.Join(dataDir, "config.json")); string(after) != string(before) {
		t.Fatal("a cluster node's settings were replaced")
	}
}

// keepListeningPort leaves a port the owner just saved alone when the
// listener was kept from an earlier start (a cluster transition).
func TestKeepListeningPortLeavesAKeptListenerAlone(t *testing.T) {
	cfg, err := config.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Update(func(c *config.Config) error { c.Server.Port = 32600; return nil }); err != nil {
		t.Fatal(err)
	}
	front := newFrontDoor(startup.NewTracker("1", time.Now), quietLog())
	front.ln, front.port = fakeListener{}, 32500
	a := &App{cfg: cfg, log: quietLog(), opts: Options{front: front}}
	if err := a.keepListeningPort(); err != nil || cfg.Get().Server.Port != 32600 {
		t.Fatalf("kept listener: port %d, %v", cfg.Get().Server.Port, err)
	}
	a.boundNow = true
	if err := a.keepListeningPort(); err != nil || cfg.Get().Server.Port != 32500 {
		t.Fatalf("fresh listener: port %d, %v", cfg.Get().Server.Port, err)
	}
}

// fakeListener stands in for a bound listener.
type fakeListener struct{}

func (fakeListener) Accept() (net.Conn, error) { return nil, net.ErrClosed }
func (fakeListener) Close() error              { return nil }
func (fakeListener) Addr() net.Addr            { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 32500} }
