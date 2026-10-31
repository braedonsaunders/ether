// Command etherperf measures Ether the way clients use it: it copies a real
// data directory, starts a real Ether process on it with outbound network
// blocked, replays each client's request sequence (Plex, Jellyfin and Emby
// apps including Infuse, Silo, Stremio and the owner console) cold, warm,
// under concurrent viewers and beside a busy catalogue writer, and reports
// every endpoint against its latency budget together with the database work
// the server did for it. It then runs EXPLAIN QUERY PLAN on every statement
// those requests executed and ranks them.
//
//	go run ./cmd/etherperf real -source ~/path/to/data
//
// See docs/performance.md.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/perfharness"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	var err error
	code := 0
	switch os.Args[1] {
	case "real":
		code, err = runReal(ctx, os.Args[2:])
	case "audit":
		err = runAudit(ctx, os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "etherperf:", err)
		os.Exit(2)
	}
	os.Exit(code)
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: etherperf real -source DATA_DIR [flags] | etherperf audit -trace FILE -db FILE")
	os.Exit(2)
}

func runReal(ctx context.Context, args []string) (int, error) {
	f := flag.NewFlagSet("real", flag.ExitOnError)
	source := f.String("source", "", "data directory to copy (config.json, .credentials-key, ether.db)")
	out := f.String("out", "", "output directory (default a new directory under the system temp dir)")
	port := f.Int("port", 32990, "base port: Plex/console on port, Jellyfin +1, Emby +2, Silo +3")
	bin := f.String("bin", "", "prebuilt ether binary (default: build ./cmd/ether)")
	viewerCount := f.Int("viewers", 2, "active viewers to sign in as (owner first)")
	rounds := f.Int("rounds", 5, "warm rounds")
	conc := f.Int("concurrency", 8, "concurrent viewers in the concurrent phase")
	concRounds := f.Int("concurrent-rounds", 2, "rounds per concurrent viewer")
	background := f.Bool("background", true, "run a phase beside a catalogue-shaped background writer")
	only := f.String("clients", "", "comma-separated client names (default all)")
	stepNames := f.String("steps", "", "comma-separated substrings of step names to replay (default all)")
	strict := f.Bool("strict", false, "exit 1 when any gated endpoint misses its budget or errors")
	_ = f.Parse(args)
	if *source == "" {
		return 2, fmt.Errorf("-source is required")
	}
	dir := *out
	if dir == "" {
		var err error
		if dir, err = os.MkdirTemp("", "etherperf-"); err != nil {
			return 2, err
		}
	}
	dir = absPath(dir)
	data := filepath.Join(dir, "data")
	if fileExists(data) {
		return 2, fmt.Errorf("%s already exists; choose a fresh -out", data)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return 2, err
	}
	log := func(format string, a ...any) { fmt.Fprintf(os.Stderr, "etherperf: "+format+"\n", a...) }
	server := *bin
	if server == "" {
		server = filepath.Join(dir, "ether")
		log("building ./cmd/ether")
		if err := buildServer(ctx, server); err != nil {
			return 2, fmt.Errorf("build: %w", err)
		}
	}
	log("copying %s", *source)
	if err := copyDataDir(ctx, *source, data); err != nil {
		return 2, err
	}
	if err := sanitizeConfig(filepath.Join(data, "config.json"), *port); err != nil {
		return 2, fmt.Errorf("sanitize settings: %w", err)
	}
	viewers, err := mintCopy(ctx, data, *viewerCount)
	if err != nil {
		return 2, fmt.Errorf("credentials: %w", err)
	}
	var names []string
	if *only != "" {
		names = strings.Split(*only, ",")
	}
	cs := perfharness.Only(perfharness.Clients(), names)
	if *stepNames != "" {
		cs = perfharness.OnlySteps(cs, strings.Split(*stepNames, ","))
	}
	trace := filepath.Join(dir, "trace.jsonl")
	log("starting server on 127.0.0.1:%d (outbound network blocked)", *port)
	srv, err := startServer(ctx, server, data, *port, trace, filepath.Join(dir, "server.log"))
	if err != nil {
		return 2, err
	}
	defer srv.stop()
	log("ready after %s", srv.ready.Round(time.Millisecond))
	r := perfharness.NewRunner(*port, false, viewers)
	consoleOnlyOwner := func(cs []perfharness.Client) []perfharness.Client {
		var out []perfharness.Client
		for _, c := range cs {
			if c.Surface != "console" {
				out = append(out, c)
			}
		}
		return out
	}
	meta := []string{
		fmt.Sprintf("source: %s (copied to %s)", *source, data),
		fmt.Sprintf("server ready %s after exec (cold start)", srv.ready.Round(time.Millisecond)),
		fmt.Sprintf("viewers: %s", viewerNames(viewers)),
	}
	log("cold: owner, every client once")
	r.Sequential(ctx, "cold", viewers[:1], cs)
	if len(viewers) > 1 {
		log("first: other viewers, every client once")
		r.Sequential(ctx, "first", viewers[1:], consoleOnlyOwner(cs))
	}
	log("warm: %d rounds", *rounds)
	for range *rounds {
		r.Sequential(ctx, "warm", viewers[:1], cs)
		if len(viewers) > 1 {
			r.Sequential(ctx, "warm", viewers[1:], consoleOnlyOwner(cs))
		}
	}
	log("concurrent: %d viewers × %d rounds", *conc, *concRounds)
	start := time.Now()
	r.Concurrent(ctx, "concurrent", *conc, *concRounds, consoleOnlyOwner(cs))
	meta = append(meta, fmt.Sprintf("concurrent phase: %d workers × %d rounds in %s", *conc, *concRounds, time.Since(start).Round(time.Millisecond)))
	if *background {
		log("background: warm rounds beside a catalogue writer")
		w, err := perfharness.StartBackgroundWriter(filepath.Join(data, "ether.db"))
		if err != nil {
			return 2, fmt.Errorf("background writer: %w", err)
		}
		for range 2 {
			r.Sequential(ctx, "background", viewers, consoleOnlyOwner(cs))
		}
		meta = append(meta, "busy-writer phase: "+w.Close())
	}
	srv.stop()
	byTag, _, err := perfharness.ReadTrace(trace)
	if err != nil {
		return 2, fmt.Errorf("trace: %w", err)
	}
	rows := perfharness.Summarize(r.Results(), byTag, cs)
	report, err := os.Create(filepath.Join(dir, "report.md"))
	if err != nil {
		return 2, err
	}
	failed := perfharness.WriteReport(report, "Ether performance — real data", meta, rows)
	report.Close()
	if b, err := json.MarshalIndent(r.Results(), "", " "); err == nil {
		_ = os.WriteFile(filepath.Join(dir, "results.json"), b, 0o600)
	}
	auditFile, err := os.Create(filepath.Join(dir, "audit.md"))
	if err != nil {
		return 2, err
	}
	if err := perfharness.Audit(ctx, filepath.Join(data, "ether.db"), byTag, auditFile); err != nil {
		log("audit: %v", err)
	}
	auditFile.Close()
	log("report: %s", filepath.Join(dir, "report.md"))
	log("audit:  %s", filepath.Join(dir, "audit.md"))
	log("%d endpoints over budget or failing", failed)
	if *strict && failed > 0 {
		return 1, nil
	}
	return 0, nil
}

func runAudit(ctx context.Context, args []string) error {
	f := flag.NewFlagSet("audit", flag.ExitOnError)
	trace := f.String("trace", "", "trace.jsonl from a run")
	db := f.String("db", "", "the run's copied ether.db")
	_ = f.Parse(args)
	byTag, _, err := perfharness.ReadTrace(*trace)
	if err != nil {
		return err
	}
	return perfharness.Audit(ctx, *db, byTag, os.Stdout)
}

func viewerNames(vs []perfharness.Viewer) string {
	var names []string
	for _, v := range vs {
		n := v.Username
		if v.Owner {
			n += " (owner)"
		}
		if v.Restricted {
			n += " (restricted)"
		}
		names = append(names, n)
	}
	return strings.Join(names, ", ")
}
