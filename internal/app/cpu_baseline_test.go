//go:build unix

package app

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/handles/capability"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/perfharness"
	"github.com/braedonsaunders/ether/internal/store"
)

type rusageSample struct {
	wall   time.Time
	utime  time.Duration
	stime  time.Duration
	nvcsw  int64
	nivcsw int64
	maxrss int64
}

func takeRusage() rusageSample {
	var ru syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	ut := time.Duration(ru.Utime.Sec)*time.Second + time.Duration(ru.Utime.Usec)*time.Microsecond
	st := time.Duration(ru.Stime.Sec)*time.Second + time.Duration(ru.Stime.Usec)*time.Microsecond
	rss := int64(ru.Maxrss)
	if runtime.GOOS != "darwin" {
		rss *= 1024 // Linux reports KiB; macOS reports bytes.
	}
	return rusageSample{
		wall:   time.Now(),
		utime:  ut,
		stime:  st,
		nvcsw:  int64(ru.Nvcsw),
		nivcsw: int64(ru.Nivcsw),
		maxrss: rss,
	}
}

type rusageDelta struct {
	wall   time.Duration
	utime  time.Duration
	stime  time.Duration
	total  time.Duration
	cpuPct float64 // % of 1 core
	nvcsw  int64
	nivcsw int64
}

func diffRusage(a, b rusageSample) rusageDelta {
	wall := b.wall.Sub(a.wall)
	ut := b.utime - a.utime
	st := b.stime - a.stime
	tot := ut + st
	var pct float64
	if wall > 0 {
		pct = float64(tot) / float64(wall) * 100.0
	}
	return rusageDelta{
		wall:   wall,
		utime:  ut,
		stime:  st,
		total:  tot,
		cpuPct: pct,
		nvcsw:  b.nvcsw - a.nvcsw,
		nivcsw: b.nivcsw - a.nivcsw,
	}
}

func dumpPprofTop(t *testing.T, label, profPath string) {
	t.Helper()
	for _, flag := range []string{"-top", "-top -cum"} {
		args := append([]string{"tool", "pprof"}, strings.Fields(flag)...)
		args = append(args, "-nodecount=18", profPath)
		out, err := exec.Command("go", args...).CombinedOutput()
		if err == nil {
			t.Logf("=== PPROF %s (%s) ===\n%s", label, flag, string(out))
		}
	}
}

