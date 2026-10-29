// Command plexparity builds fixtures and compares a preliminary browsing corpus.
// The complete binary runner, playback corpus and runtime isolation are pending.
package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	switch {
	case len(os.Args) == 3 && os.Args[1] == "seed":
		if err := seed(ctx, os.Args[2]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case len(os.Args) == 3 && os.Args[1] == "tmdb-fixture":
		host, _, err := net.SplitHostPort(os.Args[2])
		if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
			fmt.Fprintln(os.Stderr, "fixture must bind a loopback IP address")
			os.Exit(2)
		}
		if err := http.ListenAndServe(os.Args[2], tmdbFixture()); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case len(os.Args) == 5 && os.Args[1] == "run":
		if err := runPair(ctx, os.Args[2], os.Args[3], os.Args[4], os.Stdout); err != nil {
			// Keep report and final status on one stream to avoid Docker multiplex interleaving.
			fmt.Fprintln(os.Stdout, err)
			os.Exit(1)
		}
	case len(os.Args) == 4 && os.Args[1] == "compare":
		fmt.Fprintln(os.Stderr, "INCOMPLETE browsing-only comparison: not a Plex parity verdict")
		n, err := compareCorpus(ctx, os.Args[2], os.Args[3], os.Stdout)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if n > 0 {
			fmt.Fprintf(os.Stderr, "%d response differences\n", n)
			os.Exit(1)
		}
		// An incomplete corpus must not report success to automation.
		fmt.Fprintln(os.Stderr, "comparison incomplete; parity is not established")
		os.Exit(2)
	default:
		fmt.Fprintln(os.Stderr, "usage: plexparity seed EMPTY-DIRECTORY | tmdb-fixture LOOPBACK:PORT | compare BASELINE-URL CANDIDATE-URL | run BASELINE-BINARY CANDIDATE-BINARY SEED-DIRECTORY")
		os.Exit(2)
	}
}
