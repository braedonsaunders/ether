package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/backup"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/plextv"
	"github.com/braedonsaunders/ether/internal/service"
	"github.com/braedonsaunders/ether/internal/startup"
	"github.com/braedonsaunders/ether/internal/store"
)

// freePort returns a TCP port nothing listens on.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

// serveInBackground runs Serve on dataDir at port until the test ends.
func serveInBackground(t *testing.T, dataDir string, port int) (base string, stop func() error) {
	t.Helper()
	t.Setenv("ETHER_PORT", strconv.Itoa(port))
	cfg, err := config.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Update(func(c *config.Config) error {
		c.Server.BindAddress = "127.0.0.1"
		c.Transcode.FFmpegPath = "/not-installed/ffmpeg"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, Options{DataDir: dataDir, Logger: quietLog(), Version: "9.9.9"}) }()
	stop = func() error {
		cancel()
		select {
		case err := <-done:
			return err
		case <-time.After(20 * time.Second):
			t.Fatal("Serve did not stop")
			return nil
		}
	}
	t.Cleanup(func() { cancel() })
	return fmt.Sprintf("http://127.0.0.1:%d", port), stop
}

func fetchStatus(t *testing.T, base string) (startup.Status, string) {
	t.Helper()
	resp, err := http.Get(base + startup.StatusPath)
	if err != nil {
		return startup.Status{}, ""
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var st startup.Status
	_ = json.Unmarshal(raw, &st)
	return st, string(raw)
}

func waitPhase(t *testing.T, base string, phase startup.Phase) (startup.Status, string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if st, raw := fetchStatus(t, base); st.Phase == phase {
			return st, raw
		}
		time.Sleep(25 * time.Millisecond)
	}
	st, raw := fetchStatus(t, base)
	t.Fatalf("phase %q never reached; last %s", phase, raw)
	return st, raw
}

// A clustered node whose PostgreSQL server is down answers only its
// status page: no Plex API (so no client sees a half-started server).
func TestServeShowsWaitingForTheDatabase(t *testing.T) {
	dbPort := freePort(t) // nothing listens: connection refused, retried
	t.Setenv(config.DatabaseURLEnv, fmt.Sprintf("postgres://ether:sup3r-secret-pw@127.0.0.1:%d/ether?sslmode=disable", dbPort))
	base, stop := serveInBackground(t, t.TempDir(), freePort(t))
	st, raw := waitPhase(t, base, startup.PhaseWaitingDatabase)
	if st.Database == nil || st.Database.Kind != "postgres" || st.Database.Host != fmt.Sprintf("127.0.0.1:%d", dbPort) ||
		st.Attempt < 1 || st.RetryAt == nil || st.Ready || st.Version != "9.9.9" {
		t.Fatalf("status = %s", raw)
	}
	if strings.Contains(raw, "sup3r-secret-pw") || strings.Contains(raw, "ether:") {
		t.Fatalf("status leaks the database credentials: %s", raw)
	}
	for _, path := range []string{"/identity", "/library/sections", "/ether/api/bootstrap", "/ether/api/config"} {
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" {
			t.Fatalf("GET %s while waiting: %d %v", path, resp.StatusCode, resp.Header)
		}
	}
	// The console itself loads, so it can render the status page.
	resp, err := http.Get(base + "/ether/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusServiceUnavailable {
		t.Fatalf("console shell: %d", resp.StatusCode)
	}
	if err := stop(); err != nil && !errors.Is(err, context.Canceled) {
		t.Logf("Serve returned %v", err)
	}
}

