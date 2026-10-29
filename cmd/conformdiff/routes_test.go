package main

import (
	"context"
	"slices"
	"testing"

	"github.com/braedonsaunders/ether/internal/conform"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontends/mediabrowser"
)

func TestJellyfinRoutesUseMountedRouter(t *testing.T) {
	ctx := context.Background()
	got, err := jellyfinRoutes(ctx)
	if err != nil || !got.Available {
		t.Fatal(got, err)
	}
	instance, err := (mediabrowser.Jellyfin{MachineID: "fixture"}).Mount(ctx, frontend.Host{Identity: frontend.BindIdentity(nil, "jellyfin")})
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Close(ctx)
	router, ok := instance.(frontend.RouteSource)
	if !ok {
		t.Fatal("missing route source")
	}
	if !slices.Equal(got.Routes, router.Routes()) {
		t.Fatal("inventory differs from mounted router")
	}
	if !slices.Contains(got.Routes, conform.Route{Method: "GET", Pattern: "/System/Info/Public"}) {
		t.Fatal("public route missing")
	}
}
