package app

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime/pprof"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/perfharness"
	"github.com/braedonsaunders/ether/internal/store"
)

// PerfGateEnv runs TestPerformanceGate. CI sets it in its own job; see
// docs/performance.md.
const PerfGateEnv = "ETHER_PERF_GATE"

// gateClients are replayed against both scales: one client per protocol.
var gateClients = []string{"plex-appletv", "infuse-jellyfin", "emby-sdk", "silo-appletv", "stremio", "console"}

// TestPerformanceGate fails when a request path scales with the library.
// It builds the same synthetic household over a library of SmallScale and
// one ten times larger (LargeScale, 500k+ catalogue rows), replays every
// protocol's request sequence against a real App on each, and compares each
// endpoint's statements, rows read and database time between the two. A
// path that walks the catalogue does ten times the work on the larger
// library; an indexed path does about the same.
func TestPerformanceGate(t *testing.T) {
	mode := os.Getenv(PerfGateEnv)
	if *perfGateFlag != "" {
		mode = *perfGateFlag
	}
	if mode == "" {
		t.Skip("set " + PerfGateEnv + "=1 (or -args -perf-gate=1) to run the library-scale performance gate (docs/performance.md)")
	}
	smallScale, largeScale := perfharness.SmallScale, perfharness.LargeScale
	if mode == "quick" {
		// A faster pair, also 10× apart, for iterating locally.
		smallScale, largeScale = perfharness.SmallScale.Divide(10), perfharness.SmallScale
	}
	small := runPerfScale(t, "small", smallScale)
	large := runPerfScale(t, "large", largeScale)
	var report bytes.Buffer
	failures := comparePerfScales(&report, small, large)
	t.Log("\n" + report.String())
	if path := os.Getenv("ETHER_PERF_GATE_REPORT"); path != "" {
		_ = os.WriteFile(path, report.Bytes(), 0o644)
	}
	debt, err := readPerfDebt()
	if err != nil {
		t.Fatal(err)
	}
	if mode == "quick" {
		// The quick pair is for iterating; only the full pair is ratcheted.
		for _, f := range failures {
			t.Log("would fail: " + f.key + ": " + f.why)
		}
		return
	}
	failed := map[string]bool{}
	for _, f := range failures {
		failed[f.key] = true
		if debt[f.key] {
			t.Logf("known debt: %s: %s", f.key, f.why)
			continue
		}
		t.Errorf("%s: %s", f.key, f.why)
	}
	ran := map[string]bool{}
	for _, row := range large {
		ran[row.Client+" | "+row.Step] = true
	}
	for key := range debt {
		if ran[key] && !failed[key] {
			// Logged, not failed: database time near the slack can pass on
			// one run and not the next.
			t.Logf("%s stayed flat this run: remove it from %s once it does so reliably", key, perfDebtFile)
		}
	}
}

// restrictedListings is the Plex Apple TV client's whole-library listing
// steps under its own name, for the restricted viewer's pass.
func restrictedListings(cs []perfharness.Client) []perfharness.Client {
	for _, c := range cs {
		if c.Name != "plex-appletv" {
			continue
		}
		c.Name = "plex-appletv restricted"
		var steps []perfharness.Step
		for _, st := range c.Steps {
			switch st.Name {
			case "/library/sections", "shows allLeaves p1", "shows allLeaves deep", "shows all type=4 p1", "shows all type=3 p1":
				steps = append(steps, st)
			}
		}
		c.Steps = steps
		return []perfharness.Client{c}
	}
	return nil
}

var perfGateFlag = flag.String("perf-gate", "", "run TestPerformanceGate (1, or quick for a smaller pair)")

// perfDebtFile lists the endpoints known to scale with the library, one
// "client | step" per line. The gate fails on any other endpoint that
// scales and reports listed endpoints that no longer do, so the list only
// shrinks.
const perfDebtFile = "testdata/perf_gate_debt.txt"

func readPerfDebt() (map[string]bool, error) {
	b, err := os.ReadFile(perfDebtFile)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if i := strings.Index(line, " #"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		out[line] = true
	}
	return out, nil
}

type gateFailure struct{ key, why string }

// Gate thresholds between scales 10× apart. Each allows for noise and for
// logarithmic growth, never for linear growth.
const (
	gateRowsFactor    = 2.0
	gateRowsSlack     = 200
	gateQueriesFactor = 1.5
	gateQueriesSlack  = 20
	gateDBFactor      = 5.0
	gateDBSlack       = 100 * time.Millisecond
	gateWallCeiling   = 2 * time.Second
)

