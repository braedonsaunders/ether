package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/braedonsaunders/ether/internal/conform"
)

// routes will be wired to the front-end registry as protocol handlers land.
func routes(args []string) error {
	f := flag.NewFlagSet("routes", flag.ContinueOnError)
	protocol := f.String("protocol", "", "protocol")
	if err := f.Parse(args); err != nil {
		return err
	}
	switch *protocol {
	case "plex", "jellyfin", "emby", "silo":
	default:
		return fmt.Errorf("unknown protocol %q", *protocol)
	}
	return json.NewEncoder(os.Stdout).Encode(conform.RouteInventory{Available: false, Routes: []conform.Route{}})
}