// writeBundle makes a real backup bundle of a fresh standalone server in
// dataDir/backups and returns its name.
func writeBundle(t *testing.T, dataDir string, edit ...func(*config.Config)) string {
	t.Helper()
	src := t.TempDir()
	db, err := store.Open(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE backup_settings SET retain=42 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Update(func(c *config.Config) error {
		c.Server.FriendlyName = "From backup"
		for _, fn := range edit {
			fn(c)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sn, err := backup.New(db, cfg.Path(), filepath.Join(dataDir, "backups"), 7).Create(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return sn.Name
}

// A start that fails with a damaged database holds on a failed status page
// (never a crash loop) and, on a standalone server, restores a backup from
// it.
func TestServeHoldsAFailedStartAndRestoresFromItsPage(t *testing.T) {
	dataDir := t.TempDir()
	// The restored server must not reach Run (which would start network
	// services): its settings name a transcode folder that cannot be
	// created, so the start after the restore fails again, on purpose.
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	name := writeBundle(t, dataDir, func(c *config.Config) { c.Transcode.TempDir = filepath.Join(blocker, "transcode") })
	if err := os.WriteFile(filepath.Join(dataDir, "ether.db"), bytes.Repeat([]byte("not sqlite "), 2000), 0o600); err != nil {
		t.Fatal(err)
	}
	base, stop := serveInBackground(t, dataDir, freePort(t))
	st, raw := waitPhase(t, base, startup.PhaseFailed)
	if st.Failure == nil || st.Failure.Code != startup.FailDatabaseCorrupt || !st.Failure.Recoverable || st.RetryAt == nil {
		t.Fatalf("failed status = %s", raw)
	}
	resp, err := http.Get(base + "/identity")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("/identity on a failed start: %d", resp.StatusCode)
	}
	// Setup is not complete, so the local network may recover it.
	resp, err = http.Get(base + startup.RecoveryPrefix + "backups")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), name) {
		t.Fatalf("recovery backups: %d %s", resp.StatusCode, body)
	}
	req, _ := http.NewRequest("POST", base+startup.RecoveryPrefix+"restore/snapshot/"+name, nil)
	req.Header.Set("X-Ether-Request", "1")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("recovery restore: %d %s", resp.StatusCode, body)
	}
	// The retry swaps the bundle in at once instead of waiting out the
	// backoff. Its start fails (the blocker), so finding 7: the original
	// data goes back automatically, the report says so, and the server
	// starts again on the original data (which fails again, as before).
	deadline := time.Now().Add(15 * time.Second)
	var rep *backup.Report
	for time.Now().Before(deadline) {
		if rep, _ = backup.ReadReport(dataDir); rep != nil {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if rep == nil || rep.Status != "failed" || !rep.RolledBack || rep.Source != name {
		t.Fatalf("restore report = %+v", rep)
	}
	if b, err := os.ReadFile(filepath.Join(dataDir, "ether.db")); err != nil || !bytes.HasPrefix(b, []byte("not sqlite")) {
		t.Fatalf("original database not put back: %v", err)
	}
	if p, _ := backup.ReadPending(dataDir); p != nil {
		t.Fatalf("restore record left: %+v", p)
	}
	deadline = time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if st, _ := fetchStatus(t, base); st.Phase == startup.PhaseFailed && st.Failure != nil && st.Failure.Code == startup.FailDatabaseCorrupt {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if st, raw := fetchStatus(t, base); st.Phase != startup.PhaseFailed || st.Failure.Code != startup.FailDatabaseCorrupt {
		t.Fatalf("after the rollback: %s", raw)
	}
	// Finding (lock gap): the data directory stays locked across retries.
	if l, err := acquireInstanceLock(dataDir); err == nil {
		l.Release()
		t.Fatal("another instance could take the data directory during a retry")
	}
	_ = stop()
}

// A pending restore is applied by New before the database opens, and the
// report records the restored database's integrity.
func TestNewAppliesAPendingRestore(t *testing.T) {
	dataDir := t.TempDir()
	cfg, err := config.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Update(func(c *config.Config) error {
		c.Transcode.FFmpegPath = "/not-installed/ffmpeg"
		c.Server.Port = 32499
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	a, err := New(context.Background(), Options{DataDir: dataDir, Logger: quietLog()})
	if err != nil {
		t.Fatal(err)
	}
	a.Close()
	name := writeBundle(t, dataDir)
	st, err := backup.StageLocal(context.Background(), dataDir, name)
	if err != nil {
		t.Fatal(err)
	}
	if err := backup.PrepareStaged(dataDir, st.ID, config.Server{Port: 32499}); err != nil {
		t.Fatal(err)
	}
	if err := backup.RequestStaged(dataDir, st.ID); err != nil {
		t.Fatal(err)
	}
	a, err = New(context.Background(), Options{DataDir: dataDir, Logger: quietLog()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if a.restored == nil || a.restored.Status != "applied" || a.restored.Integrity == nil || !a.restored.Integrity.OK {
		t.Fatalf("restored = %+v", a.restored)
	}
	// Finding 7: nothing says "restored" until the server is ready.
	if rep, _ := backup.ReadReport(dataDir); rep != nil {
		t.Fatalf("report before ready: %+v", rep)
	}
	a.confirmRestore() // what Run does right before it serves
	var retain int
	if err := a.db.QueryRow(`SELECT retain FROM backup_settings WHERE id=1`).Scan(&retain); err != nil || retain != 42 {
		t.Fatalf("restored database retain = %d, %v", retain, err)
	}
	if got := a.cfg.Get(); got.Server.FriendlyName != "From backup" || got.Server.Port != 32499 {
		t.Fatalf("restored settings name=%q port=%d", got.Server.FriendlyName, got.Server.Port)
	}
	rep, _ := backup.ReadReport(dataDir)
	if rep == nil || rep.Integrity == nil || !rep.Integrity.OK {
		t.Fatalf("saved report = %+v", rep)
	}
	if h := a.databaseHealth(); h.Integrity == nil || !h.Integrity.OK {
		t.Fatalf("database health = %+v", h)
	}
	// The console's backups API serves the report.
	w := consoleRequest(a, "GET", "/ether/api/adoption/backups/restore", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"restored"`) || !strings.Contains(w.Body.String(), `"oneClick":true`) {
		t.Fatalf("restore status: %d %s", w.Code, w.Body)
	}
}

func consoleRequest(a *App, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://localhost"+path, strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:1024"
	if method != "GET" {
		r.Header.Set("X-Ether-Request", "1")
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
		}
	}
	w := httptest.NewRecorder()
	a.handler.ServeHTTP(w, r)
	return w
}

func newOpsApp(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()
	cfg, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Update(func(c *config.Config) error { c.Transcode.FFmpegPath = "/not-installed/ffmpeg"; return nil }); err != nil {
		t.Fatal(err)
	}
	a, err := New(context.Background(), Options{DataDir: dir, Logger: quietLog()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	return a
}

func TestRestartAPI(t *testing.T) {
	a := newOpsApp(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.bindRestart(ctx, cancel)
	viewers := 2
	a.restart.active = func() int { return viewers }
	restartPoll = 10 * time.Millisecond
	defer func() { restartPoll = 5 * time.Second }()

	w := consoleRequest(a, "GET", "/ether/api/server/restart/", "")
	var st struct {
		Method  string `json:"method"`
		Manager string `json:"manager"`
		Active  int    `json:"active"`
		Waiting *struct {
			Reason string `json:"reason"`
		} `json:"waiting"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &st) != nil || st.Method == "" || st.Manager == "" || st.Active != 2 {
		t.Fatalf("GET restart: %d %s", w.Code, w.Body)
	}
	// CSRF: a mutation without the header is refused.
	r := httptest.NewRequest("POST", "http://localhost/ether/api/server/restart/", nil)
	r.RemoteAddr = "127.0.0.1:1024"
	rec := httptest.NewRecorder()
	a.handler.ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden || ctx.Err() != nil {
		t.Fatalf("restart without CSRF header: %d", rec.Code)
	}

	// Held until nobody watches, and cancellable.
	w = consoleRequest(a, "POST", "/ether/api/server/restart/", `{"whenIdle":true}`)
	if w.Code != http.StatusAccepted || json.Unmarshal(w.Body.Bytes(), &st) != nil || st.Waiting == nil {
		t.Fatalf("POST whenIdle: %d %s", w.Code, w.Body)
	}
	time.Sleep(50 * time.Millisecond)
	if ctx.Err() != nil {
		t.Fatal("restarted while viewers were watching")
	}
	if w := consoleRequest(a, "DELETE", "/ether/api/server/restart/", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"cancelled":true`) {
		t.Fatalf("DELETE: %d %s", w.Code, w.Body)
	}
	if w := consoleRequest(a, "POST", "/ether/api/server/restart/", `{"whenIdle":true}`); w.Code != http.StatusAccepted {
		t.Fatalf("POST whenIdle again: %d", w.Code)
	}
	viewers = 0
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("held restart did not run once playback ended")
	}
	if err := a.runResult(); !errors.Is(err, service.ErrRestartRequested) {
		t.Fatalf("runResult = %v", err)
	}
}

func TestRestartNowCancelsRun(t *testing.T) {
	a := newOpsApp(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.bindRestart(ctx, cancel)
	a.restart.active = func() int { return 3 }
	if w := consoleRequest(a, "POST", "/ether/api/server/restart/", `{"whenIdle":false}`); w.Code != http.StatusAccepted {
		t.Fatalf("POST: %d %s", w.Code, w.Body)
	}
	if ctx.Err() == nil || !errors.Is(a.runResult(), service.ErrRestartRequested) {
		t.Fatal("restart now did not stop Run")
	}
}

func TestIntegrityFailureShowsInDatabaseHealth(t *testing.T) {
	a := newOpsApp(t)
	if h := a.databaseHealth(); h.Integrity != nil {
		t.Fatalf("integrity before any check: %+v", h.Integrity)
	}
	a.recordIntegrity(store.Integrity{Findings: []string{"*** in database main ***\nPage 7: btreeInitPage() returns error code 11"}, Checked: time.Now()})
	w := consoleRequest(a, "GET", "/ether/api/status/database", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"ok":false`) || !strings.Contains(w.Body.String(), "btreeInitPage") {
		t.Fatalf("database status: %d %s", w.Code, w.Body)
	}
}

func TestStartFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		err      error
		postgres bool
		want     string
	}{
		{errors.New("file is not a database (26)"), false, startup.FailDatabaseCorrupt},
		{fmt.Errorf("migration catalog/9: %w", errors.New("no such column")), false, startup.FailMigration},
		{errors.New("permission denied"), false, startup.FailUnknown},
		{errors.New("password authentication failed"), true, startup.FailDatabaseRejected},
	} {
		if got := openFailure(tc.err, tc.postgres); got != tc.want {
			t.Errorf("openFailure(%v, %v) = %s, want %s", tc.err, tc.postgres, got, tc.want)
		}
	}
	f := failureOf(fmt.Errorf("start: %w", startFail(startup.FailCredentialKey, errCredentialKeyLost)), true)
	if f.Code != startup.FailCredentialKey || !f.Recoverable || f.Detail == "" {
		t.Fatalf("failureOf = %+v", f)
	}
	if f := failureOf(errors.New("boom"), false); f.Code != startup.FailUnknown || f.Recoverable {
		t.Fatalf("failureOf(plain) = %+v", f)
	}
	if d := startRetryDelay(1); d != 15*time.Second {
		t.Fatalf("first retry %v", d)
	}
	if d := startRetryDelay(50); d != 10*time.Minute {
		t.Fatalf("retry cap %v", d)
	}
}

// fakePlex is a plex.tv sign-in that approves at once as account id.
type fakePlex struct{ id int64 }

func (f fakePlex) CreatePIN(context.Context) (*plextv.PIN, error) {
	return &plextv.PIN{ID: 7, Code: "ABCD"}, nil
}
func (f fakePlex) AuthURL(*plextv.PIN, string) string { return "https://app.plex.tv/auth#?code=ABCD" }
func (f fakePlex) CheckPIN(context.Context, int64, string) (*plextv.PIN, error) {
	return &plextv.PIN{ID: 7, AuthToken: "plex-account-token"}, nil
}
func (f fakePlex) User(context.Context, string) (*plextv.Account, error) {
	return &plextv.Account{ID: f.id}, nil
}

func recoveryCall(h http.Handler, method, path, token string, header map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://192.168.1.10:32400"+startup.RecoveryPrefix+path, nil)
	r.RemoteAddr = "192.168.1.50:5000"
	if method != "GET" {
		r.Header.Set("X-Ether-Request", "1")
	}
	if token != "" {
		r.Header.Set(recoveryTokenHeader, token)
	}
	for k, v := range header {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
