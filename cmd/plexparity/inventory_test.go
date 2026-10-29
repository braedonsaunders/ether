package main

import (
	"slices"
	"testing"
)

func TestInventoryReportsUncoveredRoutes(t *testing.T) {
	missing := missingRoutes()
	if !slices.Contains(missing, "/livetv/sessions/{session}/{consumer}/relay/{segment}") {
		t.Fatalf("uncovered IPTV relay absent from inventory: %v", missing)
	}
	if slices.Contains(missing, "/identity") {
		t.Fatal("identity is already covered")
	}
}
