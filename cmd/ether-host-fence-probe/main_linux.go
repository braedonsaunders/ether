//go:build linux

// ether-host-fence-probe is a finite, non-signing observer feasibility tool.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/braedonsaunders/ether/internal/hostfenceprobe"
)

func main() {
	proc := flag.String("proc-root", "/host/proc", "read-only host procfs")
	cgroup := flag.String("cgroup-root", "/host/cgroup", "read-only host cgroup v2 mount")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "unexpected arguments")
		os.Exit(2)
	}
	b, err := hostfenceprobe.ReadBinding(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	report, err := hostfenceprobe.Probe(*proc, *cgroup, b)
	if json.NewEncoder(os.Stdout).Encode(report) != nil {
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
