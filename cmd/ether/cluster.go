package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/clusterconfig"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/logredact"
	"github.com/braedonsaunders/ether/internal/store"
)

const clusterUsage = `usage:
  ether cluster init  [--data-dir DIR] [--database-url URL]
      Make this (stopped) standalone server the first node of a cluster:
      copy its SQLite data into an empty PostgreSQL database and share its
      settings from there. URL defaults to $ETHER_DATABASE_URL.
  ether cluster token [--data-dir DIR] [--database-url URL] [--ttl 1h]
      Print a one-time join token for a new node (valid for --ttl, at most
      1h). URL is the database address as the new node will reach it
      (default: this node's).
  ether cluster join  [--data-dir DIR] [--database-url URL] [--force] TOKEN
      Make this data directory a node of the cluster the token belongs to.
      --force moves an already configured server's settings aside.
`

// runCluster handles `ether cluster ...` and returns the exit code.
func runCluster(args []string, defaultDataDir string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(stderr, clusterUsage)
		return 2
	}
	cmd := args[0]
	fs := flag.NewFlagSet("ether cluster "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := fs.String("data-dir", defaultDataDir, "data directory")
	dbURL := fs.String("database-url", "", "PostgreSQL URL (postgres://...)")
	force := fs.Bool("force", false, "join: move an existing configured server aside")
	ttl := fs.Duration("ttl", clusterconfig.DefaultJoinTokenTTL, "token: validity")
	positional := []string{}
	rest := args[1:]
	for {
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		if fs.NArg() == 0 {
			break
		}
		positional = append(positional, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	log := slog.New(logredact.Handler(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))

	var err error
	switch cmd {
	case "init":
		url := *dbURL
		if url == "" {
			url = strings.TrimSpace(os.Getenv(config.DatabaseURLEnv))
		}
		fmt.Fprintln(stdout, "Make sure Ether is stopped on this machine; changes made while copying would be lost.")
		err = clusterconfig.Init(ctx, clusterconfig.InitOptions{DataDir: *dataDir, DatabaseURL: url, Logger: log})
		if err == nil {
			fmt.Fprintf(stdout, "This server is now the first node of a cluster on %s.\nStart Ether, then add nodes with `ether cluster token` here and `ether cluster join` there.\n", store.RedactURL(url))
		}
	case "token":
		err = clusterToken(ctx, stdout, *dataDir, *dbURL, *ttl, log)
	case "join":
		if len(positional) != 1 {
			fmt.Fprint(stderr, clusterUsage)
			return 2
		}
		var res clusterconfig.JoinResult
		res, err = clusterconfig.Join(ctx, clusterconfig.JoinOptions{DataDir: *dataDir, Token: positional[0], DatabaseURL: *dbURL, Force: *force, Logger: log})
		if err == nil {
			for _, p := range res.MovedAside {
				fmt.Fprintf(stdout, "Moved aside: %s\n", p)
			}
			fmt.Fprintf(stdout, "Joined the cluster as server %s. Start Ether on this machine (forward its own port for remote access).\n", res.MachineIdentifier)
		}
	default:
		fmt.Fprint(stderr, clusterUsage)
		return 2
	}
	if err != nil {
		fmt.Fprintln(stderr, "ether cluster "+cmd+":", err)
		return 1
	}
	return 0
}

func clusterToken(ctx context.Context, stdout io.Writer, dataDir, url string, ttl time.Duration, log *slog.Logger) error {
	if ttl <= 0 || ttl > time.Hour {
		return errors.New("--ttl must be between 1s and 1h")
	}
	cfg, err := config.Open(dataDir)
	if err != nil {
		return err
	}
	dbCfg := cfg.Get().Database.Effective(os.Getenv)
	if dbCfg.Driver != config.DatabasePostgres {
		return errors.New("clustering is not enabled on this server (it uses SQLite); run `ether cluster init` first")
	}
	db, _, err := store.OpenWith(ctx, store.Options{Driver: dbCfg.Driver, URL: dbCfg.URL, Logger: log})
	if err != nil {
		return err
	}
	defer db.Close()
	if url == "" {
		url = dbCfg.URL
	}
	tok, info, err := clusterconfig.NewToken(ctx, db, cfg, url, ttl)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%s\n\nOn the new node run:\n  ether cluster join <token>\n\nValid until %s, once.\n%s\n", tok.String(), info.ExpiresAt.Local().Format(time.RFC1123), clusterconfig.TokenWarning)
	return nil
}