func comparePerfScales(w *bytes.Buffer, small, large []*perfharness.Row) []gateFailure {
	bySmall := map[string]*perfharness.Row{}
	for _, r := range small {
		bySmall[r.Client+" "+r.Step] = r
	}
	var failures []gateFailure
	fmt.Fprintln(w, "| client | step | queries S max→L min | rows S max→L min | db S→L | warm p50 S→L | verdict |")
	fmt.Fprintln(w, "|---|---|---:|---:|---:|---:|---|")
	for _, l := range large {
		s := bySmall[l.Client+" "+l.Step]
		if s == nil || l.Class == "playback" {
			continue
		}
		verdict := "ok"
		var why []string
		switch {
		case l.Status == "SKIP" || s.Status == "SKIP":
			verdict = "skipped " + l.Skipped + s.Skipped
		case l.Status == "ERROR" && s.Status != "ERROR":
			why = append(why, "fails only on the large library: "+strings.Join(l.Errors, "; "))
		case l.Status == "ERROR":
			verdict = "error on both: " + strings.Join(l.Errors, "; ")
		default:
			// A pass may or may not be answered by a cache (a generation or
			// minute bucket can turn over between passes), so the large
			// library's least work is compared with the small one's most: a
			// path that walks the catalogue does more work on every pass.
			switch {
			case l.RowsBound > 0:
				if l.RowsMax > int64(l.RowsBound) {
					why = append(why, fmt.Sprintf("rows read %d over the step's bound %d", l.RowsMax, l.RowsBound))
				}
			case float64(l.RowsMin) > gateRowsFactor*float64(s.RowsMax)+gateRowsSlack:
				why = append(why, fmt.Sprintf("rows read grow with the library (%d → %d)", s.RowsMax, l.RowsMin))
			}
			if float64(l.QueriesMin) > gateQueriesFactor*float64(s.QueriesMax)+gateQueriesSlack {
				why = append(why, fmt.Sprintf("statements grow with the library (%d → %d)", s.QueriesMax, l.QueriesMin))
			}
			if l.DBTime > time.Duration(gateDBFactor*float64(s.DBTime))+gateDBSlack {
				why = append(why, fmt.Sprintf("database time grows with the library (%s → %s)", s.DBTime, l.DBTime))
			}
			if l.WarmP50 > gateWallCeiling {
				why = append(why, fmt.Sprintf("warm p50 %s over %s", l.WarmP50, gateWallCeiling))
			}
		}
		if len(why) > 0 {
			verdict = "FAIL: " + strings.Join(why, "; ")
			failures = append(failures, gateFailure{key: l.Client + " | " + l.Step, why: strings.Join(why, "; ")})
		}
		fmt.Fprintf(w, "| %s | %s | %d → %d | %d → %d | %s → %s | %s → %s | %s |\n", l.Client, l.Step, s.QueriesMax, l.QueriesMin, s.RowsMax, l.RowsMin,
			s.DBTime.Round(time.Microsecond*100), l.DBTime.Round(time.Microsecond*100), s.WarmP50.Round(time.Microsecond*100), l.WarmP50.Round(time.Microsecond*100), verdict)
	}
	return failures
}

func freePorts(t *testing.T, n int) []int {
	t.Helper()
	var out []int
	var ls []net.Listener
	for range n {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		ls = append(ls, l)
		out = append(out, l.Addr().(*net.TCPAddr).Port)
	}
	for _, l := range ls {
		l.Close()
	}
	return out
}

