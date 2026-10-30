// Command ether runs the Ether media server: a TMDB catalogue with
// on-demand streaming from owner-configured Usenet and debrid sources.
//
//	ether                         run (in the foreground, or under the OS service manager)
//	ether --spike movie.mp4 ...   run the spike: serve local files as a library
//	ether service install|uninstall|start|stop|restart|status [--data-dir DIR]
//	ether cluster init|token|join ...  (see cluster.go)
//	ether version
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"

	"github.com/braedonsaunders/ether/internal/app"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/logfile"
	"github.com/braedonsaunders/ether/internal/logredact"
	"github.com/braedonsaunders/ether/internal/notify"
	"github.com/braedonsaunders/ether/internal/service"
	"github.com/braedonsaunders/ether/internal/updater"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "notification-monitor" {
		if notify.RunMonitor(os.Stdin) != nil {
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Printf("ether %s (%s, %s)\n", updater.Version, updater.Commit, updater.Date)
		return
	}

	fs := flag.NewFlagSet("ether", flag.ExitOnError)
	dataDir := fs.String("data-dir", config.DefaultDataDir(), "data directory (config, database, cache)")
	spike := fs.Bool("spike", false, "spike mode: serve the given local video files as a library")
	logLevel := fs.String("log-level", "", "debug, info, warn or error (default from config)")

	_ = fs.Parse(os.Args[1:])
	if fs.Arg(0) == "cluster" {
		os.Exit(runCluster(fs.Args()[1:], *dataDir, os.Stdout, os.Stderr))
	}
	if fs.Arg(0) == "service" {
		// `ether [flags] service <cmd> [flags]`: flags may follow the
		// subcommand, e.g. `ether service install --data-dir X`.
		svcArgs := append([]string{"service"}, parseInterleaved(fs, fs.Args()[1:])...)
		dir := *dataDir
		if abs, err := filepath.Abs(dir); err == nil {
			dir = abs
		}
		// The data dir is baked into the registration as ETHER_DATA_DIR.
		// Re-installing over an existing registration (an upgrade) keeps
		// the registered one unless a data dir was asked for explicitly.
		svc, err := service.New(nil, service.Options{
			DataDir:              dir,
			KeepInstalledDataDir: !dataDirExplicit(fs, os.Getenv),
		})
		if err != nil {
			fatal(err)
		}
		if _, err := service.HandleCommand(svcArgs, svc, os.Stdout); err != nil {
			fatal(err)
		}
		return
	}

	logger := setupLogging(*dataDir, logLevelFor(*logLevel, *dataDir))
	run := func(ctx context.Context) error {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		return app.Serve(ctx, app.Options{
			DataDir: *dataDir, Logger: logger, Spike: *spike, SpikeFiles: fs.Args(),
			NotificationExecutable: exe,
		})
	}

	svc, err := service.New(run, service.Options{DataDir: *dataDir, Logger: logger})
	if err != nil {
		logger.Error("ether: service setup failed", "err", err)
		os.Exit(1)
	}
	if err := svc.Run(); err != nil {
		if errors.Is(err, service.ErrRestartRequested) {
			logger.Info("ether: restart requested", "exit_code", service.ExitCodeRestart)
			os.Exit(service.ExitCodeRestart)
		}
		logger.Error("ether exited with error", "err", err)
		os.Exit(1)
	}
}

// logLevelFor resolves the log level: the flag, else the config file,
// else info. The config is only read (config.Read): opening it would
// create, re-seal and rewrite config.json before the app's update startup
// check, which must run before anything a bad release could break.
func logLevelFor(flagLevel, dataDir string) slog.Level {
	level := slog.LevelInfo
	if flagLevel != "" {
		_ = level.UnmarshalText([]byte(strings.ToUpper(flagLevel)))
	} else if cfg, err := config.Read(dataDir); err == nil {
		_ = level.UnmarshalText([]byte(strings.ToUpper(cfg.Server.LogLevel)))
	}
	return level
}

// setupLogging tees slog to stderr and <dataDir>/logs/ether.log (rotated),
// installs it as the default logger, and points fatal runtime traces
// (unrecovered panics, "fatal error:" crashes) at <dataDir>/logs/crash.log.
// Under a Windows service stderr goes nowhere and launchd never rotates
// it, so the files are the durable record. Failures degrade to stderr.
func setupLogging(dataDir string, level slog.Level) *slog.Logger {
	logDir := filepath.Join(dataDir, "logs")
	var out io.Writer = os.Stderr
	lw, fileErr := logfile.Open(filepath.Join(logDir, "ether.log"), logfile.DefaultMaxSize, logfile.DefaultKeep)
	if fileErr == nil {
		out = logfile.Tee(os.Stderr, lw)
	}
	// Credentials are scrubbed before anything reaches stderr or the file.
	logger := slog.New(logredact.Handler(slog.NewTextHandler(out, &slog.HandlerOptions{Level: level})))
	slog.SetDefault(logger)
	if fileErr != nil {
		logger.Warn("file logging disabled", "err", fileErr)
	}
	cl, err := logfile.PrepareCrashLog(logDir, logfile.DefaultCrashMaxSize)
	if err != nil {
		logger.Warn("crash log unavailable", "err", err)
		return logger
	}
	defer cl.File.Close() // SetCrashOutput keeps its own duplicate
	if err := debug.SetCrashOutput(cl.File, debug.CrashOptions{}); err != nil {
		logger.Warn("crash log unavailable", "err", err)
	}
	if cl.Changed {
		logger.Warn("ether crashed during a previous run; see the crash log", "path", cl.Path)
	}
	return logger
}

// parseInterleaved parses flags that may appear anywhere in args and
// returns the positional arguments in order.
func parseInterleaved(fs *flag.FlagSet, args []string) []string {
	var positional []string
	for {
		_ = fs.Parse(args)
		rest := fs.Args()
		if len(rest) == 0 {
			return positional
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

// dataDirExplicit reports whether the data directory was chosen on purpose
// (--data-dir or ETHER_DATA_DIR) rather than defaulted.
func dataDirExplicit(fs *flag.FlagSet, getenv func(string) string) bool {
	explicit := getenv("ETHER_DATA_DIR") != ""
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "data-dir" {
			explicit = true
		}
	})
	return explicit
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "ether:", err)
	os.Exit(1)
}
