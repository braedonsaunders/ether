package startup

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newGate(t *testing.T) (*Gate, *time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	tr := NewTracker("1.2.3", func() time.Time { return now })
	g := &Gate{
		Tracker:  tr,
		Shell:    http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("shell")) }),
		Recovery: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("recovery")) }),
	}
	return g, &now
}

func get(h http.Handler, method, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(method, path, nil))
	return w
}

func status(t *testing.T, h http.Handler) Status {
	t.Helper()
	w := get(h, "GET", StatusPath)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status: %d %v", w.Code, w.Header())
	}
	var s Status
	if err := json.Unmarshal(w.Body.Bytes(), &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestGateBeforeReadyServesOnlyStatusAndShell(t *testing.T) {
	g, _ := newGate(t)
	if s := status(t, g); s.Phase != PhaseStarting || s.Ready || s.Version != "1.2.3" {
		t.Fatalf("status = %+v", s)
	}
	if w := get(g, "GET", "/ether/maintenance/backups"); w.Code != 200 || w.Body.String() != "shell" {
		t.Fatalf("console shell: %d %s", w.Code, w.Body)
	}
	if w := get(g, "GET", "/ether"); w.Code != http.StatusFound {
		t.Fatalf("/ether: %d", w.Code)
	}
	// The Plex API, the console API and every write wait for the server.
	for _, c := range []struct{ method, path string }{
		{"GET", "/identity"}, {"GET", "/library/sections"}, {"GET", "/:/websockets/notifications"},
		{"GET", "/ether/api/bootstrap"}, {"POST", "/ether/api/config"}, {"POST", "/ether/maintenance"},
		{"POST", StatusPath}, {"GET", RecoveryPrefix + "backups"},
	} {
		w := get(g, c.method, c.path)
		if c.path == StatusPath {
			if w.Code != http.StatusMethodNotAllowed {
				t.Fatalf("POST status: %d", w.Code)
			}
			continue
		}
		if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" {
			t.Fatalf("%s %s before ready: %d %v", c.method, c.path, w.Code, w.Header())
		}
	}
	if w := get(g, "GET", "/ether/api/bootstrap"); !strings.Contains(w.Body.String(), `"code":"starting"`) {
		t.Fatalf("console API 503 body: %s", w.Body)
	}
}

func TestGatePhasesAndProgress(t *testing.T) {
	g, now := newGate(t)
	tr := g.Tracker
	tr.SetDatabase(Database{Kind: "postgres", Host: "db.lan:5432"})
	*now = now.Add(2 * time.Second)
	tr.Waiting(PhaseWaitingDatabase, 3, 8*time.Second, errors.New("dial tcp: connect: connection refused"))
	s := status(t, g)
	if s.Phase != PhaseWaitingDatabase || s.Attempt != 3 || s.RetryAt == nil || s.Database.Host != "db.lan:5432" ||
		s.Failure == nil || s.Failure.Code != FailDatabaseUnreachable || s.Elapsed != 2 {
		t.Fatalf("waiting = %+v", s)
	}
	tr.Set(PhaseMigrating)
	tr.Progress(4, 12)
	if s := status(t, g); s.Phase != PhaseMigrating || s.Step != 4 || s.Steps != 12 || s.Failure != nil || s.RetryAt != nil {
		t.Fatalf("migrating = %+v", s)
	}
	tr.Restoring("ether-20261030.tar.gz")
	if s := status(t, g); s.Phase != PhaseRestoring || s.Restoring != "ether-20261030.tar.gz" || s.Step != 0 {
		t.Fatalf("restoring = %+v", s)
	}
	tr.Set(PhaseStartingServices)
	if s := status(t, g); s.Restoring != "" || s.Message == "" {
		t.Fatalf("starting services = %+v", s)
	}
}

