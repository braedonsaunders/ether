package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/braedonsaunders/ether/internal/conform"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontends/mediabrowser"
	"github.com/braedonsaunders/ether/internal/frontends/stremio"
	"github.com/braedonsaunders/ether/internal/plex"
)

// routes reports the actual router registrations of available frontends.
func routes(args []string) error {
	f := flag.NewFlagSet("routes", flag.ContinueOnError)
	protocol := f.String("protocol", "", "protocol")
	if err := f.Parse(args); err != nil {
		return err
	}
	switch *protocol {
	case "plex", "jellyfin", "emby", "silo", "stremio":
	default:
		return fmt.Errorf("unknown protocol %q", *protocol)
	}
	if *protocol == "plex" {
		return json.NewEncoder(os.Stdout).Encode(plex.RouteInventory())
	}
	if *protocol == "stremio" {
		registered, err := stremio.RegisteredRoutes()
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(conform.RouteInventory{Available: true, Routes: registered})
	}
	if *protocol == "jellyfin" {
		inv, err := jellyfinRoutes(context.Background())
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(inv)
	}
	return json.NewEncoder(os.Stdout).Encode(conform.RouteInventory{Available: false, Routes: []conform.Route{}})
}

func jellyfinRoutes(ctx context.Context) (conform.RouteInventory, error) {
	registry := frontend.NewRegistry(func(id frontend.Protocol) frontend.Host {
		return frontend.Host{Identity: frontend.BindIdentity(nil, id)}
	})
	if err := registry.Register(mediabrowser.Jellyfin{MachineID: "route-inventory"}); err != nil {
		return conform.RouteInventory{}, err
	}
	instance, err := registry.Mount(ctx, "jellyfin")
	if err != nil {
		return conform.RouteInventory{}, err
	}
	defer registry.Unmount(ctx, "jellyfin")
	source, ok := instance.(frontend.RouteSource)
	if !ok {
		return conform.RouteInventory{Routes: []conform.Route{}}, nil
	}
	return conform.RouteInventory{Available: true, Routes: source.Routes()}, nil
}