func runPerfScale(t *testing.T, name string, scale perfharness.Scale) []*perfharness.Row {
	ctx := context.Background()
	dir := t.TempDir()
	start := time.Now()
	cfg, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ports := freePorts(t, 3)
	if err = cfg.Update(func(c *config.Config) error {
		c.SetupComplete = true
		c.Frontends = map[string]config.Frontend{"plex": {Enabled: true}, "jellyfin": {Enabled: true}, "emby": {Enabled: true}, "silo": {Enabled: true}, "stremio": {Enabled: true}}
		c.FrontendPorts = map[string]int{"jellyfin": ports[0], "emby": ports[1], "silo": ports[2]}
		c.Transcode.FFmpegPath = "/not-installed/ffmpeg"
		c.Server.BindAddress = "127.0.0.1"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = perfharness.Generate(ctx, db, scale, time.Now()); err != nil {
		t.Fatal(err)
	}
	viewers, err := perfharness.MintCredentials(ctx, db, perfharness.RestrictedViewer+1)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	t.Logf("%s: generated %d catalogue rows in %s", name, scale.Rows(), time.Since(start).Round(time.Millisecond))

	trace := filepath.Join(dir, "trace.jsonl")
	t.Setenv(PerfTraceEnv, trace)
	var appLog bytes.Buffer
	a, err := New(ctx, Options{DataDir: dir, Logger: slog.New(slog.NewTextHandler(&appLog, &slog.HandlerOptions{Level: slog.LevelWarn}))})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(a.Handler())
	a.frontends.start(ctx, server.Listener.Addr())
	// Run serves with the background WAL checkpointer; without it every
	// read searches an ever-growing WAL index, which is not the server.
	cpCtx, stopCheckpoints := context.WithCancel(ctx)
	defer stopCheckpoints()
	go a.checkpointWAL(cpCtx)
	port := server.Listener.Addr().(*net.TCPAddr).Port
	clients := gateClients
	if only := os.Getenv("ETHER_PERF_GATE_CLIENTS"); only != "" {
		clients = strings.Split(only, ",")
	}
	cs := perfharness.Only(perfharness.Clients(), clients)
	if steps := os.Getenv("ETHER_PERF_GATE_STEPS"); steps != "" {
		cs = perfharness.OnlySteps(cs, strings.Split(steps, ","))
	}
	var viewing []perfharness.Client
	for _, c := range cs {
		if c.Surface != "console" {
			viewing = append(viewing, c)
		}
	}
	// The gate compares work, not wall time, so a cold pass and two warm
	// passes for the owner, and one for a second viewer, are enough.
	r := perfharness.NewRunner(port, true, viewers)
	// A request this slow has already failed the ceiling; do not wait longer.
	r.SetTimeout(15 * time.Second)
	if path := os.Getenv("ETHER_PERF_GATE_CPUPROFILE"); path != "" && name == "large" {
		// The large replay alone, without the fixture's generation.
		if f, err := os.Create(path); err == nil {
			if pprof.StartCPUProfile(f) == nil {
				defer func() { pprof.StopCPUProfile(); f.Close() }()
			}
		}
	}
	r.Sequential(ctx, "cold", viewers[:1], cs)
	for range 2 {
		r.Sequential(ctx, "warm", viewers[:1], cs)
	}
	r.Sequential(ctx, "warm", viewers[1:2], viewing)
	// A restricted viewer's whole-library episode and season listings page
	// in SQL with their rating cap and plex.tv share filter, not by walking
	// the library.
	restricted := restrictedListings(cs)
	if len(viewers) > perfharness.RestrictedViewer && len(restricted) > 0 {
		// A cold pass and a warm one, as the owner gets: the gate compares
		// the large library's least work with the small one's most.
		r.Sequential(ctx, "cold", viewers[perfharness.RestrictedViewer:perfharness.RestrictedViewer+1], restricted)
		r.Sequential(ctx, "warm", viewers[perfharness.RestrictedViewer:perfharness.RestrictedViewer+1], restricted)
		cs = append(cs, restricted...)
		for _, res := range r.Results() {
			if res.Client != restricted[0].Name {
				continue
			}
			t.Logf("%s: restricted %s: %d, %d bytes", name, res.Step, res.Status, res.Bytes)
			if res.Status != http.StatusOK {
				t.Errorf("%s: restricted viewer %s answered %d", name, res.Step, res.Status)
			}
			// Page 1 of each listing must hold titles: an empty container
			// would pass the scale comparison without measuring anything.
			if res.Step != "/library/sections" && !strings.HasSuffix(res.Step, "deep") && res.Bytes < 4096 {
				t.Errorf("%s: restricted viewer %s listed nothing (%d bytes)", name, res.Step, res.Bytes)
			}
		}
	}
	if fi, err := os.Stat(filepath.Join(dir, "ether.db-wal")); err == nil {
		// Every page read searches the WAL index, so a WAL that cannot
		// restart slows every query.
		t.Logf("%s: WAL at the end of the replay: %d MB", name, fi.Size()>>20)
	}
	stopCheckpoints()
	server.Close()
	a.Close()
	if keep := os.Getenv("ETHER_PERF_GATE_KEEP"); keep != "" {
		// Keep the trace and database for etherperf audit.
		_ = os.MkdirAll(keep, 0o700)
		_ = os.Rename(trace, filepath.Join(keep, name+"-trace.jsonl"))
		trace = filepath.Join(keep, name+"-trace.jsonl")
		for _, suffix := range []string{"", "-wal", "-shm"} {
			_ = os.Rename(filepath.Join(dir, "ether.db"+suffix), filepath.Join(keep, name+".db"+suffix))
		}
	}
	byTag, _, err := perfharness.ReadTrace(trace)
	if err != nil {
		t.Fatal(err)
	}
	rows := perfharness.Summarize(r.Results(), byTag, cs)
	if t.Failed() {
		t.Log(appLog.String())
	}
	t.Logf("%s: replayed in %s", name, time.Since(start).Round(time.Millisecond))
	return rows
}