func TestStatusNeverCarriesCredentials(t *testing.T) {
	g, _ := newGate(t)
	g.Tracker.Waiting(PhaseWaitingDatabase, 1, time.Second,
		errors.New(`failed to connect to postgres://ether:hunter2secret@db.lan:5432/ether?password=hunter2secret`))
	g.Tracker.Fail(Failure{Code: FailDatabaseRejected, Detail: "connect postgres://ether:hunter2secret@db.lan/ether: password authentication failed"}, 2, time.Minute)
	w := get(g, "GET", StatusPath)
	if strings.Contains(w.Body.String(), "hunter2secret") {
		t.Fatalf("status leaks a password: %s", w.Body)
	}
	g.Tracker.Fail(Failure{Code: FailUnknown, Detail: strings.Repeat("x", 5000)}, 1, 0)
	if s := status(t, g); len(s.Failure.Detail) > maxDetail+4 || s.RetryAt != nil {
		t.Fatalf("detail not bounded: %d", len(s.Failure.Detail))
	}
}

func TestGateRecoveryOnlyWhileFailed(t *testing.T) {
	g, _ := newGate(t)
	if w := get(g, "GET", RecoveryPrefix+"backups"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("recovery while starting: %d", w.Code)
	}
	g.Tracker.Fail(Failure{Code: FailDatabaseCorrupt, Recoverable: true}, 1, time.Minute)
	if w := get(g, "GET", RecoveryPrefix+"backups"); w.Code != 200 || w.Body.String() != "recovery" {
		t.Fatalf("recovery while failed: %d %s", w.Code, w.Body)
	}
	s := status(t, g)
	if s.Phase != PhaseFailed || !s.Failure.Recoverable || s.RetryAt == nil {
		t.Fatalf("failed = %+v", s)
	}
	g.Open(http.NotFoundHandler())
	if w := get(g, "GET", RecoveryPrefix+"backups"); w.Code != http.StatusNotFound {
		t.Fatalf("recovery after ready reached the gate: %d", w.Code)
	}
}

func TestGateConsolePolicyWrapsItsOwnRoutes(t *testing.T) {
	g, _ := newGate(t)
	g.Console = func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "lan only", http.StatusForbidden) })
	}
	for _, p := range []string{StatusPath, "/ether/", "/ether/api/bootstrap"} {
		if w := get(g, "GET", p); w.Code != http.StatusForbidden {
			t.Fatalf("%s: %d", p, w.Code)
		}
	}
	// Plex clients are not console users.
	if w := get(g, "GET", "/identity"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("/identity: %d", w.Code)
	}
}

func TestGateOpenAndCloseDrains(t *testing.T) {
	g, _ := newGate(t)
	release := make(chan struct{})
	entered := make(chan struct{})
	g.Open(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			close(entered)
			<-release
		}
		w.Write([]byte("app"))
	}))
	if s := status(t, g); !s.Ready || s.Phase != PhaseReady {
		t.Fatalf("after open = %+v", s)
	}
	if w := get(g, "GET", "/identity"); w.Body.String() != "app" {
		t.Fatalf("ready request: %s", w.Body)
	}
	go get(g, "GET", "/slow")
	<-entered
	closed := make(chan struct{})
	go func() { g.Close(context.Background()); close(closed) }()
	time.Sleep(20 * time.Millisecond)
	select {
	case <-closed:
		t.Fatal("Close returned with a request in flight")
	default:
	}
	// New requests already see the stopping server.
	if w := get(g, "GET", "/identity"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("request while stopping: %d", w.Code)
	}
	if s := status(t, g); s.Phase != PhaseStopping {
		t.Fatalf("phase while stopping = %s", s.Phase)
	}
	close(release)
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return after the request finished")
	}
	// A deadline bounds the wait.
	g.Open(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("GET", "/stream", nil).WithContext(ctx)
	go g.ServeHTTP(httptest.NewRecorder(), req)
	time.Sleep(10 * time.Millisecond)
	short, done := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer done()
	g.Close(short)
	cancel()
}
