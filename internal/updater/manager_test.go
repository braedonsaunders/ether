package updater

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
)

// fakeClock is a manually advanced clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// fakeChecker serves a fixed update and "downloads" its payload.
type fakeChecker struct {
	mu        sync.Mutex
	upd       *Update
	payload   []byte
	checks    int
	downloads int
	checkErr  error
	dlErr     error
}

func (f *fakeChecker) Check(context.Context) (*Update, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks++
	return f.upd, f.checkErr
}

func (f *fakeChecker) Download(_ context.Context, upd *Update, dir string, _ ProgressFunc) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.downloads++
	if f.dlErr != nil {
		return "", f.dlErr
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	p := filepath.Join(dir, upd.Artifact.Filename)
	return p, os.WriteFile(p, f.payload, 0o644)
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func binUpdate(version string, payload []byte) *Update {
	return &Update{
		Manifest: &Manifest{Version: version, Channel: ChannelStable},
		Artifact: &Artifact{Kind: KindBinary, SHA256: shaHex(payload), Size: int64(len(payload)), Filename: "ether-" + version},
	}
}

type harness struct {
	t        *testing.T
	clock    *fakeClock
	checker  *fakeChecker
	cfg      config.Updates
	active   int
	dir      string
	target   string
	applied  []string
	restarts []string
	m        *Manager
}

// newHarness builds a self-applying manager at 2026-01-10 12:00 UTC with
// running version 1.0.0 and a 2.0.0 binary on the feed.
func newHarness(t *testing.T, mutate func(*ManagerOptions)) *harness {
	t.Helper()
	root := t.TempDir()
	h := &harness{
		t:     t,
		clock: &fakeClock{t: time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)},
		cfg:   config.Updates{Enabled: true, Channel: ChannelStable, AutoUpdate: true, MaintenanceStart: "03:00", MaintenanceEnd: "06:00"},
		dir:   filepath.Join(root, "updates"),
	}
	h.target = filepath.Join(root, "bin", "ether")
	if err := os.MkdirAll(filepath.Dir(h.target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.target, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	payload := []byte("new binary 2.0.0")
	h.checker = &fakeChecker{upd: binUpdate("2.0.0", payload), payload: payload}
	opts := ManagerOptions{
		NewClient:      func(config.Updates) (Checker, error) { return h.checker, nil },
		Config:         func() config.Updates { return h.cfg },
		CurrentVersion: "1.0.0",
		Dir:            h.dir,
		Target:         h.target,
		SelfApply:      true,
		ActiveSessions: func() int { return h.active },
		RequestRestart: func(r string) { h.restarts = append(h.restarts, r) },
		Now:            h.clock.Now,
		Location:       time.UTC,
		ApplyBinary: func(newPath, target string) error {
			h.applied = append(h.applied, newPath)
			return ApplyBinary(newPath, target)
		},
		Logger: quietLog(),
	}
	if mutate != nil {
		mutate(&opts)
	}
	h.m = NewManager(opts)
	h.m.begin()
	return h
}

// run advances the clock in tick-sized steps for d, stepping each time.
func (h *harness) run(d time.Duration) {
	h.t.Helper()
	for end := h.clock.Now().Add(d); h.clock.Now().Before(end); {
		h.clock.Advance(DefaultTick)
		h.m.step(context.Background())
	}
}

func (h *harness) state() string { return h.m.Status().State }

func TestInWindowWrapAndDefaults(t *testing.T) {
	at := func(hm string) time.Time {
		tt, _ := time.Parse("15:04", hm)
		return time.Date(2026, 1, 1, tt.Hour(), tt.Minute(), 0, 0, time.UTC)
	}
	cases := []struct {
		start, end, now string
		want            bool
	}{
		{"03:00", "06:00", "02:59", false},
		{"03:00", "06:00", "03:00", true},
		{"03:00", "06:00", "05:59", true},
		{"03:00", "06:00", "06:00", false},
		{"23:00", "02:00", "23:30", true},
		{"23:00", "02:00", "01:59", true},
		{"23:00", "02:00", "02:00", false},
		{"23:00", "02:00", "12:00", false},
		{"04:00", "04:00", "17:00", true}, // equal: always open
		{"", "", "04:00", true},           // defaults 03:00-06:00
		{"", "", "07:00", false},
		{"bogus", "25:00", "03:30", true}, // invalid falls back to defaults
	}
	for _, c := range cases {
		cfg := config.Updates{MaintenanceStart: c.start, MaintenanceEnd: c.end}
		if got := inWindow(at(c.now), cfg); got != c.want {
			t.Errorf("window %q-%q at %s: got %v want %v", c.start, c.end, c.now, got, c.want)
		}
	}
}

func TestManagerWaitsForWindowThenIdle(t *testing.T) {
	h := newHarness(t, nil)
	h.run(DefaultFirstCheckDelay)
	if h.checker.checks != 1 || h.checker.downloads != 1 {
		t.Fatalf("checks=%d downloads=%d; want early check + download", h.checker.checks, h.checker.downloads)
	}
	st := h.m.Status()
	if st.State != StateWaitingWindow || st.Available == nil || !st.Available.Staged || st.Capability != CapabilitySelfApply {
		t.Fatalf("status = %+v", st)
	}
	// Noon → 03:00: still outside the window the whole afternoon.
	h.active = 1
	h.run(14*time.Hour + 50*time.Minute)
	if h.state() != StateWaitingWindow || len(h.applied) != 0 {
		t.Fatalf("state=%s applied=%v before window", h.state(), h.applied)
	}
	// Inside the window but a stream is playing.
	h.run(20 * time.Minute)
	if h.state() != StateWaitingIdle || len(h.applied) != 0 {
		t.Fatalf("state=%s applied=%v while busy", h.state(), h.applied)
	}
	// Stream stops: still needs the full quiet period.
	h.active = 0
	h.run(DefaultQuietPeriod - time.Minute)
	if h.state() != StateWaitingIdle || len(h.applied) != 0 {
		t.Fatalf("state=%s applied=%v before quiet period", h.state(), h.applied)
	}
	h.run(2 * time.Minute)
	if len(h.applied) != 1 || len(h.restarts) != 1 {
		t.Fatalf("applied=%v restarts=%v; want one apply + restart", h.applied, h.restarts)
	}
	if b, _ := os.ReadFile(h.target); string(b) != "new binary 2.0.0" {
		t.Fatalf("target = %q", b)
	}
	if b, _ := os.ReadFile(h.target + ".old"); string(b) != "old binary" {
		t.Fatalf("rollback copy = %q", b)
	}
	var p Pending
	if ok, err := readJSON(PendingPath(h.dir), &p); !ok || err != nil {
		t.Fatalf("marker missing: %v", err)
	}
	if p.From != "1.0.0" || p.To != "2.0.0" || p.Target != h.target || p.Attempts != 0 {
		t.Fatalf("marker = %+v", p)
	}
	// Restart requested once; further steps do nothing.
	h.run(time.Hour)
	if len(h.applied) != 1 || len(h.restarts) != 1 {
		t.Fatalf("re-applied: %v %v", h.applied, h.restarts)
	}
}

func TestManagerWindowWrapsMidnight(t *testing.T) {
	h := newHarness(t, nil)
	h.cfg.MaintenanceStart, h.cfg.MaintenanceEnd = "23:00", "01:00"
	h.run(10 * time.Hour) // 22:00
	if len(h.applied) != 0 {
		t.Fatal("applied outside window")
	}
	h.run(90 * time.Minute) // 23:30
	if len(h.applied) != 1 {
		t.Fatalf("not applied inside wrapped window; state=%s", h.state())
	}
}

func TestManagerNoAutoUpdateOnlyNotifies(t *testing.T) {
	h := newHarness(t, nil)
	h.cfg.AutoUpdate = false
	h.run(24 * time.Hour)
	st := h.m.Status()
	if h.checker.downloads != 0 || len(h.applied) != 0 || st.State != StateIdle || st.Available == nil || st.Available.Version != "2.0.0" {
		t.Fatalf("downloads=%d applied=%v status=%+v", h.checker.downloads, h.applied, st)
	}
}

func TestManagerUpdateNowSkipsWindowButWaitsForIdle(t *testing.T) {
	h := newHarness(t, nil)
	h.cfg.AutoUpdate = false
	h.active = 2
	if err := h.m.UpdateNow(false); err != nil {
		t.Fatal(err)
	}
	h.run(DefaultTick) // checks immediately (no first-check delay), downloads
	if h.checker.checks != 1 || h.checker.downloads != 1 || h.state() != StateWaitingIdle {
		t.Fatalf("checks=%d downloads=%d state=%s", h.checker.checks, h.checker.downloads, h.state())
	}
	h.run(time.Hour)
	if len(h.applied) != 0 {
		t.Fatal("applied while streams active")
	}
	h.active = 0
	h.run(DefaultQuietPeriod + time.Minute)
	if len(h.applied) != 1 || len(h.restarts) != 1 {
		t.Fatalf("applied=%v restarts=%v at 13:xx after Update now", h.applied, h.restarts)
	}
}

func TestManagerForceSkipsIdle(t *testing.T) {
	h := newHarness(t, nil)
	h.active = 3
	if err := h.m.UpdateNow(true); err != nil {
		t.Fatal(err)
	}
	h.run(DefaultTick)
	if len(h.applied) != 1 || len(h.restarts) != 1 {
		t.Fatalf("force did not apply immediately: state=%s applied=%v", h.state(), h.applied)
	}
}

func TestManagerNotifyOnlyCapability(t *testing.T) {
	h := newHarness(t, func(o *ManagerOptions) {
		o.SelfApply, o.NotifyReason = false, "installed by the system package manager; update with it"
	})
	h.run(24 * time.Hour)
	st := h.m.Status()
	if st.Capability != CapabilityNotifyOnly || !strings.Contains(st.CapabilityReason, "package manager") {
		t.Fatalf("capability = %q (%q)", st.Capability, st.CapabilityReason)
	}
	if h.checker.downloads != 0 || len(h.applied) != 0 || st.Available == nil {
		t.Fatalf("notify-only must not download/apply: %+v", st)
	}
	if err := h.m.UpdateNow(true); !errors.Is(err, ErrNotSelfApplicable) {
		t.Fatalf("UpdateNow err = %v", err)
	}
}

func TestManagerInstallerArtifactIsNotifyOnly(t *testing.T) {
	h := newHarness(t, nil)
	h.checker.upd.Artifact.Kind = KindInstaller
	h.run(24 * time.Hour)
	st := h.m.Status()
	if st.Capability != CapabilityNotifyOnly || h.checker.downloads != 0 || len(h.applied) != 0 {
		t.Fatalf("status=%+v downloads=%d", st, h.checker.downloads)
	}
}

func TestManagerDisabledWithoutKey(t *testing.T) {
	h := newHarness(t, func(o *ManagerOptions) {
		o.NewClient = func(config.Updates) (Checker, error) { return nil, ErrNoPublicKey }
	})
	h.run(48 * time.Hour)
	st := h.m.Status()
	if st.Enabled || st.Capability != CapabilityDisabled || h.checker.checks != 0 {
		t.Fatalf("status=%+v checks=%d", st, h.checker.checks)
	}
	if err := h.m.UpdateNow(false); !errors.Is(err, ErrUpdatesDisabled) {
		t.Fatalf("UpdateNow err = %v", err)
	}
}

func TestManagerDisabledBySettingsToggle(t *testing.T) {
	h := newHarness(t, nil)
	h.cfg.Enabled = false
	h.run(48 * time.Hour)
	st := h.m.Status()
	if st.Enabled || st.Capability != CapabilityDisabled || h.checker.checks != 0 || h.checker.downloads != 0 {
		t.Fatalf("disabled by settings ran checks: status=%+v checks=%d downloads=%d", st, h.checker.checks, h.checker.downloads)
	}
	if err := h.m.UpdateNow(false); !errors.Is(err, ErrUpdatesDisabled) {
		t.Fatalf("UpdateNow while disabled = %v; want ErrUpdatesDisabled", err)
	}

	// Re-enabling checks in settings immediately checks on the next step.
	h.cfg.Enabled = true
	h.run(DefaultTick)
	st = h.m.Status()
	if !st.Enabled || st.Capability != CapabilitySelfApply || h.checker.checks != 1 {
		t.Fatalf("re-enabled status=%+v checks=%d", st, h.checker.checks)
	}

	// Turning checks off again clears available state and stops further checks.
	h.cfg.Enabled = false
	h.run(48 * time.Hour)
	st = h.m.Status()
	if st.Enabled || st.Available != nil || h.checker.checks != 1 {
		t.Fatalf("turned off again: status=%+v checks=%d", st, h.checker.checks)
	}
}

func TestManagerDownloadAndCheckRetry(t *testing.T) {
	h := newHarness(t, nil)
	h.checker.dlErr = errors.New("connection reset")
	h.run(DefaultFirstCheckDelay)
	if st := h.m.Status(); !strings.Contains(st.LastError, "connection reset") || h.checker.downloads != 1 {
		t.Fatalf("status=%+v downloads=%d", st, h.checker.downloads)
	}
	h.run(30 * time.Minute)
	if h.checker.downloads != 1 {
		t.Fatalf("retried too soon: %d", h.checker.downloads)
	}
	h.checker.dlErr = nil
	h.run(31 * time.Minute)
	if h.checker.downloads != 2 || h.m.Status().LastError != "" {
		t.Fatalf("downloads=%d err=%q", h.checker.downloads, h.m.Status().LastError)
	}

	h2 := newHarness(t, nil)
	h2.checker.checkErr = errors.New("feed down")
	h2.run(DefaultFirstCheckDelay + DefaultRetryInterval)
	if h2.checker.checks != 2 {
		t.Fatalf("check retry: checks=%d", h2.checker.checks)
	}
}

func TestManagerStaleDownloadRemovedAndCorruptStageRedownloaded(t *testing.T) {
	h := newHarness(t, nil)
	if err := os.MkdirAll(h.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(h.dir, "ether-1.5.0")
	_ = os.WriteFile(stale, []byte("old"), 0o644)
	h.run(DefaultFirstCheckDelay)
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale download not cleaned")
	}
	// Corrupt the staged file before the window opens.
	staged := filepath.Join(h.dir, "ether-2.0.0")
	if err := os.WriteFile(staged, []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.run(15*time.Hour - 5*time.Minute) // exactly 03:00, idle since noon
	if len(h.applied) != 0 {
		t.Fatal("applied a file that failed verification")
	}
	if !strings.Contains(h.m.Status().LastError, "verification") {
		t.Fatalf("lastError = %q", h.m.Status().LastError)
	}
	h.run(20 * time.Minute)
	if len(h.applied) != 0 || h.checker.downloads != 2 {
		t.Fatalf("applied=%v downloads=%d; want redownload but apply backoff", h.applied, h.checker.downloads)
	}
	h.run(DefaultRetryInterval)
	if len(h.applied) != 1 || h.checker.downloads != 2 {
		t.Fatalf("applied=%v downloads=%d after redownload", h.applied, h.checker.downloads)
	}
}

func TestManagerApplyFailureClearsMarker(t *testing.T) {
	h := newHarness(t, func(o *ManagerOptions) {
		o.ApplyBinary = func(string, string) error { return errors.New("access denied") }
	})
	if err := h.m.UpdateNow(true); err != nil {
		t.Fatal(err)
	}
	h.run(DefaultTick)
	if len(h.restarts) != 0 {
		t.Fatal("restart after failed apply")
	}
	if _, err := os.Stat(PendingPath(h.dir)); !os.IsNotExist(err) {
		t.Fatal("marker left behind after failed apply")
	}
	if !strings.Contains(h.m.Status().LastError, "access denied") {
		t.Fatalf("lastError = %q", h.m.Status().LastError)
	}
}

// startup simulates a process start of version current against dir.
func startup(t *testing.T, dir, target, current string, now time.Time) StartupResult {
	t.Helper()
	res, err := ConfirmStartup(StartupOptions{
		Dir: dir, Target: target, CurrentVersion: current,
		Now: func() time.Time { return now }, Logger: quietLog(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestStartupConfirmAfterHealthPeriod(t *testing.T) {
	h := newHarness(t, nil)
	if err := h.m.UpdateNow(true); err != nil {
		t.Fatal(err)
	}
	h.run(DefaultTick)
	if len(h.restarts) != 1 {
		t.Fatal("no restart")
	}
	// New process: version 2.0.0.
	res := startup(t, h.dir, h.target, "2.0.0", h.clock.Now())
	if res.Confirming == nil || res.Confirming.Attempts != 1 || res.RestartRequired {
		t.Fatalf("startup = %+v", res)
	}
	h.checker.upd = nil // feed says up to date
	m2 := NewManager(ManagerOptions{
		NewClient: func(config.Updates) (Checker, error) { return h.checker, nil },
		Config:    func() config.Updates { return h.cfg }, CurrentVersion: "2.0.0",
		Dir: h.dir, Target: h.target, SelfApply: true, Boot: res,
		Now: h.clock.Now, Location: time.UTC, Logger: quietLog(),
	})
	m2.begin()
	h.clock.Advance(DefaultHealthPeriod - time.Minute)
	m2.step(context.Background())
	if !m2.Status().Confirming {
		t.Fatal("confirmed too early")
	}
	if _, err := os.Stat(PendingPath(h.dir)); err != nil {
		t.Fatal("marker removed before health period")
	}
	h.clock.Advance(2 * time.Minute)
	m2.step(context.Background())
	if m2.Status().Confirming {
		t.Fatal("not confirmed after health period")
	}
	for _, p := range []string{PendingPath(h.dir), h.target + ".old", filepath.Join(h.dir, "ether-2.0.0")} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s should be gone after confirmation", p)
		}
	}
}

// A newer release staged while the current update is still confirming must
// survive the confirmation's cleanup of old downloads.
func TestConfirmationKeepsNewerStagedRelease(t *testing.T) {
	h := newHarness(t, nil)
	if err := h.m.UpdateNow(true); err != nil {
		t.Fatal(err)
	}
	h.run(DefaultTick)
	res := startup(t, h.dir, h.target, "2.0.0", h.clock.Now())
	if res.Confirming == nil {
		t.Fatalf("startup = %+v", res)
	}
	payload := []byte("new binary 3.0.0")
	h.checker.upd, h.checker.payload = binUpdate("3.0.0", payload), payload
	h.checker.downloads = 0
	m2 := NewManager(ManagerOptions{
		NewClient: func(config.Updates) (Checker, error) { return h.checker, nil },
		Config:    func() config.Updates { return h.cfg }, CurrentVersion: "2.0.0",
		Dir: h.dir, Target: h.target, SelfApply: true, Boot: res,
		Now: h.clock.Now, Location: time.UTC, Logger: quietLog(),
	})
	m2.begin()
	// The first check (5 min) stages 3.0.0 before the health period (10 min) ends.
	for end := h.clock.Now().Add(DefaultHealthPeriod + time.Minute); h.clock.Now().Before(end); {
		h.clock.Advance(DefaultTick)
		m2.step(context.Background())
	}
	st := m2.Status()
	if st.Confirming || st.Available == nil || !st.Available.Staged || h.checker.downloads != 1 {
		t.Fatalf("status = %+v downloads=%d", st, h.checker.downloads)
	}
	if _, err := os.Stat(filepath.Join(h.dir, "ether-3.0.0")); err != nil {
		t.Fatalf("staged 3.0.0 removed by confirmation cleanup: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.dir, "ether-2.0.0")); !os.IsNotExist(err) {
		t.Fatal("confirmed update's download not cleaned")
	}
}

func TestStartupRollsBackAfterCrashLoopAndSkipsVersion(t *testing.T) {
	h := newHarness(t, nil)
	if err := h.m.UpdateNow(true); err != nil {
		t.Fatal(err)
	}
	h.run(DefaultTick)
	now := h.clock.Now()
	// Three starts that never confirm (crash before the health period).
	for i := 1; i <= DefaultMaxStartAttempts; i++ {
		res := startup(t, h.dir, h.target, "2.0.0", now)
		if res.Confirming == nil || res.Confirming.Attempts != i || res.RestartRequired {
			t.Fatalf("start %d: %+v", i, res)
		}
	}
	// Fourth start: crash loop → roll back.
	res := startup(t, h.dir, h.target, "2.0.0", now)
	if !res.RestartRequired || res.RolledBack == nil || res.RolledBack.Version != "2.0.0" {
		t.Fatalf("expected rollback, got %+v", res)
	}
	if b, _ := os.ReadFile(h.target); string(b) != "old binary" {
		t.Fatalf("target after rollback = %q", b)
	}
	if _, err := os.Stat(PendingPath(h.dir)); !os.IsNotExist(err) {
		t.Fatal("marker survives rollback")
	}
	// Old version restarts: no marker, failed version recorded.
	res = startup(t, h.dir, h.target, "1.0.0", now)
	if res.Confirming != nil || res.RestartRequired {
		t.Fatalf("restored start: %+v", res)
	}
	h.checker.upd = binUpdate("2.0.0", h.checker.payload)
	h.checker.downloads = 0
	m := NewManager(ManagerOptions{
		NewClient: func(config.Updates) (Checker, error) { return h.checker, nil },
		Config:    func() config.Updates { return h.cfg }, CurrentVersion: "1.0.0",
		Dir: h.dir, Target: h.target, SelfApply: true, Boot: res,
		Now: h.clock.Now, Location: time.UTC, Logger: quietLog(),
	})
	m.begin()
	h.clock.Advance(DefaultFirstCheckDelay)
	m.step(context.Background())
	st := m.Status()
	if st.State != StateRolledBack || st.FailedVersion != "2.0.0" || st.Available != nil || h.checker.downloads != 0 {
		t.Fatalf("failed version not skipped: %+v downloads=%d", st, h.checker.downloads)
	}
	// A newer release is offered again.
	h.checker.upd = binUpdate("2.0.1", []byte("fixed"))
	h.checker.payload = []byte("fixed")
	h.clock.Advance(DefaultPollInterval)
	m.step(context.Background())
	if st := m.Status(); st.Available == nil || st.Available.Version != "2.0.1" || h.checker.downloads != 1 {
		t.Fatalf("newer release not taken: %+v downloads=%d", st, h.checker.downloads)
	}
}

func TestStartupSwapDidNotTake(t *testing.T) {
	dir := t.TempDir()
	if err := writeJSONAtomic(PendingPath(dir), &Pending{From: "1.0.0", To: "2.0.0", Target: "/nonexistent/ether"}); err != nil {
		t.Fatal(err)
	}
	res := startup(t, dir, "/nonexistent/ether", "1.0.0", time.Now())
	if res.Confirming != nil || res.RestartRequired || !strings.Contains(res.Note, "did not take effect") {
		t.Fatalf("res = %+v", res)
	}
	if _, err := os.Stat(PendingPath(dir)); !os.IsNotExist(err) {
		t.Fatal("marker not cleared")
	}
}

func TestStartupCleansLeftoversWithoutMarker(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "ether")
	for _, p := range []string{target, target + ".old", target + ".bad"} {
		if err := os.WriteFile(p, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	_ = writeJSONAtomic(FailedPath(dir), &FailedUpdate{Version: "1.5.0"})
	startup(t, dir, target, "1.5.0", time.Now())
	for _, p := range []string{target + ".old", target + ".bad", FailedPath(dir)} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s not cleaned", p)
		}
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatal("removed the binary itself")
	}
}

func TestSelfApplyCapability(t *testing.T) {
	yes := func(string) bool { return true }
	no := func(string) bool { return false }
	base := Environment{GOOS: "windows", Target: `C:\Program Files\Ether\ether.exe`, Supervised: true, DirWritable: yes}
	if ok, why := SelfApplyCapability(base); !ok {
		t.Fatalf("windows service should self-apply: %s", why)
	}
	cases := map[string]func(e *Environment){
		"development":     func(e *Environment) { e.Dev = true },
		"macOS":           func(e *Environment) { e.GOOS = "darwin" },
		"container":       func(e *Environment) { e.InContainer = true },
		"package manager": func(e *Environment) { e.GOOS, e.Target = "linux", "/usr/lib/ether/ether" },
		"not running as":  func(e *Environment) { e.Supervised = false },
		"not writable":    func(e *Environment) { e.DirWritable = no },
	}
	for want, mutate := range cases {
		e := base
		mutate(&e)
		ok, why := SelfApplyCapability(e)
		if ok || !strings.Contains(why, want) {
			t.Errorf("%s: ok=%v why=%q", want, ok, why)
		}
	}
	linuxTarball := Environment{GOOS: "linux", Target: "/home/me/ether/ether", Supervised: true, DirWritable: yes}
	if ok, why := SelfApplyCapability(linuxTarball); !ok {
		t.Errorf("writable linux tarball install: %s", why)
	}
	if !DirWritable(t.TempDir()) {
		t.Error("temp dir should be writable")
	}
}

func TestManagerRunStopsAfterRestart(t *testing.T) {
	h := newHarness(t, func(o *ManagerOptions) {
		o.Tick = time.Millisecond
		o.Now = time.Now
	})
	if err := h.m.UpdateNow(true); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { h.m.Run(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after requesting a restart")
	}
	if len(h.restarts) != 1 {
		t.Fatalf("restarts = %v", h.restarts)
	}
}

func TestManagerRebuildsClientOnChannelChange(t *testing.T) {
	var built []string
	h := newHarness(t, nil)
	h.m.opts.NewClient = func(u config.Updates) (Checker, error) {
		built = append(built, u.Channel)
		return h.checker, nil
	}
	h.run(DefaultFirstCheckDelay)
	if h.checker.checks != 1 || len(built) != 0 {
		t.Fatalf("checks=%d built=%v", h.checker.checks, built)
	}
	h.cfg.Channel = ChannelBeta
	h.run(DefaultTick)
	if h.checker.checks != 2 || len(built) != 1 || built[0] != ChannelBeta {
		t.Fatalf("channel change: checks=%d built=%v", h.checker.checks, built)
	}
}

// Use a real clock value with monotonic metadata: a fake time.Date alone cannot
// catch deadlines that accidentally retain elapsed-time semantics across sleep.
func TestManagerWallClockResumeAndRetry(t *testing.T) {
	now := time.Now()
	h := newHarness(t, func(o *ManagerOptions) { o.Now = func() time.Time { return now }; o.SelfApply = false })
	if h.m.nextCheck != h.m.nextCheck.Round(0) {
		t.Fatal("startup deadline retains monotonic clock")
	}
	now = now.Add(72 * time.Hour).Round(0)
	h.checker.checkErr = errors.New("offline")
	h.m.step(t.Context())
	if h.checker.checks != 1 || h.m.lastErr == "" {
		t.Fatal("overdue check not attempted")
	}
	if h.m.nextCheck != h.m.nextCheck.Round(0) {
		t.Fatal("retry deadline retains monotonic clock")
	}
	for range 5 {
		h.m.step(t.Context())
	}
	if h.checker.checks != 1 {
		t.Fatal("missed intervals replayed or retry ignored")
	}
	now = now.Add(h.m.opts.RetryInterval)
	h.checker.checkErr = nil
	h.m.step(t.Context())
	if h.checker.checks != 2 || h.m.lastErr != "" {
		t.Fatal("failed check did not recover")
	}
	if len(h.restarts) != 0 {
		t.Fatal("notify-only check restarted the server")
	}
	now = now.Add(-7 * 24 * time.Hour)
	h.m.step(t.Context())
	if h.m.nextCheck.Sub(now) > h.m.opts.PollInterval {
		t.Fatal("backwards clock correction postponed checks beyond a period")
	}
}

// OnAvailable hears each found version until it accepts it, without the
// manager's lock held (it may read Status).
func TestOnAvailableUntilAcceptedOutsideLock(t *testing.T) {
	var heard []string
	fail := true
	var h *harness
	h = newHarness(t, func(o *ManagerOptions) {
		o.OnAvailable = func(v string) error {
			_ = h.m.Status() // deadlocks if called under the lock
			heard = append(heard, v)
			if fail {
				return errors.New("notification queue unavailable")
			}
			return nil
		}
		o.SelfApply, o.NotifyReason = false, "notify only"
	})
	h.run(DefaultFirstCheckDelay + DefaultPollInterval + time.Minute)
	if len(heard) != 2 || heard[0] != "2.0.0" || heard[1] != "2.0.0" {
		t.Fatalf("heard %v, want a retry after the failed announcement", heard)
	}
	fail = false
	h.run(DefaultPollInterval)
	h.run(2 * DefaultPollInterval)
	if len(heard) != 3 {
		t.Fatalf("heard %v, want no calls after acceptance", heard)
	}
	h.checker.mu.Lock()
	h.checker.upd = binUpdate("2.1.0", []byte("2.1.0"))
	h.checker.mu.Unlock()
	h.run(2 * DefaultPollInterval)
	if len(heard) != 4 || heard[3] != "2.1.0" {
		t.Fatalf("heard %v", heard)
	}
}