// TestCPUBaselineAndLoadLevels measures Ether's exact CPU %, context switches,
// memory footprint, and pprof profile across:
//   - Level A: Steady-State Idle (0 streams, settled background workers + per-worker micro-attribution)
//   - Level B: Background Maintenance (personalize model rebuild, housekeeping shard sweep, DB pruners, stats & health sample, availability cycle)
//   - Level C: Active Browse / Home Load on the 540k-item LargeScale catalog (warm + concurrent Home/Hub browse + pprof breakdown)
func TestCPUBaselineAndLoadLevels(t *testing.T) {
	mode := os.Getenv("ETHER_CPU_BASELINE")
	if mode == "" {
		t.Skip("set ETHER_CPU_BASELINE=1 to run the CPU baseline & load-level benchmark")
	}
	scale := perfharness.LargeScale
	if mode == "small" {
		scale = perfharness.SmallScale
	}

	ctx := context.Background()
	dir := t.TempDir()
	cfg, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ports := freePorts(t, 4)
	if err := cfg.Update(func(c *config.Config) error {
		c.SetupComplete = true
		c.Server.BindAddress = "127.0.0.1"
		c.Server.Port = ports[0]
		c.Frontends = map[string]config.Frontend{
			"plex":     {Enabled: true},
			"jellyfin": {Enabled: true},
			"emby":     {Enabled: true},
			"silo":     {Enabled: true},
			"stremio":  {Enabled: true},
		}
		c.FrontendPorts = map[string]int{
			"jellyfin": ports[1],
			"emby":     ports[2],
			"silo":     ports[3],
		}
		c.Transcode.FFmpegPath = "/not-installed/ffmpeg"
		c.Plex.RemoteAccess = false
		c.Plex.UseUPnP = false
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	genStart := time.Now()
	db, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := perfharness.Generate(ctx, db, scale, time.Now()); err != nil {
		t.Fatal(err)
	}
	viewers, err := perfharness.MintCredentials(ctx, db, perfharness.RestrictedViewer+1)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	t.Logf("Fixture generated: %d catalogue rows in %s", scale.Rows(), time.Since(genStart).Round(time.Millisecond))

	trace := filepath.Join(dir, "trace.jsonl")
	t.Setenv(PerfTraceEnv, trace)
	var appLog bytes.Buffer

	// Capture CPU profile of startup + background model/collection warm-up.
	bgProfPath := filepath.Join(dir, "background_warmup.pprof")
	bgProfFile, err := os.Create(bgProfPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = pprof.StartCPUProfile(bgProfFile)
	startupR0 := takeRusage()

	a, err := New(ctx, Options{
		DataDir: dir,
		Logger:  slog.New(slog.NewTextHandler(&appLog, &slog.HandlerOptions{Level: slog.LevelWarn})),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	runCtx, stopRun := context.WithCancel(ctx)
	runDone := make(chan error, 1)
	go func() {
		runDone <- a.Run(runCtx)
	}()

	// Wait for initial background warm-up (personalize model builds, collections, prewarmBrowse) to settle.
	var settleHistory []string
	for sec := 1; sec <= 25; sec++ {
		s0 := takeRusage()
		time.Sleep(1 * time.Second)
		d := diffRusage(s0, takeRusage())
		settleHistory = append(settleHistory, time.Duration(sec*int(time.Second)).String()+":"+
			strings.TrimSpace(slog.Float64Value(d.cpuPct).String()[:min(5, len(slog.Float64Value(d.cpuPct).String()))])+"%")
		if sec >= 4 && d.cpuPct < 5.0 {
			break
		}
	}
	startupDelta := diffRusage(startupR0, takeRusage())
	pprof.StopCPUProfile()
	bgProfFile.Close()
	t.Logf("Startup + Background Warm-up Curve (per-second CPU%%): %s", strings.Join(settleHistory, " -> "))
	t.Logf("Startup + Background Warm-up Total: Wall=%s | CPU=%s (User=%s Sys=%s, %.1f%% of 1 core)",
		startupDelta.wall.Round(time.Millisecond), startupDelta.total.Round(time.Millisecond),
		startupDelta.utime.Round(time.Millisecond), startupDelta.stime.Round(time.Millisecond), startupDelta.cpuPct)
	dumpPprofTop(t, "BACKGROUND_WARMUP_540K", bgProfPath)

	// =========================================================================
	// LEVEL A: STEADY-STATE IDLE (0 streams, settled background workers)
	// =========================================================================
	runtime.GC()
	var idleMemBefore runtime.MemStats
	runtime.ReadMemStats(&idleMemBefore)
	idleStart := takeRusage()
	time.Sleep(8 * time.Second)
	idleEnd := takeRusage()
	var idleMemAfter runtime.MemStats
	runtime.ReadMemStats(&idleMemAfter)
	idleGoroutines := runtime.NumGoroutine()
	idleDelta := diffRusage(idleStart, idleEnd)

	stopRun()
	<-runDone
	a.Close()

	t.Logf("=== LEVEL A: STEADY-STATE IDLE (8.0s settled window, %d catalogue rows) ===", scale.Rows())
	t.Logf("Idle CPU: %.3f%% of 1 core (%.4f%% of %d-core host) | User=%s Sys=%s Total=%s",
		idleDelta.cpuPct, idleDelta.cpuPct/float64(runtime.NumCPU()), runtime.NumCPU(),
		idleDelta.utime.Round(100*time.Microsecond), idleDelta.stime.Round(100*time.Microsecond), idleDelta.total.Round(100*time.Microsecond))
	t.Logf("Idle Wakeups / Context Switches: %.1f/sec (vol=%d nonvol=%d over %s) | Goroutines=%d",
		float64(idleDelta.nvcsw+idleDelta.nivcsw)/idleDelta.wall.Seconds(),
		idleDelta.nvcsw, idleDelta.nivcsw, idleDelta.wall.Round(time.Millisecond), idleGoroutines)
	t.Logf("Idle Memory: HeapAlloc=%.1f MB | HeapInuse=%.1f MB | Sys=%.1f MB | MaxRSS=%.1f MB | AllocRate=%.1f KB/s",
		float64(idleMemAfter.HeapAlloc)/(1<<20),
		float64(idleMemAfter.HeapInuse)/(1<<20),
		float64(idleMemAfter.Sys)/(1<<20),
		float64(idleEnd.maxrss)/(1<<20),
		float64(idleMemAfter.TotalAlloc-idleMemBefore.TotalAlloc)/1024.0/idleDelta.wall.Seconds())

	// Re-open App for isolated Level A micro-attribution, Level B maintenance, and Level C browse load.
	a2, err := New(ctx, Options{
		DataDir: dir,
		Logger:  slog.New(slog.NewTextHandler(&appLog, &slog.HandlerOptions{Level: slog.LevelWarn})),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a2.Close()
	// Wait for background prewarm on a2 to settle before micro-attribution.
	for range 20 {
		s0 := takeRusage()
		time.Sleep(500 * time.Millisecond)
		if diffRusage(s0, takeRusage()).cpuPct < 5.0 {
			break
		}
	}

	// Micro-attribution of background ticker callbacks
	type workerCost struct {
		name     string
		interval string
		iters    int
		delta    rusageDelta
		estIdle  float64 // estimated % of 1 core at its production ticker interval
	}
	measureWorker := func(name, interval string, period time.Duration, iters int, fn func()) workerCost {
		s0 := takeRusage()
		for range iters {
			fn()
		}
		s1 := takeRusage()
		d := diffRusage(s0, s1)
		cpuPerTick := float64(d.total) / float64(iters)
		estPct := (cpuPerTick / float64(period)) * 100.0
		return workerCost{name: name, interval: interval, iters: iters, delta: d, estIdle: estPct}
	}

	var shard int64
	workers := []workerCost{
		measureWorker("sysload.Sampler.Sample (gopsutil CPU/Mem/Net/Proc)", "2s", 2*time.Second, 20, func() {
			a2.load.Sample(ctx)
		}),
		measureWorker("notify.Service.Drain (SQLite notify_channels poll)", "1s", 1*time.Second, 50, func() {
			_ = a2.notifications.Drain(ctx)
		}),
		measureWorker("plexwatch.ReconcileOnce (SQLite poll)", "2s", 2*time.Second, 50, func() {
			if a2.plexWatch != nil {
				_, _ = a2.plexWatch.ReconcileOnce(ctx)
			}
		}),
		measureWorker("housekeeping shard sweep (ImageGrant+Receipt DELETE)", "5s", 5*time.Second, 50, func() {
			now := time.Now()
			_, _ = identity.PruneImageGrantShard(ctx, a2.db, now, shard%64)
			_, _ = experience.PruneReceiptShard(ctx, a2.db, now, shard%64)
			shard++
		}),
	}
	t.Log("=== LEVEL A MICRO-ATTRIBUTION: PER-TICK COST OF IDLE BACKGROUND WORKERS ===")
	for _, w := range workers {
		t.Logf("  %-54s | period=%-3s | CPU/tick=%8.3fms | Wall/tick=%8.3fms | EstIdle=%.4f%% of 1 core",
			w.name, w.interval,
			float64(w.delta.total.Microseconds())/1000.0/float64(w.iters),
			float64(w.delta.wall.Microseconds())/1000.0/float64(w.iters),
			w.estIdle)
	}

	// =========================================================================
	// LEVEL B: BACKGROUND MAINTENANCE
	// =========================================================================
	t.Log("=== LEVEL B: BACKGROUND MAINTENANCE TASKS ===")
	maintTasks := []struct {
		name string
		fn   func()
	}{
		{
			name: "Personalize viewer model rebuild (540k catalog, 20k history)",
			fn: func() {
				_ = a2.personalize.Rebuild(ctx, &core.User{ID: viewers[0].ID, Username: viewers[0].Username, IsOwner: true})
			},
		},
		{
			name: "Full 64-shard housekeeping + capability/pin/seen flush",
			fn: func() {
				now := time.Now()
				for s := int64(0); s < 64; s++ {
					_, _ = identity.PruneImageGrantShard(ctx, a2.db, now, s)
					_, _ = experience.PruneReceiptShard(ctx, a2.db, now, s)
				}
				_, _ = capability.PruneExpired(ctx, a2.db, now)
				_ = a2.playback.ReconcilePins(ctx)
				_ = a2.users.FlushSeen(ctx)
			},
		},
		{
			name: "Database maintenance pruners (runPruners)",
			fn: func() {
				a2.runPruners(ctx)
			},
		},
		{
			name: "Stats sample + Notification health sample (10 passes)",
			fn: func() {
				for range 10 {
					a2.stats.Sample(ctx)
					_ = a2.notifications.SampleHealth(ctx)
				}
			},
		},
		{
			name: "Availability check cycle",
			fn: func() {
				_ = a2.availability.Cycle(ctx)
			},
		},
	}
	for _, mt := range maintTasks {
		s0 := takeRusage()
		mt.fn()
		s1 := takeRusage()
		d := diffRusage(s0, s1)
		t.Logf("  %-64s | Wall=%8.2fms | CPU=%8.2fms (User=%s Sys=%s) | CPU%%=%.1f%% | CtxtSw=%d",
			mt.name,
			float64(d.wall.Microseconds())/1000.0,
			float64(d.total.Microseconds())/1000.0,
			d.utime.Round(100*time.Microsecond), d.stime.Round(100*time.Microsecond),
			d.cpuPct, d.nvcsw+d.nivcsw)
	}

	// =========================================================================
	// LEVEL C: ACTIVE BROWSE / HOME LOAD (540k-item catalog) + PPROF
	// =========================================================================
	server := httptest.NewServer(a2.Handler())
	defer server.Close()
	a2.frontends.start(ctx, server.Listener.Addr())
	defer a2.frontends.stop()
	cpCtx, stopCheckpoints := context.WithCancel(ctx)
	defer stopCheckpoints()
	go a2.checkpointWAL(cpCtx)

	port := server.Listener.Addr().(*net.TCPAddr).Port
	cs := perfharness.Only(perfharness.Clients(), gateClients)
	var viewing []perfharness.Client
	for _, c := range cs {
		if c.Surface != "console" {
			viewing = append(viewing, c)
		}
	}

	r := perfharness.NewRunner(port, true, viewers)
	r.SetTimeout(15 * time.Second)

	// 1. Cold pass
	coldR0 := takeRusage()
	r.Sequential(ctx, "cold", viewers[:1], cs)
	coldDelta := diffRusage(coldR0, takeRusage())

	// 2. Warm pass + Concurrent Home/Hub pass with CPU pprof capture
	profPath := filepath.Join(dir, "browse_warm_and_concurrent.pprof")
	profFile, err := os.Create(profPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := pprof.StartCPUProfile(profFile); err != nil {
		t.Fatal(err)
	}

	t.Logf("=== LEVEL C: ACTIVE BROWSE / HOME LOAD (%d catalogue rows) ===", scale.Rows())
	warmR0 := takeRusage()
	for _, c := range cs {
		cR0 := takeRusage()
		beforeCount := len(r.Results())
		for range 2 {
			r.Sequential(ctx, "warm", viewers[:1], []perfharness.Client{c})
		}
		cDelta := diffRusage(cR0, takeRusage())
		reqs := len(r.Results()) - beforeCount
		t.Logf("  Warm Client %-16s (%3d reqs): Wall=%7.1fms (%6.2fms/req) | CPU=%7.1fms (%6.2fms/req, %.1f%% core) | CtxtSw=%d",
			c.Name, reqs,
			float64(cDelta.wall.Microseconds())/1000.0,
			float64(cDelta.wall.Microseconds())/1000.0/float64(max(reqs, 1)),
			float64(cDelta.total.Microseconds())/1000.0,
			float64(cDelta.total.Microseconds())/1000.0/float64(max(reqs, 1)),
			cDelta.cpuPct,
			cDelta.nvcsw+cDelta.nivcsw)
	}
	r.Sequential(ctx, "warm", viewers[1:2], viewing)
	warmDelta := diffRusage(warmR0, takeRusage())

	// 3. Concurrent Home/Hub/Browse pass (2 concurrent viewers x 1 round on Home/Hub/Resume/NextUp/Sections/Movies p1)
	homeHubClients := perfharness.OnlySteps(viewing, []string{
		"/hubs", "/library/sections", "Views", "Resume", "NextUp", "Latest", "Movies p1", "home", "catalog movie",
	})
	concBefore := len(r.Results())
	concR0 := takeRusage()
	r.Concurrent(ctx, "concurrent", 2, 1, homeHubClients)
	concDelta := diffRusage(concR0, takeRusage())

	pprof.StopCPUProfile()
	profFile.Close()
	concReqs := len(r.Results()) - concBefore

	var browseMem runtime.MemStats
	runtime.ReadMemStats(&browseMem)

	t.Logf("Cold Pass Summary:       Wall=%7.1fms | CPU=%7.1fms (%.1f%% of 1 core)",
		float64(coldDelta.wall.Microseconds())/1000.0, float64(coldDelta.total.Microseconds())/1000.0, coldDelta.cpuPct)
	t.Logf("Warm Pass Summary:       Wall=%7.1fms | CPU=%7.1fms (%.1f%% of 1 core)",
		float64(warmDelta.wall.Microseconds())/1000.0, float64(warmDelta.total.Microseconds())/1000.0, warmDelta.cpuPct)
	t.Logf("Concurrent Home/Hub Pass (2v×1r, %d reqs): Wall=%7.1fms (%.1f req/s) | CPU=%7.1fms (%6.2fms CPU/req, %.1f%% of 1 core, %.1f%% of %d-core host)",
		concReqs,
		float64(concDelta.wall.Microseconds())/1000.0,
		float64(concReqs)/concDelta.wall.Seconds(),
		float64(concDelta.total.Microseconds())/1000.0,
		float64(concDelta.total.Microseconds())/1000.0/float64(max(concReqs, 1)),
		concDelta.cpuPct,
		concDelta.cpuPct/float64(runtime.NumCPU()),
		runtime.NumCPU())
	t.Logf("Browse Memory Footprint: HeapAlloc=%.1f MB | HeapInuse=%.1f MB | Sys=%.1f MB | MaxRSS=%.1f MB",
		float64(browseMem.HeapAlloc)/(1<<20),
		float64(browseMem.HeapInuse)/(1<<20),
		float64(browseMem.Sys)/(1<<20),
		float64(takeRusage().maxrss)/(1<<20))

	// Summarize per-step latencies & DB work
	byTag, _, err := perfharness.ReadTrace(trace)
	if err != nil {
		t.Fatal(err)
	}
	rows := perfharness.Summarize(r.Results(), byTag, cs)
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].WarmP50 > rows[j].WarmP50
	})
	t.Log("=== TOP 20 STEPS BY WARM P50 LATENCY & DB WORK (540k CATALOG) ===")
	for i := 0; i < min(20, len(rows)); i++ {
		row := rows[i]
		t.Logf("  %-16s | %-30s | warm_p50=%8s | conc_p95=%8s | queries=%2d | rows=%4d | db=%8s | status=%s",
			row.Client, row.Step,
			row.WarmP50.Round(100*time.Microsecond),
			row.ConcP95.Round(100*time.Microsecond),
			row.QueriesMax, row.RowsMax,
			row.DBTime.Round(100*time.Microsecond),
			row.Status)
		if row.Status == "ERROR" && row.Class != "playback" {
			t.Errorf("step error %s | %s: %v", row.Client, row.Step, row.Errors)
		}
	}

	dumpPprofTop(t, "BROWSE_540K", profPath)
}
