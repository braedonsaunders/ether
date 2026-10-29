// Command plexparity builds fixtures and compares a preliminary browsing corpus.
// The complete binary runner, playback corpus and runtime isolation are pending.
package main

import (
	"context"
	"fmt"
	"os"
)

func main() {
	ctx := context.Background()
	switch {
	case len(os.Args) == 3 && os.Args[1] == "seed":
		if err := seed(ctx, os.Args[2]); err != nil {
			fmt.Fprintln(os.Stderr, err)
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
		fmt.Fprintln(os.Stderr, "usage: plexparity seed EMPTY-DIRECTORY | compare BASELINE-URL CANDIDATE-URL")
		os.Exit(2)
	}
}
