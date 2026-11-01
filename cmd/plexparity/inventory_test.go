package main

import (
	"slices"
	"testing"
)

func TestInventoryReportsUncoveredRoutes(t *testing.T) {
	missing := missingRoutes()
	if len(missing) != 0 {
		t.Fatalf("uncovered Plex GET routes: %v", missing)
	}
	if slices.Contains(missing, "/identity") {
		t.Fatal("identity is already covered")
	}
}
