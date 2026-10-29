package main

import (
	"slices"
	"testing"
)

func TestInventoryReportsUncoveredRoutes(t *testing.T) {
	missing := missingRoutes()
	if !slices.Contains(missing, "/:/eventsource/notifications") {
		t.Fatalf("uncovered event stream absent from inventory: %v", missing)
	}
	if slices.Contains(missing, "/identity") {
		t.Fatal("identity is already covered")
	}
}
